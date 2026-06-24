import assert from 'node:assert/strict'
import { afterAll, beforeAll, test } from 'bun:test'
// password.ts has no env.ts dependency, so importing it here does not cache
// env.ts before we set the env below.
import { hashPassword } from '../src/auth/password.ts'

const PASSWORD = 'hunter2-test'
// Set the knobs BEFORE importing anything that reads env.ts. A small limit and
// a long window make the limiter deterministic within a single test run.
process.env.AUTH_MODE = 'password'
process.env.FUTO_NOTES_PASSWORD_HASH = await hashPassword(PASSWORD)
process.env.AUTH_RATE_LIMIT = '3'
process.env.AUTH_RATE_LIMIT_WINDOW_MS = '60000'

const { buildApp } = await import('../src/app.ts')
const { _resetRateLimit } = await import('../src/auth/rate-limit.ts')

const app = buildApp()

function login(password: string): Promise<Response> {
  return app.fetch(new Request('http://test.local/api/auth/password/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password }),
  }))
}

beforeAll(() => {
  _resetRateLimit()
})

afterAll(() => {
  _resetRateLimit()
})

// Wrong-password requests never touch the DB (scrypt fails → 401 before any
// query), so this test is hermetic — it exercises the limiter, not auth.
test('blocks with 429 + Retry-After once the per-window limit is exceeded', async () => {
  // First AUTH_RATE_LIMIT (=3) attempts are allowed through to the auth check.
  for (let i = 0; i < 3; i++) {
    const res = await login('wrong')
    assert.equal(res.status, 401, `attempt ${i + 1} should reach auth (401), got ${res.status}`)
  }

  // The 4th within the window is rejected before auth runs.
  const limited = await login('wrong')
  assert.equal(limited.status, 429)
  const retryAfter = Number(limited.headers.get('retry-after'))
  assert.ok(retryAfter > 0 && retryAfter <= 60, `Retry-After should be a positive ≤60s, got ${retryAfter}`)
  const body = await limited.json() as { error: string }
  assert.equal(body.error, 'too many requests')

  // A correct password is also blocked while the window is saturated — the
  // limiter gates the path, not the outcome.
  const correctButLimited = await login(PASSWORD)
  assert.equal(correctButLimited.status, 429)
})
