import { type Kysely, sql } from 'kysely'

export async function up(db: Kysely<unknown>): Promise<void> {
  await db.schema
    .createTable('blob_ledger')
    .ifNotExists()
    .addColumn('blob_key', 'text', (c) => c.primaryKey())
    .addColumn('user_id', 'uuid', (c) =>
      c.notNull().references('users.id').onDelete('cascade'),
    )
    .addColumn('size_bytes', 'bigint', (c) => c.notNull())
    .addColumn('state', 'text', (c) => c.notNull())
    .addColumn('collection_id', 'uuid')
    .addColumn('object_id', 'uuid', (c) =>
      c.references('objects.id').onDelete('set null'),
    )
    .addColumn('object_version', 'bigint')
    .addColumn('created_at', 'timestamptz', (c) => c.notNull().defaultTo(sql`now()`))
    .addColumn('state_changed_at', 'timestamptz', (c) => c.notNull().defaultTo(sql`now()`))
    .addCheckConstraint(
      'blob_ledger_state_check',
      sql`state in ('staged', 'claimed', 'retained', 'purgeable', 'legacy_shared')`,
    )
    .execute()

  await db.schema
    .createIndex('idx_blob_ledger_user_state')
    .ifNotExists()
    .on('blob_ledger')
    .columns(['user_id', 'state'])
    .execute()
  await db.schema
    .createIndex('idx_blob_ledger_cleanup')
    .ifNotExists()
    .on('blob_ledger')
    .columns(['state', 'state_changed_at'])
    .execute()
  await db.schema
    .createIndex('idx_blob_ledger_object')
    .ifNotExists()
    .on('blob_ledger')
    .column('object_id')
    .execute()

  // Existing object rows are authoritative. Historic duplicate references are
  // preserved as legacy_shared and are never eligible for cleanup.
  await sql`
    insert into blob_ledger (
      blob_key,
      user_id,
      size_bytes,
      state,
      collection_id,
      object_id,
      object_version,
      created_at,
      state_changed_at
    )
    select
      blob_key,
      (array_agg(user_id order by user_id::text))[1],
      coalesce(max(size_bytes), 0),
      case when count(*) = 1 then 'claimed' else 'legacy_shared' end,
      case
        when count(distinct collection_id) = 1
        then (array_agg(collection_id order by collection_id::text))[1]
        else null
      end,
      case when count(*) = 1 then (array_agg(id order by id::text))[1] else null end,
      case when count(*) = 1 then max(version) else null end,
      min(created_at),
      max(updated_at)
    from objects
    where blob_key is not null
    group by blob_key
    on conflict (blob_key) do nothing
  `.execute(db)

  // A key still referenced by an object stays claimed/legacy_shared. Otherwise
  // preserve the old retention timestamp in the canonical ledger.
  await sql`
    insert into blob_ledger (
      blob_key,
      user_id,
      size_bytes,
      state,
      collection_id,
      object_id,
      object_version,
      created_at,
      state_changed_at
    )
    select
      blob_key,
      user_id,
      size_bytes,
      'retained',
      null,
      null,
      null,
      orphaned_at,
      orphaned_at
    from orphaned_blobs
    on conflict (blob_key) do nothing
  `.execute(db)
}

export async function down(db: Kysely<unknown>): Promise<void> {
  await db.schema.dropTable('blob_ledger').execute()
}
