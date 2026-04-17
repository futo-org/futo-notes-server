import { Hono } from 'hono'
import { sql, type Transaction } from 'kysely'
import { uuidv7 } from 'uuidv7'
import type { AuthContext } from '../auth/middleware.ts'
import type { BlobStore } from '../blob/interface.ts'
import { db } from '../db/connection.ts'
import type { Database } from '../db/types.ts'

export function createObjectsRoutes(
  store: BlobStore,
): Hono<{ Variables: AuthContext }> {
  const objectsRoutes = new Hono<{ Variables: AuthContext }>()

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

async function nextCollectionVersion(
  trx: Transaction<Database>,
  userId: string,
  collectionId: string,
): Promise<string> {
  const row = await trx
    .updateTable('collections')
    .set({ current_version: sql`current_version + 1` })
    .where('id', '=', collectionId)
    .where('user_id', '=', userId)
    .returning('current_version')
    .executeTakeFirstOrThrow()
  return row.current_version
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
    .where('change_seq', '>', String(sinceVersion))
    .select([
      'id',
      'collection_id',
      'version',
      'change_seq',
      'deleted',
      'blob_key',
      'size_bytes',
      'created_at',
      'updated_at',
    ])
    .orderBy('change_seq', 'asc')
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
      'change_seq',
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

  return await db.transaction().execute(async (trx) => {
    const changeSeq = await nextCollectionVersion(trx, userId, cid)
    const row = await trx
      .insertInto('objects')
      .values({
        id: uuidv7(),
        collection_id: cid,
        user_id: userId,
        blob_key,
        size_bytes: String(size_bytes),
        change_seq: changeSeq,
      })
      .returning([
        'id',
        'collection_id',
        'version',
        'change_seq',
        'deleted',
        'blob_key',
        'size_bytes',
        'created_at',
        'updated_at',
      ])
      .executeTakeFirstOrThrow()

    return c.json({ object: row, collectionVersion: Number(changeSeq) }, 201)
  })
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

    if (Number(current.version) !== version - 1) {
      return c.json(
        {
          error: 'version conflict',
          currentVersion: Number(current.version),
          currentBlobKey: current.blob_key,
        },
        409,
      )
    }

    const changeSeq = await nextCollectionVersion(trx, userId, cid)
    const updated = await trx
      .updateTable('objects')
      .set({
        version: String(version),
        change_seq: changeSeq,
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
        'change_seq',
        'deleted',
        'blob_key',
        'size_bytes',
        'created_at',
        'updated_at',
      ])
      .executeTakeFirst()

    if (updated) {
      return c.json({ object: updated, collectionVersion: Number(changeSeq) })
    }

    // A concurrent writer won between the read above and the guarded update.
    const latest = await trx
      .selectFrom('objects')
      .where('id', '=', oid)
      .where('collection_id', '=', cid)
      .where('user_id', '=', userId)
      .select(['version', 'blob_key'])
      .executeTakeFirst()

    if (!latest) {
      return c.json({ error: 'not found' }, 404)
    }

    return c.json(
      {
        error: 'version conflict',
        currentVersion: Number(latest.version),
        currentBlobKey: latest.blob_key,
      },
      409,
    )
  })
})

// Soft delete: set deleted=true, bump version so peers see the tombstone.
// Optional ?version=N query rejects the delete with 409 if the caller's
// expected version is stale — preventing a delete from clobbering a
// newer edit from a peer (delete-vs-edit race; edit wins).
objectsRoutes.delete('/:cid/objects/:oid', async (c) => {
  const userId = c.var.user.id
  const cid = c.req.param('cid')
  const oid = c.req.param('oid')

  const expectedRaw = c.req.query('version')
  let expectedVersion: number | null = null
  if (expectedRaw !== undefined) {
    const parsed = Number(expectedRaw)
    if (!Number.isSafeInteger(parsed) || parsed < 1) {
      return c.json({ error: 'invalid version' }, 400)
    }
    expectedVersion = parsed
  }

  const result = await db
    .transaction()
    .execute(async (trx) => {
      const existing = await trx
        .selectFrom('objects')
        .where('id', '=', oid)
        .where('collection_id', '=', cid)
        .where('user_id', '=', userId)
        .select(['id', 'version', 'blob_key', 'deleted'])
        .executeTakeFirst()
      if (!existing) return { kind: 'missing' as const }

      if (
        expectedVersion !== null
        && !existing.deleted
        && Number(existing.version) !== expectedVersion
      ) {
        return {
          kind: 'conflict' as const,
          currentVersion: Number(existing.version),
          currentBlobKey: existing.blob_key,
        }
      }

      const changeSeq = await nextCollectionVersion(trx, userId, cid)
      const updated = await trx
        .updateTable('objects')
        .set({
          deleted: true,
          version: sql`version + 1`,
          change_seq: changeSeq,
          updated_at: sql`now()`,
        })
        .where('id', '=', oid)
        .where('collection_id', '=', cid)
        .where('user_id', '=', userId)
        .returning(['id', 'version', 'change_seq', 'deleted'])
        .executeTakeFirst()
      if (!updated) return { kind: 'missing' as const }
      return { kind: 'ok' as const, row: updated }
    })

  if (result.kind === 'missing') return c.json({ error: 'not found' }, 404)
  if (result.kind === 'conflict') {
    return c.json(
      {
        error: 'version conflict',
        currentVersion: result.currentVersion,
        currentBlobKey: result.currentBlobKey,
      },
      409,
    )
  }
  return c.json({ object: result.row, collectionVersion: Number(result.row.change_seq) })
})

  // Single-round-trip create: body is raw ciphertext. Server mints the
  // blob key, writes the blob, then inserts the object row. Halves the
  // RTT cost of the original POST /blobs + POST /objects pair, which
  // matters a lot on first-sync of a large collection over high-latency
  // networks. Semantically equivalent to those two calls — same response
  // shape, same invariants.
  objectsRoutes.post('/:cid/blob-objects', async (c) => {
    const userId = c.var.user.id
    const cid = c.req.param('cid')
    if (!(await ensureCollectionOwned(userId, cid))) {
      return c.json({ error: 'not found' }, 404)
    }

    const body = await c.req.arrayBuffer()
    if (body.byteLength === 0) {
      return c.json({ error: 'empty body' }, 400)
    }

    const blobKey = `${userId}/${uuidv7()}`
    await store.put(blobKey, new Uint8Array(body))

    return await db.transaction().execute(async (trx) => {
      const changeSeq = await nextCollectionVersion(trx, userId, cid)
      const row = await trx
        .insertInto('objects')
        .values({
          id: uuidv7(),
          collection_id: cid,
          user_id: userId,
          blob_key: blobKey,
          size_bytes: String(body.byteLength),
          change_seq: changeSeq,
        })
        .returning([
          'id',
          'collection_id',
          'version',
          'change_seq',
          'deleted',
          'blob_key',
          'size_bytes',
          'created_at',
          'updated_at',
        ])
        .executeTakeFirstOrThrow()

      return c.json({ object: row, collectionVersion: Number(changeSeq) }, 201)
    })
  })

  // Single-round-trip update: body is raw ciphertext, ?version=N is the
  // expected next version. Mirrors PUT /objects/:oid's version-check +
  // conflict semantics exactly so the client's merge path is unchanged.
  objectsRoutes.put('/:cid/blob-objects/:oid', async (c) => {
    const userId = c.var.user.id
    const cid = c.req.param('cid')
    const oid = c.req.param('oid')

    const versionRaw = c.req.query('version')
    const version = Number(versionRaw)
    if (!Number.isSafeInteger(version) || version < 1) {
      return c.json({ error: 'valid ?version query required' }, 400)
    }

    const body = await c.req.arrayBuffer()
    if (body.byteLength === 0) {
      return c.json({ error: 'empty body' }, 400)
    }

    const blobKey = `${userId}/${uuidv7()}`
    await store.put(blobKey, new Uint8Array(body))
    const sizeBytes = body.byteLength

    return await db.transaction().execute(async (trx) => {
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

      if (Number(current.version) !== version - 1) {
        return c.json(
          {
            error: 'version conflict',
            currentVersion: Number(current.version),
            currentBlobKey: current.blob_key,
          },
          409,
        )
      }

      const changeSeq = await nextCollectionVersion(trx, userId, cid)
      const updated = await trx
        .updateTable('objects')
        .set({
          version: String(version),
          change_seq: changeSeq,
          blob_key: blobKey,
          size_bytes: String(sizeBytes),
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
          'change_seq',
          'deleted',
          'blob_key',
          'size_bytes',
          'created_at',
          'updated_at',
        ])
        .executeTakeFirst()

      if (updated) {
        return c.json({ object: updated, collectionVersion: Number(changeSeq) })
      }

      const latest = await trx
        .selectFrom('objects')
        .where('id', '=', oid)
        .where('collection_id', '=', cid)
        .where('user_id', '=', userId)
        .select(['version', 'blob_key'])
        .executeTakeFirst()

      if (!latest) {
        return c.json({ error: 'not found' }, 404)
      }

      return c.json(
        {
          error: 'version conflict',
          currentVersion: Number(latest.version),
          currentBlobKey: latest.blob_key,
        },
        409,
      )
    })
  })

  return objectsRoutes
}
