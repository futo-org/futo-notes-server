import { existsSync } from 'node:fs'
import { promises as fs } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { FileMigrationProvider, Migrator } from 'kysely'
import { db, waitForDb } from './connection.ts'
import { log } from '../logger.ts'
import { migrations as staticMigrations } from './migration-registry.ts'

function createProvider() {
  // When running via tsx in dev, the migrations/ directory exists on disk.
  // When bundled (esbuild), it won't — fall back to the static registry.
  const dir = path.join(path.dirname(fileURLToPath(import.meta.url)), 'migrations')
  if (existsSync(dir)) {
    return { provider: new FileMigrationProvider({ fs, path, migrationFolder: dir }), mode: 'file' as const }
  }
  return { provider: { getMigrations: async () => staticMigrations }, mode: 'static' as const }
}

export async function migrateToLatest(): Promise<void> {
  const { provider, mode } = createProvider()
  log.debug('running migrations', { mode })

  const migrator = new Migrator({ db, provider })
  const { error, results } = await migrator.migrateToLatest()

  for (const r of results ?? []) {
    if (r.status === 'Success') {
      log.info(`migration ${r.migrationName} (${r.direction})`)
    } else if (r.status === 'Error') {
      log.error(`migration ${r.migrationName} failed`)
    }
  }

  if (error) throw error
}

// CLI entry: `pnpm migrate` (only runs when this file is the direct entry point,
// not when bundled as part of the server).
const __filename = fileURLToPath(import.meta.url)
if (__filename === process.argv[1] && __filename.endsWith('migrate.ts')) {
  await waitForDb()
  await migrateToLatest()
  await db.destroy()
}
