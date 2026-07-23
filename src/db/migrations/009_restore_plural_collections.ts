import { type Kysely, sql } from 'kysely'

/**
 * Restore the generic plural-collection schema for databases that ran the
 * original migration 008 from a development `:latest` image.
 *
 * Stable upgrades run the safe no-op 008 first, so the constraint is absent
 * and the ordinary user index already exists. Both DDL statements are
 * conditional to support either migration history without touching row data.
 */
export async function up(db: Kysely<unknown>): Promise<void> {
  await sql`
    alter table collections
    drop constraint if exists collections_user_id_unique
  `.execute(db)
  await sql`
    create index if not exists idx_collections_user
    on collections (user_id)
  `.execute(db)
}
