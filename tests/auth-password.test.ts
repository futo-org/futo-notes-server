import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { afterAll, beforeAll, test } from 'bun:test'
// password.ts has no env.ts dependency, so importing it here does not cache
// env.ts before we've set FUTO_NOTES_PASSWORD_HASH below.
import { hashPassword } from '../src/auth/password.ts'

const PASSWORD = 'hunter2-test'
process.env.FUTO_NOTES_PASSWORD_HASH = await hashPassword(PASSWORD)

// Dynamic imports so env.ts sees the hash we just set.
const { buildApp } = await import('../src/app.ts')
const { db, waitForDb } = await import('../src/db/connection.ts')
const { migrateToLatest } = await import('../src/db/migrate.ts')

const app = buildApp()

beforeAll(async () => {
  await waitForDb()
  await migrateToLatest()
  // Start clean — lazy-upsert will recreate the singleton.
  await db.deleteFrom('sessions').execute()
  await db.deleteFrom('users').where('sub', '=', 'local').execute()
})

afterAll(async () => {
  await db.destroy()
})

test('login with correct password returns a usable session', async () => {
  const response = await app.fetch(new Request('http://test.local/api/auth/password/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: PASSWORD }),
  }))
  assert.equal(response.status, 200)
  // Password mode defaults COOKIE_SECURE=true, so the cookie must carry Secure.
  assert.match(response.headers.get('set-cookie') ?? '', /Secure/)
  const data = await response.json() as { user: { id: string; email: string; name: string }; token: string }
  assert.ok(data.token)
  assert.equal(data.user.email, 'local@futo-notes.local')

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
    'bun',
    ['--no-env-file', '-e', 'import("./src/env.ts").then(({ validateEnv }) => validateEnv())'],
    {
      env: {
        ...process.env,
        DATABASE_URL: 'postgres://x:y@z/x',
        AUTH_MODE: 'password',
        FUTO_NOTES_PASSWORD_HASH: '',
      },
      encoding: 'utf8',
    },
  )
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /FUTO_NOTES_PASSWORD_HASH/)
})
