import { type Kysely, sql } from 'kysely'

export async function up(db: Kysely<unknown>): Promise<void> {
  await db.schema
    .createTable('users')
    .addColumn('id', 'uuid', (c) => c.primaryKey())
    .addColumn('sub', 'text', (c) => c.notNull().unique())
    .addColumn('name', 'text', (c) => c.notNull())
    .addColumn('email', 'text', (c) => c.notNull().unique())
    .execute()

  await db.schema
    .createTable('sessions')
    .addColumn('id', 'uuid', (c) => c.primaryKey())
    .addColumn('user_id', 'uuid', (c) =>
      c.notNull().references('users.id').onDelete('cascade'),
    )
    .addColumn('access_token_hash', 'bytea', (c) => c.notNull().unique())
    .addColumn('expires_at', 'timestamptz', (c) => c.notNull())
    .execute()

  await db.schema
    .createIndex('idx_sessions_token_hash')
    .on('sessions')
    .column('access_token_hash')
    .execute()

  await db.schema
    .createTable('collections')
    .addColumn('id', 'uuid', (c) => c.primaryKey())
    .addColumn('user_id', 'uuid', (c) =>
      c.notNull().references('users.id').onDelete('cascade'),
    )
    .addColumn('created_at', 'timestamptz', (c) => c.notNull().defaultTo(sql`now()`))
    .execute()

  await db.schema
    .createIndex('idx_collections_user')
    .on('collections')
    .column('user_id')
    .execute()

  await db.schema
    .createTable('objects')
    .addColumn('id', 'uuid', (c) => c.primaryKey())
    .addColumn('collection_id', 'uuid', (c) =>
      c.notNull().references('collections.id').onDelete('cascade'),
    )
    .addColumn('user_id', 'uuid', (c) =>
      c.notNull().references('users.id').onDelete('cascade'),
    )
    .addColumn('version', 'bigint', (c) => c.notNull().defaultTo(1))
    .addColumn('deleted', 'boolean', (c) => c.notNull().defaultTo(false))
    .addColumn('blob_key', 'text')
    .addColumn('size_bytes', 'bigint')
    .addColumn('created_at', 'timestamptz', (c) => c.notNull().defaultTo(sql`now()`))
    .addColumn('updated_at', 'timestamptz', (c) => c.notNull().defaultTo(sql`now()`))
    .execute()

  await db.schema
    .createIndex('idx_objects_collection')
    .on('objects')
    .columns(['collection_id', 'user_id'])
    .execute()
}

export async function down(db: Kysely<unknown>): Promise<void> {
  await db.schema.dropTable('objects').execute()
  await db.schema.dropTable('collections').execute()
  await db.schema.dropTable('sessions').execute()
  await db.schema.dropTable('users').execute()
}
