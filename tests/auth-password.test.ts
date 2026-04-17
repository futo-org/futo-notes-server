import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { after, before, test } from 'node:test'
// password.ts has no env.ts dependency, so importing it here does not cache
// env.ts before we've set STONEFRUIT_PASSWORD_HASH below.
import { hashPassword } from '../src/auth/password.ts'

const PASSWORD = 'hunter2-test'
process.env.STONEFRUIT_PASSWORD_HASH = await hashPassword(PASSWORD)

// Dynamic imports so env.ts sees the hash we just set.
const { buildApp } = await import('../src/app.ts')
const { db, waitForDb } = await import('../src/db/connection.ts')
const { migrateToLatest } = await import('../src/db/migrate.ts')

const app = buildApp()

before(async () => {
  await waitForDb()
  await migrateToLatest()
  // Start clean — lazy-upsert will recreate the singleton.
  await db.deleteFrom('sessions').execute()
  await db.deleteFrom('users').where('sub', '=', 'local').execute()
})

after(async () => {
  await db.destroy()
})

test('login with correct password returns a usable session', async () => {
  const response = await app.fetch(new Request('http://test.local/api/auth/password/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: PASSWORD }),
  }))
  assert.equal(response.status, 200)
  const data = await response.json() as { user: { id: string; email: string; name: string }; token: string }
  assert.ok(data.token)
  assert.equal(data.user.email, 'local@stonefruit.local')

  // Token works as a bearer on an authenticated route.
  const whoami = await app.fetch(new Request('http://test.local/api/auth', {
    headers: { Authorization: `Bearer ${data.token}` },
  }))
  assert.equal(whoami.status, 200)
  const whoamiData = await whoami.json() as { user: { id: string } }
  assert.equal(whoamiData.user.id, data.user.id)
})

test('login with wrong password returns 401', async () => {
  const response = await app.fetch(new Request('http://test.local/api/auth/password/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: 'nope' }),
  }))
  assert.equal(response.status, 401)
})

test('validateEnv rejects password mode without a hash', () => {
  // Run validateEnv in a subprocess with AUTH_MODE=password but no hash.
  const result = spawnSync(
    'node',
    ['--import', 'tsx', '-e', 'import("./src/env.ts").then(({ validateEnv }) => validateEnv())'],
    {
      env: {
        ...process.env,
        DATABASE_URL: 'postgres://x:y@z/x',
        AUTH_MODE: 'password',
        STONEFRUIT_PASSWORD_HASH: '',
      },
      encoding: 'utf8',
    },
  )
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /STONEFRUIT_PASSWORD_HASH/)
})
