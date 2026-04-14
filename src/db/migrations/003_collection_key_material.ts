import { type Kysely } from 'kysely'

export async function up(db: Kysely<unknown>): Promise<void> {
  await db.schema
    .alterTable('collections')
    .addColumn('key_salt', 'text')
    .addColumn('key_kdf', 'jsonb')
    .addColumn('encrypted_vault_key', 'text')
    .addColumn('key_updated_at', 'timestamptz')
    .execute()
}

export async function down(db: Kysely<unknown>): Promise<void> {
  await db.schema
    .alterTable('collections')
    .dropColumn('key_updated_at')
    .dropColumn('encrypted_vault_key')
    .dropColumn('key_kdf')
    .dropColumn('key_salt')
    .execute()
}
