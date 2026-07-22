// Test runner: one `bun test` process per file, so each file gets the per-file
// isolation the suite relies on (the `db` pool is a module singleton each file
// destroys in afterAll, and blob-limit/auth-password snapshot env at module
// load). Files are grouped by the AUTH_MODE they need.
//
//   bun scripts/test.ts            # all groups
//   bun scripts/test.ts dev        # one group
//   bun scripts/test.ts password
const groups = [
  {
    name: 'dev',
    env: { AUTH_MODE: 'dev' },
    files: ['e2ee-routes', 'migration-upgrades', 'capability', 'sync-routes', 'isolation', 'blob-limit', 'blobs-batch'],
  },
  {
    name: 'password',
    env: { AUTH_MODE: 'password' },
    files: ['auth-password', 'auth-password-plaintext', 'auth-rate-limit'],
  },
]

const filter = process.argv[2]
const selected = filter ? groups.filter((g) => g.name === filter) : groups
if (filter && selected.length === 0) {
  console.error(`unknown test group: ${filter} (expected: ${groups.map((g) => g.name).join(', ')})`)
  process.exit(2)
}

const failures: string[] = []
for (const group of selected) {
  for (const file of group.files) {
    const path = `tests/${file}.test.ts`
    const result = Bun.spawnSync(['bun', 'test', path], {
      env: { ...process.env, ...group.env },
      stdout: 'inherit',
      stderr: 'inherit',
    })
    if (!result.success) failures.push(path)
  }
}

if (failures.length > 0) {
  console.error(`\n${failures.length} test file(s) failed:\n${failures.map((f) => `  - ${f}`).join('\n')}`)
  process.exit(1)
}
console.log('\nAll test files passed.')
