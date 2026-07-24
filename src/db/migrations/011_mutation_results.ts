import { type Kysely, sql } from 'kysely'

export async function up(db: Kysely<unknown>): Promise<void> {
  await db.schema
    .createTable('mutation_results')
    .ifNotExists()
    .addColumn('user_id', 'uuid', (c) =>
      c.notNull().references('users.id').onDelete('cascade'),
    )
    .addColumn('mutation_id', 'text', (c) => c.notNull())
    .addColumn('kind', 'text', (c) => c.notNull())
    .addColumn('collection_id', 'uuid', (c) => c.notNull())
    .addColumn('object_id', 'uuid')
    .addColumn('requested_version', 'bigint')
    .addColumn('result', 'jsonb', (c) => c.notNull())
    .addColumn('created_at', 'timestamptz', (c) => c.notNull().defaultTo(sql`now()`))
    .addPrimaryKeyConstraint('mutation_results_pkey', ['user_id', 'mutation_id'])
    .addCheckConstraint(
      'mutation_results_kind_check',
      sql`kind in ('create', 'update', 'delete')`,
    )
    .addCheckConstraint(
      'mutation_results_id_length_check',
      sql`char_length(mutation_id) between 1 and 128`,
    )
    .execute()

  await db.schema
    .createIndex('idx_mutation_results_created_at')
    .ifNotExists()
    .on('mutation_results')
    .column('created_at')
    .execute()
}

export async function down(db: Kysely<unknown>): Promise<void> {
  await db.schema.dropTable('mutation_results').execute()
}
