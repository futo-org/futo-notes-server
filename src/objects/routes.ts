import { Hono } from 'hono'
import { sql } from 'kysely'
import { uuidv7 } from 'uuidv7'
import type { AuthContext } from '../auth/middleware.ts'
import { db } from '../db/connection.ts'

export const objectsRoutes = new Hono<{ Variables: AuthContext }>()

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

interface CreateBody {
  blob_key?: string
  size_bytes?: number
}

interface UpdateBody {
  version?: number
  blob_key?: string
  size_bytes?: number
}

function isNonNegativeSafeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

function isBlobKeyOwnedByUser(blobKey: unknown, userId: string): blobKey is string {
  if (typeof blobKey !== 'string') return false
  const [keyUserId, blobId, extra] = blobKey.split('/')
  return extra === undefined && keyUserId === userId && UUID_RE.test(blobId)
}

async function ensureCollectionOwned(
  userId: string,
  collectionId: string,
): Promise<boolean> {
  const row = await db
    .selectFrom('collections')
    .where('id', '=', collectionId)
    .where('user_id', '=', userId)
    .select('id')
    .executeTakeFirst()
  return !!row
}

// Pull sync + list.
objectsRoutes.get('/:cid/objects', async (c) => {
  const userId = c.var.user.id
  const cid = c.req.param('cid')
  if (!(await ensureCollectionOwned(userId, cid))) {
    return c.json({ error: 'not found' }, 404)
  }
  const sinceVersion = Number(c.req.query('sinceVersion') ?? 0)
  if (!isNonNegativeSafeInteger(sinceVersion)) {
    return c.json({ error: 'invalid sinceVersion' }, 400)
  }

  const rows = await db
    .selectFrom('objects')
    .where('collection_id', '=', cid)
    .where('user_id', '=', userId)
    .where('version', '>', String(sinceVersion))
    .select([
      'id',
      'collection_id',
      'version',
      'deleted',
      'blob_key',
      'size_bytes',
      'created_at',
      'updated_at',
    ])
    .orderBy('version', 'asc')
    .execute()

  return c.json({ objects: rows })
})

objectsRoutes.get('/:cid/objects/:oid', async (c) => {
  const userId = c.var.user.id
  const cid = c.req.param('cid')
  const oid = c.req.param('oid')
  const row = await db
    .selectFrom('objects')
    .where('id', '=', oid)
    .where('collection_id', '=', cid)
    .where('user_id', '=', userId)
    .select([
      'id',
      'collection_id',
      'version',
      'deleted',
      'blob_key',
      'size_bytes',
      'created_at',
      'updated_at',
    ])
    .executeTakeFirst()
  if (!row) return c.json({ error: 'not found' }, 404)
  return c.json({ object: row })
})

objectsRoutes.post('/:cid/objects', async (c) => {
  const userId = c.var.user.id
  const cid = c.req.param('cid')
  if (!(await ensureCollectionOwned(userId, cid))) {
    return c.json({ error: 'not found' }, 404)
  }

  let body: CreateBody
  try {
    body = await c.req.json()
  } catch {
    return c.json({ error: 'invalid json' }, 400)
  }
  const blob_key = body.blob_key
  const size_bytes = body.size_bytes
  if (!isBlobKeyOwnedByUser(blob_key, userId) || !isNonNegativeSafeInteger(size_bytes)) {
    return c.json({ error: 'valid blob_key and size_bytes required' }, 400)
  }

  const row = await db
    .insertInto('objects')
    .values({
      id: uuidv7(),
      collection_id: cid,
      user_id: userId,
      blob_key,
      size_bytes: String(size_bytes),
    })
    .returning([
      'id',
      'collection_id',
      'version',
      'deleted',
      'blob_key',
      'size_bytes',
      'created_at',
      'updated_at',
    ])
    .executeTakeFirstOrThrow()

  return c.json({ object: row }, 201)
})

// Atomic version-check update. On stale version, returns 409 with current state.
objectsRoutes.put('/:cid/objects/:oid', async (c) => {
  const userId = c.var.user.id
  const cid = c.req.param('cid')
  const oid = c.req.param('oid')

  let body: UpdateBody
  try {
    body = await c.req.json()
  } catch {
    return c.json({ error: 'invalid json' }, 400)
  }
  const version = body.version
  const blob_key = body.blob_key
  const size_bytes = body.size_bytes
  if (
    typeof version !== 'number' ||
    !Number.isSafeInteger(version) ||
    version < 1 ||
    !isBlobKeyOwnedByUser(blob_key, userId) ||
    !isNonNegativeSafeInteger(size_bytes)
  ) {
    return c.json({ error: 'valid version, blob_key, size_bytes required' }, 400)
  }

  return await db.transaction().execute(async (trx) => {
    const updated = await trx
      .updateTable('objects')
      .set({
        version: String(version),
        blob_key,
        size_bytes: String(size_bytes),
        updated_at: sql`now()`,
      })
      .where('id', '=', oid)
      .where('collection_id', '=', cid)
      .where('user_id', '=', userId)
      .where('version', '=', String(version - 1))
      .returning([
        'id',
        'collection_id',
        'version',
        'deleted',
        'blob_key',
        'size_bytes',
        'created_at',
        'updated_at',
      ])
      .executeTakeFirst()

    if (updated) {
      return c.json({ object: updated })
    }

    // Zero rows updated. Either the object doesn't exist (for this user)
    // or the version is stale. Distinguish, and surface 409 on stale.
    const current = await trx
      .selectFrom('objects')
      .where('id', '=', oid)
      .where('collection_id', '=', cid)
      .where('user_id', '=', userId)
      .select(['version', 'blob_key'])
      .executeTakeFirst()

    if (!current) {
      return c.json({ error: 'not found' }, 404)
    }

    return c.json(
      {
        error: 'version conflict',
        currentVersion: Number(current.version),
        currentBlobKey: current.blob_key,
      },
      409,
    )
  })
})

// Soft delete: set deleted=true, bump version so peers see the tombstone.
objectsRoutes.delete('/:cid/objects/:oid', async (c) => {
  const userId = c.var.user.id
  const cid = c.req.param('cid')
  const oid = c.req.param('oid')
  const row = await db
    .updateTable('objects')
    .set({
      deleted: true,
      version: sql`version + 1`,
      updated_at: sql`now()`,
    })
    .where('id', '=', oid)
    .where('collection_id', '=', cid)
    .where('user_id', '=', userId)
    .returning(['id', 'version', 'deleted'])
    .executeTakeFirst()
  if (!row) return c.json({ error: 'not found' }, 404)
  return c.json({ object: row })
})
