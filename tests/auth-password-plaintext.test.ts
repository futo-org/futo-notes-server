import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { afterAll, beforeAll, test } from 'bun:test'

const PASSWORD = 'correct horse battery staple'
process.env.FUTO_NOTES_PASSWORD = PASSWORD
delete process.env.FUTO_NOTES_PASSWORD_HASH

const { buildApp } = await import('../src/app.ts')
const { db, waitForDb } = await import('../src/db/connection.ts')
const { migrateToLatest } = await import('../src/db/migrate.ts')

const app = buildApp()

beforeAll(async () => {
  await waitForDb()
  await migrateToLatest()
  await db.deleteFrom('sessions').execute()
  await db.deleteFrom('users').where('sub', '=', 'local').execute()
})

afterAll(async () => {
  await db.destroy()
})

test('plaintext password configuration can log in through the password endpoint', async () => {
  const response = await app.fetch(new Request('http://test.local/api/auth/password/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: PASSWORD }),
  }))

  assert.equal(response.status, 200)
  const data = await response.json() as { token: string }
  assert.ok(data.token)
})

test('plaintext password configuration rejects a wrong password', async () => {
  const response = await app.fetch(new Request('http://test.local/api/auth/password/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: `${PASSWORD}!` }),
  }))

  assert.equal(response.status, 401)
})

test('validateEnv rejects ambiguous plaintext and hash configuration', () => {
  const result = spawnSync(
    'bun',
    ['--no-env-file', '-e', 'import("./src/env.ts").then(({ validateEnv }) => validateEnv())'],
    {
      env: {
        ...process.env,
        DATABASE_URL: 'postgres://x:y@z/x',
        AUTH_MODE: 'password',
        FUTO_NOTES_PASSWORD: PASSWORD,
        FUTO_NOTES_PASSWORD_HASH: 'configured-too',
      },
      encoding: 'utf8',
    },
  )

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /only one of FUTO_NOTES_PASSWORD or FUTO_NOTES_PASSWORD_HASH/i)
})
