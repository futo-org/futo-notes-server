import type { ColumnType, Generated } from 'kysely'

export interface Database {
  users: UsersTable
  sessions: SessionsTable
  collections: CollectionsTable
  objects: ObjectsTable
}

export interface UsersTable {
  id: string
  sub: string
  name: string
  email: string
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
  created_at: Generated<Date>
}

export interface ObjectsTable {
  id: string
  collection_id: string
  user_id: string
  version: Generated<string> // bigint arrives as string from pg
  deleted: Generated<boolean>
  blob_key: string | null
  size_bytes: string | null // bigint
  created_at: Generated<Date>
  updated_at: Generated<Date>
}
