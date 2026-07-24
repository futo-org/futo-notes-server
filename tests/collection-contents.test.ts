import assert from 'node:assert/strict'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { afterAll, beforeAll, test } from 'bun:test'
import { uuidv7 } from 'uuidv7'
import { FsBlobStore } from '../src/blob/fs.ts'
import type { BlobStore } from '../src/blob/interface.ts'
import { CollectionContents } from '../src/collection-contents/index.ts'
import { db, waitForDb } from '../src/db/connection.ts'
import { migrateToLatest } from '../src/db/migrate.ts'
import { runBlobMaintenanceOnce } from '../src/maintenance/blobMaintenance.ts'
import { notifier } from '../src/sync/notifier.ts'

let blobDir: string
let contents: CollectionContents
let store: FsBlobStore
let currentTime = new Date('2026-07-24T12:00:00.000Z')
const seededUserIds: string[] = []

beforeAll(async () => {
  await waitForDb()
  await migrateToLatest()
  blobDir = await mkdtemp(path.join(tmpdir(), 'futo-notes-collection-contents-'))
  store = new FsBlobStore(blobDir)
  contents = new CollectionContents({
    db,
    store,
    notifier,
    now: () => currentTime,
  })
})

afterAll(async () => {
  if (seededUserIds.length > 0) {
    await db.deleteFrom('users').where('id', 'in', seededUserIds).execute()
  }
  await db.destroy()
  await rm(blobDir, { recursive: true, force: true })
})

async function createUserAndCollection(): Promise<{ userId: string; collectionId: string }> {
  const userId = uuidv7()
  const collectionId = uuidv7()
  seededUserIds.push(userId)
  await db
    .insertInto('users')
    .values({
      id: userId,
      sub: `dev:collection-contents-${userId}@example.test`,
      name: 'Collection Contents Test',
      email: `collection-contents-${userId}@example.test`,
    })
    .execute()
  await db
    .insertInto('collections')
    .values({ id: collectionId, user_id: userId })
    .execute()
  return { userId, collectionId }
}

test('a staged blob can be claimed by exactly one object mutation', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  const staged = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('opaque-ciphertext'),
  })

  const created = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: staged.blobKey,
  })
  assert.equal(created.kind, 'ok')
  if (created.kind !== 'ok') return
  assert.equal(created.object.blob_key, staged.blobKey)
  assert.equal(created.object.size_bytes, String('opaque-ciphertext'.length))
  assert.equal(created.object.version, '1')
  assert.equal(created.collectionVersion, 1)

  const reused = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: staged.blobKey,
  })
  assert.deepEqual(reused, { kind: 'blob_not_staged' })
})

test('a version conflict leaves the blob staged and does not advance the collection cursor', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  const firstBlob = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('version-one'),
  })
  const created = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: firstBlob.blobKey,
  })
  assert.equal(created.kind, 'ok')
  if (created.kind !== 'ok') return

  const secondBlob = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('version-two'),
  })
  const updated = await contents.mutateObject({
    kind: 'update',
    userId,
    collectionId,
    objectId: created.object.id,
    version: 2,
    stagedBlobKey: secondBlob.blobKey,
  })
  assert.equal(updated.kind, 'ok')
  if (updated.kind !== 'ok') return
  assert.equal(updated.object.version, '2')
  assert.equal(updated.collectionVersion, 2)

  const retryBlob = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('version-three'),
  })
  const conflict = await contents.mutateObject({
    kind: 'update',
    userId,
    collectionId,
    objectId: created.object.id,
    version: 2,
    stagedBlobKey: retryBlob.blobKey,
  })
  assert.deepEqual(conflict, {
    kind: 'conflict',
    currentVersion: 2,
    currentBlobKey: secondBlob.blobKey,
  })

  const retried = await contents.mutateObject({
    kind: 'update',
    userId,
    collectionId,
    objectId: created.object.id,
    version: 3,
    stagedBlobKey: retryBlob.blobKey,
  })
  assert.equal(retried.kind, 'ok')
  if (retried.kind !== 'ok') return
  assert.equal(retried.object.version, '3')
  assert.equal(retried.collectionVersion, 3)
})

test('a new soft-delete request advances an existing tombstone again', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  const staged = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('delete-me'),
  })
  const created = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: staged.blobKey,
  })
  assert.equal(created.kind, 'ok')
  if (created.kind !== 'ok') return

  const deleted = await contents.mutateObject({
    kind: 'delete',
    userId,
    collectionId,
    objectId: created.object.id,
    expectedVersion: 1,
  })
  assert.equal(deleted.kind, 'ok')
  if (deleted.kind !== 'ok') return
  assert.equal(deleted.object.deleted, true)
  assert.equal(deleted.object.version, '2')
  assert.equal(deleted.collectionVersion, 2)

  const retried = await contents.mutateObject({
    kind: 'delete',
    userId,
    collectionId,
    objectId: created.object.id,
    expectedVersion: 1,
  })
  assert.equal(retried.kind, 'ok')
  if (retried.kind !== 'ok') return
  assert.equal(retried.object.deleted, true)
  assert.equal(retried.object.version, '3')
  assert.equal(retried.object.change_seq, '3')
  assert.equal(retried.object.blob_key, deleted.object.blob_key)
  assert.equal(retried.collectionVersion, 3)
})

test('a Mutation ID returns its original result and cannot identify a different intent', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  const staged = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('idempotent-create'),
  })
  const mutationId = uuidv7()
  const created = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: staged.blobKey,
    mutationId,
  })
  assert.equal(created.kind, 'ok')

  const replayed = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: staged.blobKey,
    mutationId,
  })
  assert.deepEqual(replayed, created)

  const other = await createUserAndCollection()
  const mismatch = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId: other.collectionId,
    stagedBlobKey: staged.blobKey,
    mutationId,
  })
  assert.deepEqual(mismatch, { kind: 'mutation_mismatch' })
})

test('Mutation IDs replay updates and deletes without another collection change', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  const firstBlob = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('mutation-v1'),
  })
  const created = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: firstBlob.blobKey,
  })
  assert.equal(created.kind, 'ok')
  if (created.kind !== 'ok') return

  const secondBlob = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('mutation-v2'),
  })
  const updateMutationId = uuidv7()
  const updated = await contents.mutateObject({
    kind: 'update',
    userId,
    collectionId,
    objectId: created.object.id,
    version: 2,
    stagedBlobKey: secondBlob.blobKey,
    mutationId: updateMutationId,
  })
  assert.equal(updated.kind, 'ok')
  assert.deepEqual(await contents.mutateObject({
    kind: 'update',
    userId,
    collectionId,
    objectId: created.object.id,
    version: 2,
    stagedBlobKey: secondBlob.blobKey,
    mutationId: updateMutationId,
  }), updated)
  assert.deepEqual(await contents.mutateObject({
    kind: 'update',
    userId,
    collectionId,
    objectId: created.object.id,
    version: 3,
    stagedBlobKey: secondBlob.blobKey,
    mutationId: updateMutationId,
  }), { kind: 'mutation_mismatch' })

  const deleteMutationId = uuidv7()
  const deleted = await contents.mutateObject({
    kind: 'delete',
    userId,
    collectionId,
    objectId: created.object.id,
    expectedVersion: 2,
    mutationId: deleteMutationId,
  })
  assert.equal(deleted.kind, 'ok')
  assert.deepEqual(await contents.mutateObject({
    kind: 'delete',
    userId,
    collectionId,
    objectId: created.object.id,
    expectedVersion: 2,
    mutationId: deleteMutationId,
  }), deleted)

  const collection = await db
    .selectFrom('collections')
    .where('id', '=', collectionId)
    .where('user_id', '=', userId)
    .select('current_version')
    .executeTakeFirstOrThrow()
  assert.equal(collection.current_version, '3')
})

test('an unclaimed staged blob is removed after the fixed 24-hour window', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  const staged = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('abandoned-upload'),
  })

  currentTime = new Date('2026-07-25T11:59:59.000Z')
  await contents.runMaintenance()
  assert.ok(await store.get(staged.blobKey))
  assert.ok(
    await db
      .selectFrom('blob_ledger')
      .where('blob_key', '=', staged.blobKey)
      .where('user_id', '=', userId)
      .where('state', '=', 'staged')
      .select('blob_key')
      .executeTakeFirst(),
  )

  currentTime = new Date('2026-07-25T12:00:00.000Z')
  const maintenance = await contents.runMaintenance()
  assert.ok(maintenance.stagedDeleted >= 1)
  assert.equal(await store.get(staged.blobKey), null)
  assert.equal(
    await db
      .selectFrom('blob_ledger')
      .where('blob_key', '=', staged.blobKey)
      .where('user_id', '=', userId)
      .select('blob_key')
      .executeTakeFirst(),
    undefined,
  )
  assert.deepEqual(
    await contents.mutateObject({
      kind: 'create',
      userId,
      collectionId,
      stagedBlobKey: staged.blobKey,
    }),
    { kind: 'blob_not_staged' },
  )
})

test('direct deletion removes staged or untracked blobs, but not in-use blobs', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  const unclaimed = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('unclaimed'),
  })
  assert.deepEqual(
    await contents.deleteStagedBlob({ userId, blobKey: unclaimed.blobKey }),
    { kind: 'deleted' },
  )
  assert.equal(await store.get(unclaimed.blobKey), null)
  assert.deepEqual(
    await contents.deleteStagedBlob({ userId, blobKey: unclaimed.blobKey }),
    { kind: 'deleted' },
  )

  const claimed = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('claimed'),
  })
  const created = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: claimed.blobKey,
  })
  assert.equal(created.kind, 'ok')
  assert.deepEqual(
    await contents.deleteStagedBlob({ userId, blobKey: claimed.blobKey }),
    { kind: 'in_use' },
  )
  assert.ok(await store.get(claimed.blobKey))

  // A file written before the authoritative ledger existed has no row yet.
  // DELETE must still remove its bytes, and reconciliation must not resurrect it.
  const untrackedKey = `${userId}/${uuidv7()}`
  await store.put(untrackedKey, new TextEncoder().encode('pre-ledger'))
  const reconciler = new CollectionContents({
    db,
    store,
    notifier,
    now: () => currentTime,
  })
  const deleteDuringReconciliation: BlobStore = {
    put: (key, data) => store.put(key, data),
    get: (key) => store.get(key),
    list: (prefix) => store.list(prefix),
    delete: async (key) => {
      // Force reconciliation into the deletion window. A deletion marker must
      // keep it from adopting the file just before its bytes disappear.
      await reconciler.reconcileStorage()
      await store.delete(key)
    },
  }
  const deletingContents = new CollectionContents({
    db,
    store: deleteDuringReconciliation,
    notifier,
    now: () => currentTime,
  })

  assert.deepEqual(
    await deletingContents.deleteStagedBlob({ userId, blobKey: untrackedKey }),
    { kind: 'deleted' },
  )
  assert.equal(await store.get(untrackedKey), null)
  await reconciler.reconcileStorage()
  assert.equal(
    await db
      .selectFrom('blob_ledger')
      .where('blob_key', '=', untrackedKey)
      .select('blob_key')
      .executeTakeFirst(),
    undefined,
  )
})

test('deleting a collection makes its claimed and retained blobs immediately purgeable', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  const firstBlob = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('first-version'),
  })
  const created = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: firstBlob.blobKey,
  })
  assert.equal(created.kind, 'ok')
  if (created.kind !== 'ok') return
  const secondBlob = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('second-version'),
  })
  const updated = await contents.mutateObject({
    kind: 'update',
    userId,
    collectionId,
    objectId: created.object.id,
    version: 2,
    stagedBlobKey: secondBlob.blobKey,
  })
  assert.equal(updated.kind, 'ok')

  assert.deepEqual(
    await contents.deleteCollection({ userId, collectionId }),
    { kind: 'deleted' },
  )
  assert.deepEqual(
    await contents.deleteCollection({ userId, collectionId }),
    { kind: 'not_found' },
  )

  const maintenance = await contents.runMaintenance()
  assert.ok(maintenance.purgeableDeleted >= 2)
  assert.equal(await store.get(firstBlob.blobKey), null)
  assert.equal(await store.get(secondBlob.blobKey), null)
  assert.deepEqual(
    await db
      .selectFrom('blob_ledger')
      .where('user_id', '=', userId)
      .where('blob_key', 'in', [firstBlob.blobKey, secondBlob.blobKey])
      .select('blob_key')
      .execute(),
    [],
  )
})

test('a retained merge ancestor remains available for one year', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  const firstBlob = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('merge-ancestor'),
  })
  const created = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: firstBlob.blobKey,
  })
  assert.equal(created.kind, 'ok')
  if (created.kind !== 'ok') return
  const secondBlob = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('current-version'),
  })
  assert.equal((await contents.mutateObject({
    kind: 'update',
    userId,
    collectionId,
    objectId: created.object.id,
    version: 2,
    stagedBlobKey: secondBlob.blobKey,
  })).kind, 'ok')

  currentTime = new Date('2027-07-24T11:59:59.000Z')
  await contents.runMaintenance()
  assert.ok(await store.get(firstBlob.blobKey))
  assert.ok(
    await db
      .selectFrom('blob_ledger')
      .where('blob_key', '=', firstBlob.blobKey)
      .where('user_id', '=', userId)
      .where('state', '=', 'retained')
      .select('blob_key')
      .executeTakeFirst(),
  )

  currentTime = new Date('2027-07-24T12:00:00.000Z')
  const maintenance = await contents.runMaintenance()
  assert.ok(maintenance.retainedDeleted >= 1)
  assert.equal(await store.get(firstBlob.blobKey), null)
  assert.equal(
    await db
      .selectFrom('blob_ledger')
      .where('blob_key', '=', firstBlob.blobKey)
      .where('user_id', '=', userId)
      .select('blob_key')
      .executeTakeFirst(),
    undefined,
  )
  assert.ok(await store.get(secondBlob.blobKey))
})

test('a Mutation ID can identify a new intent at the fixed 30-day boundary', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  const staged = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('thirty-day-retry'),
  })
  const mutationId = uuidv7()
  const created = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: staged.blobKey,
    mutationId,
  })
  assert.equal(created.kind, 'ok')
  if (created.kind !== 'ok') return

  currentTime = new Date('2026-08-23T11:59:59.000Z')
  await contents.runMaintenance()
  assert.ok(
    await db
      .selectFrom('mutation_results')
      .where('user_id', '=', userId)
      .where('mutation_id', '=', mutationId)
      .select('mutation_id')
      .executeTakeFirst(),
  )
  assert.deepEqual(
    await contents.mutateObject({
      kind: 'create',
      userId,
      collectionId,
      stagedBlobKey: staged.blobKey,
      mutationId,
    }),
    created,
  )

  currentTime = new Date('2026-08-23T12:00:00.000Z')
  // Expiry is part of Mutation-ID semantics, not a side effect of whether the
  // maintenance loop happened to run at the boundary. A different intent must
  // be free to reuse the expired identifier instead of seeing a stale mismatch.
  const reused = await contents.mutateObject({
    kind: 'delete',
    userId,
    collectionId,
    objectId: created.object.id,
    expectedVersion: 1,
    mutationId,
  })
  assert.equal(reused.kind, 'ok')
  if (reused.kind !== 'ok') return
  assert.equal(reused.object.deleted, true)
  assert.equal(reused.object.version, '2')
  assert.equal(reused.collectionVersion, 2)
})

test('maintenance reconciles an untracked stored blob as freshly staged', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  const blobKey = `${userId}/${uuidv7()}`
  await store.put(blobKey, new TextEncoder().encode('historical-untracked-upload'))

  const maintenance = await contents.runMaintenance()
  assert.ok(maintenance.reconciled >= 1)
  const ledgerRow = await db
    .selectFrom('blob_ledger')
    .where('blob_key', '=', blobKey)
    .where('user_id', '=', userId)
    .select('state')
    .executeTakeFirst()
  assert.equal(ledgerRow?.state, 'staged')
  assert.ok(await store.get(blobKey))

  const claimed = await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: blobKey,
  })
  assert.equal(claimed.kind, 'ok')
  if (claimed.kind !== 'ok') return
  assert.equal(claimed.object.blob_key, blobKey)
})

test('one maintenance cycle drains more purgeable blobs than a single batch', async () => {
  const { userId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  // Deliberately over the 500-row maintenance batch size: a sweep that trimmed
  // one batch per cycle would need days to reclaim a large deleted collection.
  const total = 600
  const blobKeys: string[] = []
  for (let index = 0; index < total; index += 1) {
    const blobKey = `${userId}/${uuidv7()}`
    await store.put(blobKey, new TextEncoder().encode(`purgeable-${index}`))
    blobKeys.push(blobKey)
  }
  await db
    .insertInto('blob_ledger')
    .values(blobKeys.map((blobKey) => ({
      blob_key: blobKey,
      user_id: userId,
      size_bytes: '1',
      state: 'purgeable' as const,
      collection_id: null,
      object_id: null,
      object_version: null,
      created_at: currentTime,
      state_changed_at: currentTime,
    })))
    .execute()

  const maintenance = await contents.runMaintenance()
  assert.ok(maintenance.purgeableDeleted >= total)
  assert.deepEqual(
    await db
      .selectFrom('blob_ledger')
      .where('user_id', '=', userId)
      .where('state', '=', 'purgeable')
      .select('blob_key')
      .execute(),
    [],
  )
  const firstKey = blobKeys.at(0)
  const lastKey = blobKeys.at(-1)
  assert.ok(firstKey && lastKey)
  assert.equal(await store.get(firstKey), null)
  assert.equal(await store.get(lastKey), null)
})

test('maintenance retains its ledger entry and retries after a storage deletion failure', async () => {
  const { userId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  const staged = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('retry-delete'),
  })
  let failNextDelete = true
  const failOnceStore: BlobStore = {
    put: (key, data) => store.put(key, data),
    get: (key) => store.get(key),
    list: (prefix) => store.list(prefix),
    delete: async (key) => {
      if (key === staged.blobKey && failNextDelete) {
        failNextDelete = false
        throw new Error('injected delete failure')
      }
      await store.delete(key)
    },
  }
  const retryingContents = new CollectionContents({
    db,
    store: failOnceStore,
    notifier,
    now: () => currentTime,
  })

  currentTime = new Date('2026-07-25T12:00:00.000Z')
  await retryingContents.runMaintenance()
  assert.ok(await store.get(staged.blobKey))
  assert.ok(await db
    .selectFrom('blob_ledger')
    .where('blob_key', '=', staged.blobKey)
    .select('blob_key')
    .executeTakeFirst())

  await retryingContents.runMaintenance()
  assert.equal(await store.get(staged.blobKey), null)
  assert.equal(await db
    .selectFrom('blob_ledger')
    .where('blob_key', '=', staged.blobKey)
    .select('blob_key')
    .executeTakeFirst(), undefined)
})

test('a staged blob stops being claimable after 24 hours even when cleanup has not run', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  const insideWindow = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('inside-window'),
  })
  const outsideWindow = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('outside-window'),
  })

  currentTime = new Date('2026-07-25T11:59:59.000Z')
  assert.equal((await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: insideWindow.blobKey,
  })).kind, 'ok')

  // The window is enforced when claiming, not by whenever the sweeper happens to
  // run: no maintenance has executed here, so the row and its bytes both still
  // exist and the claim is refused anyway.
  currentTime = new Date('2026-07-25T12:00:00.000Z')
  assert.deepEqual(await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: outsideWindow.blobKey,
  }), { kind: 'blob_not_staged' })
  assert.ok(await store.get(outsideWindow.blobKey))
  assert.ok(await db
    .selectFrom('blob_ledger')
    .where('blob_key', '=', outsideWindow.blobKey)
    .where('user_id', '=', userId)
    .where('state', '=', 'staged')
    .select('blob_key')
    .executeTakeFirst())
})

test('staging survives a reconciliation pass landing mid-write', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  const reconciler = new CollectionContents({
    db,
    store,
    notifier,
    now: () => currentTime,
  })
  // Reconciliation used to be able to adopt the key in the gap between the
  // storage write and the ledger insert, so the insert failed on the primary
  // key and a valid upload became a 500.
  const racingStore: BlobStore = {
    put: async (key, data) => {
      await store.put(key, data)
      await reconciler.reconcileStorage()
    },
    get: (key) => store.get(key),
    delete: (key) => store.delete(key),
    list: (prefix) => store.list(prefix),
  }
  const racing = new CollectionContents({
    db,
    store: racingStore,
    notifier,
    now: () => currentTime,
  })

  const staged = await racing.stageBlob({
    userId,
    data: new TextEncoder().encode('racy-ciphertext'),
  })
  const row = await db
    .selectFrom('blob_ledger')
    .where('blob_key', '=', staged.blobKey)
    .where('user_id', '=', userId)
    .select(['state', 'size_bytes'])
    .executeTakeFirstOrThrow()
  assert.equal(row.state, 'staged')
  assert.equal(row.size_bytes, String('racy-ciphertext'.length))

  // Bookkeeping survived intact, so the blob is still claimable exactly once.
  assert.equal((await racing.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: staged.blobKey,
  })).kind, 'ok')
})

test('reconciliation still runs when garbage collection is disabled', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  // An untracked file, as left behind by an upload predating the ledger.
  const untracked = `${userId}/${uuidv7()}`
  await store.put(untracked, new TextEncoder().encode('pre-ledger-upload'))
  // Plus a staged blob that destructive cleanup would be eligible to remove.
  const expiring = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('would-expire'),
  })
  const mutationId = uuidv7()
  const mutationBlob = await contents.stageBlob({
    userId,
    data: new TextEncoder().encode('expired-mutation-result'),
  })
  assert.equal((await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: mutationBlob.blobKey,
    mutationId,
  })).kind, 'ok')

  currentTime = new Date('2026-08-24T12:00:00.000Z')
  const result = await runBlobMaintenanceOnce(contents, { collectGarbage: false })
  assert.ok(result.reconciled >= 1)
  assert.equal(result.stagedDeleted, 0)
  assert.equal(result.retainedDeleted, 0)
  assert.equal(result.purgeableDeleted, 0)
  assert.ok(result.mutationResultsDeleted >= 1)
  assert.ok(await store.get(expiring.blobKey))
  assert.equal(
    await db
      .selectFrom('mutation_results')
      .where('user_id', '=', userId)
      .where('mutation_id', '=', mutationId)
      .select('mutation_id')
      .executeTakeFirst(),
    undefined,
  )

  // The pre-ledger blob is tracked now, so the legacy two-call API can still
  // claim it inside the fresh staging window reconciliation gave it.
  assert.equal((await contents.mutateObject({
    kind: 'create',
    userId,
    collectionId,
    stagedBlobKey: untracked,
  })).kind, 'ok')
})
