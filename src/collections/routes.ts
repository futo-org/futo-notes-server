import { Hono } from 'hono'
import { isDeepStrictEqual } from 'node:util'
import { sql } from 'kysely'
import { uuidv7 } from 'uuidv7'
import type { AuthContext } from '../auth/middleware.ts'
import { db } from '../db/connection.ts'

export const collectionsRoutes = new Hono<{ Variables: AuthContext }>()

interface KeyMaterialRequest {
  key_salt: string
  key_kdf: Record<string, unknown>
  encrypted_vault_key: string
  previous_key_updated_at?: string
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object' && !Array.isArray(value)
}

function isKeyMaterialBody(value: unknown): value is KeyMaterialRequest {
  if (!isRecord(value)) return false
  return (
    typeof value.key_salt === 'string' &&
    value.key_salt.length > 0 &&
    isRecord(value.key_kdf) &&
    typeof value.encrypted_vault_key === 'string' &&
    value.encrypted_vault_key.length > 0 &&
    (value.previous_key_updated_at === undefined ||
      (typeof value.previous_key_updated_at === 'string' && value.previous_key_updated_at.length > 0))
  )
}

collectionsRoutes.post('/', async (c) => {
  const userId = c.var.user.id
  // One vault per account. Claim the single collection, or return the existing
  // one — idempotent. The `UNIQUE(user_id)` constraint (migration 008) makes
  // concurrent creates converge on one row instead of forking into two vaults.
  const created = await db
    .insertInto('collections')
    .values({ id: uuidv7(), user_id: userId })
    .onConflict((oc) => oc.column('user_id').doNothing())
    .returning(['id', 'user_id', 'current_version', 'created_at'])
    .executeTakeFirst()
  if (created) return c.json({ collection: created }, 201)
  const existing = await db
    .selectFrom('collections')
    .where('user_id', '=', userId)
    .select(['id', 'user_id', 'current_version', 'created_at'])
    .executeTakeFirstOrThrow()
  return c.json({ collection: existing }, 200)
})

collectionsRoutes.get('/', async (c) => {
  const userId = c.var.user.id
  const rows = await db
    .selectFrom('collections')
    .where('user_id', '=', userId)
    .select(['id', 'user_id', 'current_version', 'created_at'])
    .orderBy('created_at', 'asc')
    .execute()
  return c.json({ collections: rows })
})

collectionsRoutes.get('/:id/key', async (c) => {
  const userId = c.var.user.id
  const id = c.req.param('id')
  const row = await db
    .selectFrom('collections')
    .where('id', '=', id)
    .where('user_id', '=', userId)
    .select(['key_salt', 'key_kdf', 'encrypted_vault_key', 'key_updated_at'])
    .executeTakeFirst()
  if (!row) return c.json({ error: 'not found' }, 404)
  if (!row.key_salt || !row.key_kdf || !row.encrypted_vault_key) {
    return c.json({ key: null })
  }
  return c.json({
    key: {
      key_salt: row.key_salt,
      key_kdf: row.key_kdf,
      encrypted_vault_key: row.encrypted_vault_key,
      key_updated_at: row.key_updated_at,
    },
  })
})

collectionsRoutes.put('/:id/key', async (c) => {
  const userId = c.var.user.id
  const id = c.req.param('id')
  let body: unknown
  try {
    body = await c.req.json()
  } catch {
    return c.json({ error: 'invalid json' }, 400)
  }
  if (!isKeyMaterialBody(body)) {
    return c.json({ error: 'valid key_salt, key_kdf, and encrypted_vault_key required' }, 400)
  }

  const outcome = await db.transaction().execute(async (trx) => {
    // Serialize claims and rotations on this collection. This closes the gap
    // between reading a revision token and applying its guarded replacement.
    const current = await trx
      .selectFrom('collections')
      .where('id', '=', id)
      .where('user_id', '=', userId)
      .select(['key_salt', 'key_kdf', 'encrypted_vault_key', 'key_updated_at'])
      .forUpdate()
      .executeTakeFirst()
    if (!current) return { kind: 'not-found' } as const

    const save = async (rotation: boolean) => {
      const row = await trx
        .updateTable('collections')
        .set({
          key_salt: body.key_salt,
          key_kdf: body.key_kdf,
          encrypted_vault_key: body.encrypted_vault_key,
          // Keep the serialized timestamp usable as an opaque revision token,
          // even when two requests land in the same millisecond.
          key_updated_at: rotation
            ? sql`greatest(clock_timestamp(), key_updated_at + interval '1 millisecond')`
            : sql`clock_timestamp()`,
        })
        .where('id', '=', id)
        .where('user_id', '=', userId)
        .returning(['key_salt', 'key_kdf', 'encrypted_vault_key', 'key_updated_at'])
        .executeTakeFirstOrThrow()
      return { kind: 'ok', key: row } as const
    }

    if (!current.key_salt || !current.key_kdf || !current.encrypted_vault_key || !current.key_updated_at) {
      return save(false)
    }
    const key = {
      key_salt: current.key_salt,
      key_kdf: current.key_kdf,
      encrypted_vault_key: current.encrypted_vault_key,
      key_updated_at: current.key_updated_at,
    }
    const idempotent = body.key_salt === current.key_salt &&
      isDeepStrictEqual(body.key_kdf, current.key_kdf) &&
      body.encrypted_vault_key === current.encrypted_vault_key
    if (idempotent) return { kind: 'ok', key } as const
    if (body.previous_key_updated_at === current.key_updated_at.toISOString()) {
      return save(true)
    }
    return { kind: 'conflict', key } as const
  })

  if (outcome.kind === 'not-found') {
    return c.json({ error: 'not found' }, 404)
  }
  if (outcome.kind === 'conflict') {
    return c.json({ error: 'key conflict', currentKey: outcome.key }, 409)
  }
  return c.json({ key: outcome.key })
})

collectionsRoutes.get('/:id', async (c) => {
  const userId = c.var.user.id
  const id = c.req.param('id')
  const row = await db
    .selectFrom('collections')
    .where('id', '=', id)
    .where('user_id', '=', userId)
    .select(['id', 'user_id', 'current_version', 'created_at'])
    .executeTakeFirst()
  if (!row) return c.json({ error: 'not found' }, 404)
  return c.json({ collection: row })
})

collectionsRoutes.delete('/:id', async (c) => {
  const userId = c.var.user.id
  const id = c.req.param('id')
  const deleted = await db.transaction().execute(async (trx) => {
    // Record every blob the collection's objects still reference as orphaned
    // BEFORE the object rows cascade away — otherwise the blobKeys are lost and
    // the blob GC (src/maintenance/blobGc.ts) can never reclaim them.
    await trx
      .insertInto('orphaned_blobs')
      .expression((eb) =>
        eb
          .selectFrom('objects')
          .where('collection_id', '=', id)
          .where('user_id', '=', userId)
          .where('blob_key', 'is not', null)
          .select((sb) => [
            'blob_key',
            'user_id',
            sb.fn.coalesce('size_bytes', sql<string>`0`).as('size_bytes'),
          ]),
      )
      .onConflict((oc) => oc.column('blob_key').doNothing())
      .execute()

    return await trx
      .deleteFrom('collections')
      .where('id', '=', id)
      .where('user_id', '=', userId)
      .returning('id')
      .executeTakeFirst()
  })
  if (!deleted) return c.json({ error: 'not found' }, 404)
  // Objects cascade via FK. Orphaned blobs are GC'd later (see DESIGN.md).
  return c.body(null, 204)
})
