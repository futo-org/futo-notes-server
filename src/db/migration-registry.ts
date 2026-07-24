import * as m001 from './migrations/001_initial.ts'
import * as m002 from './migrations/002_global_sync_cursor.ts'
import * as m003 from './migrations/003_collection_key_material.ts'
import * as m004 from './migrations/004_server_config.ts'
import * as m005 from './migrations/005_user_passwords.ts'
import * as m006 from './migrations/006_drop_user_password_hash.ts'
import * as m007 from './migrations/007_orphaned_blobs.ts'
import * as m008 from './migrations/008_single_collection_per_user.ts'
import * as m009 from './migrations/009_restore_plural_collections.ts'
import * as m010 from './migrations/010_authoritative_blob_ledger.ts'
import * as m011 from './migrations/011_mutation_results.ts'

export const migrations: Record<string, { up: typeof m001.up; down?: typeof m001.down }> = {
  '001_initial': m001,
  '002_global_sync_cursor': m002,
  '003_collection_key_material': m003,
  '004_server_config': m004,
  '005_user_passwords': m005,
  '006_drop_user_password_hash': m006,
  '007_orphaned_blobs': m007,
  '008_single_collection_per_user': m008,
  '009_restore_plural_collections': m009,
  '010_authoritative_blob_ledger': m010,
  '011_mutation_results': m011,
}
