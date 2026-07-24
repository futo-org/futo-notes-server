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

test('repeating a soft delete returns the existing tombstone without another change', async () => {
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
  assert.deepEqual(retried, deleted)
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
  assert.deepEqual(await contents.runMaintenance(), {
    reconciled: 0,
    stagedDeleted: 0,
    retainedDeleted: 0,
    purgeableDeleted: 0,
    mutationResultsDeleted: 0,
  })
  assert.ok(await store.get(staged.blobKey))

  currentTime = new Date('2026-07-25T12:00:00.000Z')
  assert.deepEqual(await contents.runMaintenance(), {
    reconciled: 0,
    stagedDeleted: 1,
    retainedDeleted: 0,
    purgeableDeleted: 0,
    mutationResultsDeleted: 0,
  })
  assert.equal(await store.get(staged.blobKey), null)
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

test('direct deletion removes only staged blobs', async () => {
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
    { kind: 'missing' },
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
  assert.equal(maintenance.purgeableDeleted, 2)
  assert.equal(await store.get(firstBlob.blobKey), null)
  assert.equal(await store.get(secondBlob.blobKey), null)
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
  assert.equal((await contents.runMaintenance()).retainedDeleted, 0)
  assert.ok(await store.get(firstBlob.blobKey))

  currentTime = new Date('2027-07-24T12:00:00.000Z')
  assert.ok((await contents.runMaintenance()).retainedDeleted >= 1)
  assert.equal(await store.get(firstBlob.blobKey), null)
  assert.ok(await store.get(secondBlob.blobKey))
})

test('a Mutation ID result expires after the fixed 30-day window', async () => {
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

  currentTime = new Date('2026-08-23T11:59:59.000Z')
  assert.equal((await contents.runMaintenance()).mutationResultsDeleted, 0)
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
  assert.ok((await contents.runMaintenance()).mutationResultsDeleted >= 1)
  assert.deepEqual(
    await contents.mutateObject({
      kind: 'create',
      userId,
      collectionId,
      stagedBlobKey: staged.blobKey,
      mutationId,
    }),
    { kind: 'blob_not_staged' },
  )
})

test('maintenance reconciles an untracked stored blob as freshly staged', async () => {
  const { userId, collectionId } = await createUserAndCollection()
  currentTime = new Date('2026-07-24T12:00:00.000Z')
  const blobKey = `${userId}/${uuidv7()}`
  await store.put(blobKey, new TextEncoder().encode('historical-untracked-upload'))

  const maintenance = await contents.runMaintenance()
  assert.equal(maintenance.reconciled, 1)
  assert.equal(maintenance.stagedDeleted, 0)

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
