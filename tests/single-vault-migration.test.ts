import assert from 'node:assert/strict'
import { afterAll, beforeAll, test } from 'bun:test'
import { type Kysely } from 'kysely'
import { db, waitForDb } from '../src/db/connection.ts'
import { migrateToLatest } from '../src/db/migrate.ts'
import * as m008 from '../src/db/migrations/008_single_collection_per_user.ts'

// The migration up/down take `Kysely<unknown>` (as Kysely's Migrator passes
// them); our typed `db` singleton needs a cast to call them directly.
const rawDb = db as unknown as Kysely<unknown>

beforeAll(async () => {
  await waitForDb()
  await migrateToLatest()
})

afterAll(async () => {
  await db.destroy()
})

/**
 * Migration 008 heals an already-split account: it keeps the EARLIEST vault per
 * user (the one the client's connect() picks) and deletes the rest, whose
 * objects cascade away. This drives the real migration code — roll back to the
 * pre-008 schema, seed a split, then re-apply.
 */
test('migration 008 collapses duplicate vaults to the earliest, keeping its objects', async () => {
  // A user to own the seeded split. Insert directly so we don't depend on the
  // (now idempotent) collections route.
  const userId = crypto.randomUUID()
  await db
    .insertInto('users')
    .values({
      id: userId,
      sub: `dev:collapse-${userId}@example.test`,
      name: 'Collapse Test',
      email: `collapse-${userId}@example.test`,
    })
    .execute()

  const earliestId = crypto.randomUUID()
  const loserId = crypto.randomUUID()
  const earliestObj = crypto.randomUUID()
  const loserObj = crypto.randomUUID()
  const earliestBlob = `${userId}/${crypto.randomUUID()}`
  const loserBlob = `${userId}/${crypto.randomUUID()}`

  // Roll back the unique constraint so a split (two vaults / one user) can exist.
  await m008.down(rawDb)
  let upDone = false
  try {
    await db
      .insertInto('collections')
      .values([
        { id: earliestId, user_id: userId, created_at: new Date('2020-01-01T00:00:00Z') },
        { id: loserId, user_id: userId, created_at: new Date('2020-06-01T00:00:00Z') },
      ])
      .execute()
    await db
      .insertInto('objects')
      .values([
        { id: earliestObj, collection_id: earliestId, user_id: userId, blob_key: earliestBlob, size_bytes: '11' },
        { id: loserObj, collection_id: loserId, user_id: userId, blob_key: loserBlob, size_bytes: '22' },
      ])
      .execute()

    // The migration under test: collapse + re-add UNIQUE(user_id).
    await m008.up(rawDb)
    upDone = true
  } finally {
    if (!upDone) await m008.up(rawDb)
  }

  // Only the earliest vault survives.
  const cols = await db
    .selectFrom('collections')
    .where('user_id', '=', userId)
    .select(['id'])
    .execute()
  assert.deepEqual(cols.map((c) => c.id), [earliestId])

  // Its object is intact; the loser's cascaded away with its vault.
  const objs = await db
    .selectFrom('objects')
    .where('user_id', '=', userId)
    .select(['id', 'collection_id'])
    .execute()
  assert.deepEqual(objs, [{ id: earliestObj, collection_id: earliestId }])

  // The loser vault's blob is recorded as orphaned (so the GC can reclaim the
  // file); the survivor's blob is NOT orphaned (its object is still live).
  const orphans = await db
    .selectFrom('orphaned_blobs')
    .where('user_id', '=', userId)
    .select(['blob_key'])
    .execute()
  const orphanKeys = orphans.map((o) => o.blob_key)
  assert.ok(orphanKeys.includes(loserBlob), 'loser blob must be recorded as orphaned')
  assert.ok(!orphanKeys.includes(earliestBlob), 'survivor blob must NOT be orphaned')

  // The unique constraint is back, so a second vault can never be created.
  await assert.rejects(
    db.insertInto('collections').values({ id: crypto.randomUUID(), user_id: userId }).execute(),
  )
})
