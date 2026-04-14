import { type Kysely, sql } from 'kysely'

export async function up(db: Kysely<unknown>): Promise<void> {
  await db.schema
    .alterTable('collections')
    .addColumn('current_version', 'bigint', (c) => c.notNull().defaultTo(0))
    .execute()

  await db.schema
    .alterTable('objects')
    .addColumn('change_seq', 'bigint', (c) => c.notNull().defaultTo(0))
    .execute()

  await sql`
    update objects
    set change_seq = version
  `.execute(db)

  await sql`
    update collections
    set current_version = coalesce((
      select max(objects.change_seq)
      from objects
      where objects.collection_id = collections.id
    ), 0)
  `.execute(db)

  await db.schema
    .createIndex('idx_objects_collection_change_seq')
    .on('objects')
    .columns(['collection_id', 'user_id', 'change_seq'])
    .execute()
}

export async function down(db: Kysely<unknown>): Promise<void> {
  await db.schema.dropIndex('idx_objects_collection_change_seq').execute()
  await db.schema.alterTable('objects').dropColumn('change_seq').execute()
  await db.schema.alterTable('collections').dropColumn('current_version').execute()
}
