import type { ColumnType, Generated } from 'kysely'

export interface Database {
  users: UsersTable
  sessions: SessionsTable
  collections: CollectionsTable
  objects: ObjectsTable
  server_config: ServerConfigTable
}

export interface UsersTable {
  id: string
  sub: string
  name: string
  email: string
  password_hash: string | null
}

export interface SessionsTable {
  id: string
  user_id: string
  access_token_hash: Buffer
  expires_at: ColumnType<Date, Date | string, Date | string>
}

export interface CollectionsTable {
  id: string
  user_id: string
  current_version: Generated<string> // bigint arrives as string from pg
  key_salt: string | null
  key_kdf: Record<string, unknown> | null
  encrypted_vault_key: string | null
  key_updated_at: ColumnType<Date | null, Date | string | null, Date | string | null>
  created_at: Generated<Date>
}

export interface ObjectsTable {
  id: string
  collection_id: string
  user_id: string
  version: Generated<string> // bigint arrives as string from pg
  change_seq: Generated<string> // collection-global sync cursor
  deleted: Generated<boolean>
  blob_key: string | null
  size_bytes: string | null // bigint
  created_at: Generated<Date>
  updated_at: Generated<Date>
}

export interface ServerConfigTable {
  key: string
  value: string
}
