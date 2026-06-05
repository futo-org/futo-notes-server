// Fails if migration files in src/db/migrations/ do not match the keys
// registered in src/db/migration-registry.ts.
//
// Why: dev and tests run via tsx and read migrations off the filesystem
// (FileMigrationProvider), so a forgotten registry entry is invisible there.
// The production esbuild bundle has no filesystem migrations and uses the
// static registry instead — a missing entry passes the whole pipeline and
// only fails at container startup. This check catches it in CI.
import { readdirSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)))
const migrationsDir = path.join(root, 'src', 'db', 'migrations')

const files = new Set(
  readdirSync(migrationsDir)
    .filter((f) => f.endsWith('.ts'))
    .map((f) => f.slice(0, -'.ts'.length)),
)

const { migrations } = await import(path.join(root, 'src', 'db', 'migration-registry.ts'))
const registered = new Set(Object.keys(migrations))

const missing = [...files].filter((name) => !registered.has(name)).sort()
const stale = [...registered].filter((name) => !files.has(name)).sort()

if (missing.length || stale.length) {
  if (missing.length) {
    console.error('Migration files not registered in src/db/migration-registry.ts:')
    for (const name of missing) console.error(`  - ${name}`)
  }
  if (stale.length) {
    console.error('Registry entries with no matching file in src/db/migrations/:')
    for (const name of stale) console.error(`  - ${name}`)
  }
  console.error(
    '\nThe production bundle loads migrations from the static registry, not the filesystem.\n' +
      'Register every migration in src/db/migration-registry.ts (see src/db/AGENTS.md).',
  )
  process.exit(1)
}

console.log(`Migration registry in sync (${files.size} migrations).`)
