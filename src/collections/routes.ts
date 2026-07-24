import { Hono } from 'hono'
import { isDeepStrictEqual } from 'node:util'
import { sql } from 'kysely'
import { uuidv7 } from 'uuidv7'
import type { AuthContext } from '../auth/middleware.ts'
import type { CollectionContents } from '../collection-contents/index.ts'
import { db } from '../db/connection.ts'

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

export function createCollectionsRoutes(
  contents: CollectionContents,
): Hono<{ Variables: AuthContext }> {
  const collectionsRoutes = new Hono<{ Variables: AuthContext }>()

  collectionsRoutes.post('/', async (c) => {
    const userId = c.var.user.id
    // One vault per account: claim it, or return the one already there.
    // Enforced here rather than by a UNIQUE(user_id) constraint — see
    // DESIGN.md §One vault per account for why, and for what lifting it takes.
    const claimed = await db.transaction().execute(async (trx) => {
      // Serialize concurrent creates for this account on its user row, so the
      // check below cannot race another request's insert.
      await trx
        .selectFrom('users')
        .where('id', '=', userId)
        .select('id')
        .forUpdate()
        .executeTakeFirstOrThrow()
      const existing = await trx
        .selectFrom('collections')
        .where('user_id', '=', userId)
        .select(['id', 'user_id', 'current_version', 'created_at'])
        // The earliest is the one the client's connect() picks. Same ordering as
        // the list route below, tiebreak included, so the two never disagree
        // about which vault is first.
        .orderBy('created_at', 'asc')
        .orderBy('id', 'asc')
        .executeTakeFirst()
      if (existing) return { created: false, collection: existing } as const
      const collection = await trx
        .insertInto('collections')
        .values({ id: uuidv7(), user_id: userId })
        .returning(['id', 'user_id', 'current_version', 'created_at'])
        .executeTakeFirstOrThrow()
      return { created: true, collection } as const
    })
    return c.json({ collection: claimed.collection }, claimed.created ? 201 : 200)
  })

  collectionsRoutes.get('/', async (c) => {
    const userId = c.var.user.id
    const rows = await db
      .selectFrom('collections')
      .where('user_id', '=', userId)
      .select(['id', 'user_id', 'current_version', 'created_at'])
      .orderBy('created_at', 'asc')
      .orderBy('id', 'asc')
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
      // A first claim names no revision, so it never asked to replace anything.
      // Losing that race is not a conflict: return the authoritative material so
      // the caller adopts it (one vault, one key). Only guarded rotations below
      // can conflict.
      if (body.previous_key_updated_at === undefined) return { kind: 'ok', key } as const
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
    const result = await contents.deleteCollection({
      userId: c.var.user.id,
      collectionId: c.req.param('id'),
    })
    if (result.kind === 'not_found') return c.json({ error: 'not found' }, 404)
    return c.body(null, 204)
  })

  return collectionsRoutes
}
