import * as m001 from './migrations/001_initial.ts'
import * as m002 from './migrations/002_global_sync_cursor.ts'
import * as m003 from './migrations/003_collection_key_material.ts'
import * as m004 from './migrations/004_server_config.ts'
import * as m005 from './migrations/005_user_passwords.ts'

export const migrations: Record<string, { up: typeof m001.up; down?: typeof m001.down }> = {
  '001_initial': m001,
  '002_global_sync_cursor': m002,
  '003_collection_key_material': m003,
  '004_server_config': m004,
  '005_user_passwords': m005,
}
