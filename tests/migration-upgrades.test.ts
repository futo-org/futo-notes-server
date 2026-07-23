import assert from 'node:assert/strict'
import { afterAll, afterEach, beforeAll, test } from 'bun:test'
import { sql } from 'kysely'
import { uuidv7 } from 'uuidv7'
import { db, waitForDb } from '../src/db/connection.ts'
import { migrateToLatest } from '../src/db/migrate.ts'

const migration008 = '008_single_collection_per_user'
const migration009 = '009_restore_plural_collections'
const seededUserIds: string[] = []

beforeAll(async () => {
  await waitForDb()
  await migrateToLatest()
})

afterAll(async () => {
  await db.destroy()
})

afterEach(async () => {
  if (seededUserIds.length > 0) {
    await db.deleteFrom('users').where('id', 'in', seededUserIds).execute()
    seededUserIds.length = 0
  }
  await migrateToLatest()
})

async function restorePre008MigrationState(): Promise<void> {
  await sql`alter table collections drop constraint if exists collections_user_id_unique`.execute(db)
  await sql`create index if not exists idx_collections_user on collections (user_id)`.execute(db)
  await sql`
    delete from kysely_migration
    where name in (${migration008}, ${migration009})
  `.execute(db)
}

async function restoreAlreadyApplied008State(): Promise<void> {
  await sql`drop index if exists idx_collections_user`.execute(db)
  await sql`
    do $$
    begin
      if not exists (
        select 1
        from pg_constraint
        where conname = 'collections_user_id_unique'
          and conrelid = 'collections'::regclass
      ) then
        alter table collections
          add constraint collections_user_id_unique unique (user_id);
      end if;
    end
    $$
  `.execute(db)
  await sql`delete from kysely_migration where name = ${migration009}`.execute(db)
}

test('stable upgrade preserves every collection and its opaque sync metadata', async () => {
  await restorePre008MigrationState()

  const userId = uuidv7()
  seededUserIds.push(userId)
  const firstCollectionId = uuidv7()
  const secondCollectionId = uuidv7()
  const firstObjectId = uuidv7()
  const secondObjectId = uuidv7()
  const firstBlobKey = `${userId}/${uuidv7()}`
  const secondBlobKey = `${userId}/${uuidv7()}`
  const existingOrphanKey = `${userId}/${uuidv7()}`

  await db
    .insertInto('users')
    .values({
      id: userId,
      sub: `dev:migration-${userId}@example.test`,
      name: 'Migration Test',
      email: `migration-${userId}@example.test`,
    })
    .execute()
  await db
    .insertInto('collections')
    .values([
      {
        id: firstCollectionId,
        user_id: userId,
        current_version: '17',
        key_salt: 'first-salt',
        key_kdf: { algorithm: 'argon2id', iterations: 3 },
        encrypted_vault_key: 'first-encrypted-key',
        key_updated_at: new Date('2025-01-01T00:00:00.000Z'),
        created_at: new Date('2024-01-01T00:00:00.000Z'),
      },
      {
        id: secondCollectionId,
        user_id: userId,
        current_version: '29',
        key_salt: 'second-salt',
        key_kdf: { algorithm: 'scrypt', cost: 32768 },
        encrypted_vault_key: 'second-encrypted-key',
        key_updated_at: new Date('2025-02-01T00:00:00.000Z'),
        created_at: new Date('2024-02-01T00:00:00.000Z'),
      },
    ])
    .execute()
  await db
    .insertInto('objects')
    .values([
      {
        id: firstObjectId,
        collection_id: firstCollectionId,
        user_id: userId,
        version: '5',
        change_seq: '17',
        deleted: false,
        blob_key: firstBlobKey,
        size_bytes: '111',
        created_at: new Date('2024-03-01T00:00:00.000Z'),
        updated_at: new Date('2025-03-01T00:00:00.000Z'),
      },
      {
        id: secondObjectId,
        collection_id: secondCollectionId,
        user_id: userId,
        version: '8',
        change_seq: '29',
        deleted: true,
        blob_key: secondBlobKey,
        size_bytes: '222',
        created_at: new Date('2024-04-01T00:00:00.000Z'),
        updated_at: new Date('2025-04-01T00:00:00.000Z'),
      },
    ])
    .execute()
  await db
    .insertInto('orphaned_blobs')
    .values({
      blob_key: existingOrphanKey,
      user_id: userId,
      size_bytes: '333',
      orphaned_at: new Date('2025-05-01T00:00:00.000Z'),
    })
    .execute()

  await migrateToLatest()

  const collections = await db
    .selectFrom('collections')
    .where('user_id', '=', userId)
    .select([
      'id',
      'user_id',
      'current_version',
      'key_salt',
      'key_kdf',
      'encrypted_vault_key',
      'key_updated_at',
      'created_at',
    ])
    .orderBy('created_at')
    .execute()
  assert.deepEqual(collections, [
    {
      id: firstCollectionId,
      user_id: userId,
      current_version: '17',
      key_salt: 'first-salt',
      key_kdf: { algorithm: 'argon2id', iterations: 3 },
      encrypted_vault_key: 'first-encrypted-key',
      key_updated_at: new Date('2025-01-01T00:00:00.000Z'),
      created_at: new Date('2024-01-01T00:00:00.000Z'),
    },
    {
      id: secondCollectionId,
      user_id: userId,
      current_version: '29',
      key_salt: 'second-salt',
      key_kdf: { algorithm: 'scrypt', cost: 32768 },
      encrypted_vault_key: 'second-encrypted-key',
      key_updated_at: new Date('2025-02-01T00:00:00.000Z'),
      created_at: new Date('2024-02-01T00:00:00.000Z'),
    },
  ])

  const objects = await db
    .selectFrom('objects')
    .where('user_id', '=', userId)
    .select([
      'id',
      'collection_id',
      'user_id',
      'version',
      'change_seq',
      'deleted',
      'blob_key',
      'size_bytes',
      'created_at',
      'updated_at',
    ])
    .orderBy('created_at')
    .execute()
  assert.deepEqual(objects, [
    {
      id: firstObjectId,
      collection_id: firstCollectionId,
      user_id: userId,
      version: '5',
      change_seq: '17',
      deleted: false,
      blob_key: firstBlobKey,
      size_bytes: '111',
      created_at: new Date('2024-03-01T00:00:00.000Z'),
      updated_at: new Date('2025-03-01T00:00:00.000Z'),
    },
    {
      id: secondObjectId,
      collection_id: secondCollectionId,
      user_id: userId,
      version: '8',
      change_seq: '29',
      deleted: true,
      blob_key: secondBlobKey,
      size_bytes: '222',
      created_at: new Date('2024-04-01T00:00:00.000Z'),
      updated_at: new Date('2025-04-01T00:00:00.000Z'),
    },
  ])

  const orphans = await db
    .selectFrom('orphaned_blobs')
    .where('user_id', '=', userId)
    .select(['blob_key', 'user_id', 'size_bytes', 'orphaned_at'])
    .execute()
  assert.deepEqual(orphans, [
    {
      blob_key: existingOrphanKey,
      user_id: userId,
      size_bytes: '333',
      orphaned_at: new Date('2025-05-01T00:00:00.000Z'),
    },
  ])

})

test('upgrade repairs databases that already recorded destructive migration 008', async () => {
  await restoreAlreadyApplied008State()

  const userId = uuidv7()
  seededUserIds.push(userId)
  const existingCollectionId = uuidv7()
  const liveObjectId = uuidv7()
  const tombstoneObjectId = uuidv7()
  const liveBlobKey = `${userId}/${uuidv7()}`
  const tombstoneBlobKey = `${userId}/${uuidv7()}`
  const orphanBlobKey = `${userId}/${uuidv7()}`
  await db
    .insertInto('users')
    .values({
      id: userId,
      sub: `dev:already-008-${userId}@example.test`,
      name: 'Already 008 Test',
      email: `already-008-${userId}@example.test`,
    })
    .execute()
  await db
    .insertInto('collections')
    .values({
      id: existingCollectionId,
      user_id: userId,
      current_version: '12',
      key_salt: 'already-008-salt',
      key_kdf: { algorithm: 'opaque-kdf', work: 42 },
      encrypted_vault_key: 'already-008-encrypted-key',
      key_updated_at: new Date('2025-06-01T00:00:00.000Z'),
      created_at: new Date('2024-06-01T00:00:00.000Z'),
    })
    .execute()
  await db
    .insertInto('objects')
    .values([
      {
        id: liveObjectId,
        collection_id: existingCollectionId,
        user_id: userId,
        version: '3',
        change_seq: '9',
        deleted: false,
        blob_key: liveBlobKey,
        size_bytes: '444',
        created_at: new Date('2024-07-01T00:00:00.000Z'),
        updated_at: new Date('2025-07-01T00:00:00.000Z'),
      },
      {
        id: tombstoneObjectId,
        collection_id: existingCollectionId,
        user_id: userId,
        version: '5',
        change_seq: '12',
        deleted: true,
        blob_key: tombstoneBlobKey,
        size_bytes: '555',
        created_at: new Date('2024-08-01T00:00:00.000Z'),
        updated_at: new Date('2025-08-01T00:00:00.000Z'),
      },
    ])
    .execute()
  await db
    .insertInto('orphaned_blobs')
    .values({
      blob_key: orphanBlobKey,
      user_id: userId,
      size_bytes: '666',
      orphaned_at: new Date('2025-09-01T00:00:00.000Z'),
    })
    .execute()

  await migrateToLatest()

  const existingCollection = await db
    .selectFrom('collections')
    .where('id', '=', existingCollectionId)
    .where('user_id', '=', userId)
    .select([
      'id',
      'user_id',
      'current_version',
      'key_salt',
      'key_kdf',
      'encrypted_vault_key',
      'key_updated_at',
      'created_at',
    ])
    .executeTakeFirstOrThrow()
  assert.deepEqual(existingCollection, {
    id: existingCollectionId,
    user_id: userId,
    current_version: '12',
    key_salt: 'already-008-salt',
    key_kdf: { algorithm: 'opaque-kdf', work: 42 },
    encrypted_vault_key: 'already-008-encrypted-key',
    key_updated_at: new Date('2025-06-01T00:00:00.000Z'),
    created_at: new Date('2024-06-01T00:00:00.000Z'),
  })

  const existingObjects = await db
    .selectFrom('objects')
    .where('collection_id', '=', existingCollectionId)
    .where('user_id', '=', userId)
    .select([
      'id',
      'collection_id',
      'user_id',
      'version',
      'change_seq',
      'deleted',
      'blob_key',
      'size_bytes',
      'created_at',
      'updated_at',
    ])
    .orderBy('created_at')
    .execute()
  assert.deepEqual(existingObjects, [
    {
      id: liveObjectId,
      collection_id: existingCollectionId,
      user_id: userId,
      version: '3',
      change_seq: '9',
      deleted: false,
      blob_key: liveBlobKey,
      size_bytes: '444',
      created_at: new Date('2024-07-01T00:00:00.000Z'),
      updated_at: new Date('2025-07-01T00:00:00.000Z'),
    },
    {
      id: tombstoneObjectId,
      collection_id: existingCollectionId,
      user_id: userId,
      version: '5',
      change_seq: '12',
      deleted: true,
      blob_key: tombstoneBlobKey,
      size_bytes: '555',
      created_at: new Date('2024-08-01T00:00:00.000Z'),
      updated_at: new Date('2025-08-01T00:00:00.000Z'),
    },
  ])

  const existingOrphan = await db
    .selectFrom('orphaned_blobs')
    .where('blob_key', '=', orphanBlobKey)
    .where('user_id', '=', userId)
    .select(['blob_key', 'user_id', 'size_bytes', 'orphaned_at'])
    .executeTakeFirstOrThrow()
  assert.deepEqual(existingOrphan, {
    blob_key: orphanBlobKey,
    user_id: userId,
    size_bytes: '666',
    orphaned_at: new Date('2025-09-01T00:00:00.000Z'),
  })

  await db
    .insertInto('collections')
    .values({ id: uuidv7(), user_id: userId })
    .execute()

  const collections = await db
    .selectFrom('collections')
    .where('user_id', '=', userId)
    .select('id')
    .execute()
  assert.equal(collections.length, 2)

})
