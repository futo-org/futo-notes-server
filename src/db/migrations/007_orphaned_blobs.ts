import { type Kysely, sql } from 'kysely'

// Retention ledger for blobs that have been replaced by a newer version of
// an object. Clients keep the previous blobKey in their local object map and
// fetch it on demand to resolve three-way merge conflicts — so we must keep
// orphaned blobs around for long enough that a plausibly-delayed client can
// still reach them. `orphaned_at` is stamped at the moment of replacement;
// a periodic GC job deletes rows (and their blobs) past the retention window.

export async function up(db: Kysely<unknown>): Promise<void> {
  await db.schema
    .createTable('orphaned_blobs')
    .addColumn('blob_key', 'text', (c) => c.primaryKey())
    .addColumn('user_id', 'uuid', (c) =>
      c.notNull().references('users.id').onDelete('cascade'),
    )
    .addColumn('size_bytes', 'bigint', (c) => c.notNull())
    .addColumn('orphaned_at', 'timestamptz', (c) => c.notNull().defaultTo(sql`now()`))
    .execute()

  await db.schema
    .createIndex('idx_orphaned_blobs_orphaned_at')
    .on('orphaned_blobs')
    .column('orphaned_at')
    .execute()
}

export async function down(db: Kysely<unknown>): Promise<void> {
  await db.schema.dropTable('orphaned_blobs').execute()
}
