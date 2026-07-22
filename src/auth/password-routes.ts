import { Hono } from 'hono'
import type { Context } from 'hono'
import { setCookie } from 'hono/cookie'
import { createHash, timingSafeEqual } from 'node:crypto'
import { uuidv7 } from 'uuidv7'
import { db } from '../db/connection.ts'
import { env } from '../env.ts'
import { log } from '../logger.ts'
import { verifyPassword } from './password.ts'
import { createSession } from './session.ts'

const SESSION_MAX_AGE_SECONDS = 7 * 24 * 60 * 60

// The singleton user in password mode. All E2EE data is owned by this row.
const SINGLETON_SUB = 'local'
const SINGLETON_EMAIL = 'local@futo-notes.local'
const SINGLETON_NAME = 'FUTO Notes'

export const passwordRoutes = new Hono()

function verifyPlaintextPassword(submitted: string, configured: string): boolean {
  // Compare fixed-size digests so neither content nor length affects the
  // comparison's timing. Plaintext mode intentionally avoids scrypt's setup
  // cost; the login rate limiter remains the online brute-force defense.
  const submittedDigest = createHash('sha256').update(submitted, 'utf8').digest()
  const configuredDigest = createHash('sha256').update(configured, 'utf8').digest()
  return timingSafeEqual(submittedDigest, configuredDigest)
}

function setSessionCookie(c: Context, rawToken: string): void {
  setCookie(c, 'session', rawToken, {
    httpOnly: true,
    secure: env.COOKIE_SECURE,
    sameSite: 'Lax',
    path: '/',
    maxAge: SESSION_MAX_AGE_SECONDS,
  })
}

/**
 * POST /api/auth/password/login
 *
 * Verifies the submitted password against the configured plaintext or hash, upserts
 * the singleton user on first login, and opens a session.
 */
passwordRoutes.post('/login', async (c) => {
  let body: { password?: string }
  try {
    body = await c.req.json()
  } catch {
    return c.json({ error: 'invalid json' }, 400)
  }

  const password = body.password
  if (!password) {
    return c.json({ error: 'password is required' }, 400)
  }

  const valid = env.FUTO_NOTES_PASSWORD !== undefined
    ? verifyPlaintextPassword(password, env.FUTO_NOTES_PASSWORD)
    : await verifyPassword(password, env.FUTO_NOTES_PASSWORD_HASH!)
  if (!valid) {
    return c.json({ error: 'invalid password' }, 401)
  }

  const existing = await db
    .selectFrom('users')
    .select(['id', 'email', 'name'])
    .where('sub', '=', SINGLETON_SUB)
    .executeTakeFirst()

  const user = existing ?? await db
    .insertInto('users')
    .values({
      id: uuidv7(),
      sub: SINGLETON_SUB,
      email: SINGLETON_EMAIL,
      name: SINGLETON_NAME,
    })
    .returning(['id', 'email', 'name'])
    .executeTakeFirstOrThrow()

  const { rawToken } = await createSession(user.id)
  setSessionCookie(c, rawToken)
  log.info('password login')
  return c.json({ user, token: rawToken }, 200)
})
