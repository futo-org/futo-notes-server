import { Hono } from 'hono'
import type { Context } from 'hono'
import { setCookie } from 'hono/cookie'
import { uuidv7 } from 'uuidv7'
import { db } from '../db/connection.ts'
import { log } from '../logger.ts'
import { hashPassword, verifyPassword } from './password.ts'
import { createSession } from './session.ts'

const SESSION_MAX_AGE_SECONDS = 7 * 24 * 60 * 60

export const signupRoutes = new Hono()

function isValidEmail(s: string): boolean {
  // Minimal check — the point isn't RFC compliance, it's "did the user enter
  // something that looks like an email on the server form".
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(s)
}

function setSessionCookie(c: Context, rawToken: string): void {
  setCookie(c, 'session', rawToken, {
    httpOnly: true,
    sameSite: 'Lax',
    path: '/',
    maxAge: SESSION_MAX_AGE_SECONDS,
  })
}

/**
 * POST /api/auth/signup — create a new account.
 * Body: { email, password, name? }
 * Returns 409 if email is already registered.
 */
signupRoutes.post('/api/auth/signup', async (c) => {
  let body: { email?: string; password?: string; name?: string }
  try {
    body = await c.req.json()
  } catch {
    return c.json({ error: 'invalid json' }, 400)
  }

  const email = body.email?.trim().toLowerCase()
  const password = body.password
  const name = body.name?.trim() || email?.split('@')[0]

  if (!email || !isValidEmail(email)) {
    return c.json({ error: 'valid email is required' }, 422)
  }
  if (!password || password.length < 8) {
    return c.json({ error: 'password must be at least 8 characters' }, 422)
  }
  if (!name) {
    return c.json({ error: 'name is required' }, 422)
  }

  const existing = await db
    .selectFrom('users')
    .select('id')
    .where('email', '=', email)
    .executeTakeFirst()
  if (existing) {
    return c.json({ error: 'email already registered' }, 409)
  }

  const hash = await hashPassword(password)
  const user = await db
    .insertInto('users')
    .values({
      id: uuidv7(),
      sub: `email:${email}`,
      name,
      email,
      password_hash: hash,
    })
    .returning(['id', 'email', 'name'])
    .executeTakeFirstOrThrow()

  const { rawToken } = await createSession(user.id)
  setSessionCookie(c, rawToken)

  log.info('user signed up', { email })
  return c.json({ user, token: rawToken }, 201)
})

/**
 * POST /api/auth/login — authenticate with email + password.
 */
signupRoutes.post('/api/auth/login', async (c) => {
  let body: { email?: string; password?: string }
  try {
    body = await c.req.json()
  } catch {
    return c.json({ error: 'invalid json' }, 400)
  }

  const email = body.email?.trim().toLowerCase()
  const password = body.password
  if (!email || !password) {
    return c.json({ error: 'email and password are required' }, 400)
  }

  const row = await db
    .selectFrom('users')
    .select(['id', 'email', 'name', 'password_hash'])
    .where('email', '=', email)
    .executeTakeFirst()

  if (!row || !row.password_hash) {
    return c.json({ error: 'invalid email or password' }, 401)
  }

  const valid = await verifyPassword(password, row.password_hash)
  if (!valid) {
    return c.json({ error: 'invalid email or password' }, 401)
  }

  const { rawToken } = await createSession(row.id)
  setSessionCookie(c, rawToken)

  return c.json({
    user: { id: row.id, email: row.email, name: row.name },
    token: rawToken,
  }, 200)
})

/** Always true now that there's no server-wide setup step. Kept for the test. */
export async function isSetupComplete(): Promise<boolean> {
  return true
}
