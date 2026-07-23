import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { afterAll, beforeAll, test } from 'bun:test'
// password.ts has no env.ts dependency, so importing it here does not cache
// env.ts before we've set FUTO_NOTES_PASSWORD_HASH below.
import { hashPassword } from '../src/auth/password.ts'

const PASSWORD = 'hunter2-test'
// Production Compose forwards the unused plaintext alternative as an empty
// value; this must not shadow a configured hash.
process.env.FUTO_NOTES_PASSWORD = ''
process.env.FUTO_NOTES_PASSWORD_HASH = await hashPassword(PASSWORD)

// Dynamic imports so env.ts sees the hash we just set.
const { buildApp } = await import('../src/app.ts')
const { db, waitForDb } = await import('../src/db/connection.ts')
const { migrateToLatest } = await import('../src/db/migrate.ts')
const { hashToken } = await import('../src/auth/session.ts')

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

test('expired bearer session returns a machine-readable reauthentication signal', async () => {
  const login = await app.fetch(new Request('http://test.local/api/auth/password/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: PASSWORD }),
  }))
  assert.equal(login.status, 200)
  const { token } = await login.json() as { token: string }

  await db
    .updateTable('sessions')
    .set({ expires_at: new Date(Date.now() - 1_000) })
    .where('access_token_hash', '=', hashToken(token))
    .execute()

  const response = await app.fetch(new Request('http://test.local/api/auth', {
    headers: { Authorization: `Bearer ${token}` },
  }))
  assert.equal(response.status, 401)
  assert.equal(
    response.headers.get('www-authenticate'),
    'Bearer realm="futo-notes", error="invalid_token"',
  )
  assert.deepEqual(await response.json(), {
    error: 'session expired or invalid',
    code: 'invalid_session',
  })
})

test('authenticated activity does not extend the session expiry', async () => {
  const login = await app.fetch(new Request('http://test.local/api/auth/password/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: PASSWORD }),
  }))
  assert.equal(login.status, 200)
  const { token } = await login.json() as { token: string }
  const fixedExpiry = new Date(Date.now() + 60_000)

  await db
    .updateTable('sessions')
    .set({ expires_at: fixedExpiry })
    .where('access_token_hash', '=', hashToken(token))
    .execute()

  const response = await app.fetch(new Request('http://test.local/api/auth', {
    headers: { Authorization: `Bearer ${token}` },
  }))
  assert.equal(response.status, 200)

  const session = await db
    .selectFrom('sessions')
    .select('expires_at')
    .where('access_token_hash', '=', hashToken(token))
    .executeTakeFirstOrThrow()
  assert.equal(new Date(session.expires_at).getTime(), fixedExpiry.getTime())
})

test('validateEnv rejects password mode without either password option', () => {
  // Run validateEnv in a subprocess with AUTH_MODE=password but no credential.
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
  assert.match(result.stderr, /FUTO_NOTES_PASSWORD or FUTO_NOTES_PASSWORD_HASH/)
})
