import { promises as fs } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { FileMigrationProvider, Migrator } from 'kysely'
import { db, waitForDb } from './connection.ts'

export async function migrateToLatest(): Promise<void> {
  const migrator = new Migrator({
    db,
    provider: new FileMigrationProvider({
      fs,
      path,
      migrationFolder: path.join(path.dirname(fileURLToPath(import.meta.url)), 'migrations'),
    }),
  })

  const { error, results } = await migrator.migrateToLatest()

  for (const r of results ?? []) {
    if (r.status === 'Success') {
      console.log(`✓ ${r.migrationName} (${r.direction})`)
    } else if (r.status === 'Error') {
      console.error(`✗ ${r.migrationName} failed`)
    }
  }

  if (error) throw error
}

// CLI entry: `pnpm migrate`. The server also calls migrateToLatest() on
// startup, so this is optional — keep it around for standalone runs.
if (fileURLToPath(import.meta.url) === process.argv[1]) {
  await waitForDb()
  await migrateToLatest()
  await db.destroy()
}
