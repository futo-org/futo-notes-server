import { Hono } from 'hono'
import { setCookie } from 'hono/cookie'
import { randomBytes } from 'node:crypto'
import { uuidv7 } from 'uuidv7'
import { db } from '../db/connection.ts'
import { env } from '../env.ts'
import { log } from '../logger.ts'
import { hashPassword, verifyPassword } from './password.ts'
import { createSession } from './session.ts'

const SESSION_MAX_AGE_SECONDS = 7 * 24 * 60 * 60

export const passwordRoutes = new Hono()

/** Helper to read a config value from server_config. */
async function getConfig(key: string): Promise<string | undefined> {
  const row = await db
    .selectFrom('server_config')
    .select('value')
    .where('key', '=', key)
    .executeTakeFirst()
  return row?.value
}

/** Helper to upsert a config value. */
async function setConfig(key: string, value: string): Promise<void> {
  await db
    .insertInto('server_config')
    .values({ key, value })
    .onConflict((oc) => oc.column('key').doUpdateSet({ value }))
    .execute()
}

/**
 * POST /setup — set the initial server password.
 * Returns 409 if already configured.
 */
passwordRoutes.post('/setup', async (c) => {
  let body: { password?: string }
  try {
    body = await c.req.json()
  } catch {
    return c.json({ error: 'invalid json' }, 400)
  }

  const password = body.password?.trim()
  if (!password || password.length < 8) {
    return c.json({ error: 'password must be at least 8 characters' }, 422)
  }

  const existing = await getConfig('password_hash')
  if (existing) {
    return c.json({ error: 'server already configured' }, 409)
  }

  const hash = await hashPassword(password)
  const adminToken = randomBytes(32).toString('hex')

  await setConfig('password_hash', hash)
  await setConfig('admin_token', adminToken)

  log.info('server setup complete')
  return c.json({ admin_token: adminToken }, 201)
})

/**
 * POST /api/auth/login — password-based login.
 * Creates a single "admin" user on first login, then reuses it.
 */
passwordRoutes.post('/api/auth/login', async (c) => {
  let body: { password?: string }
  try {
    body = await c.req.json()
  } catch {
    return c.json({ error: 'invalid json' }, 400)
  }

  const password = body.password?.trim()
  if (!password) {
    return c.json({ error: 'password is required' }, 400)
  }

  const hash = await getConfig('password_hash')
  if (!hash) {
    return c.json({ error: 'server not configured — call POST /setup first' }, 403)
  }

  const valid = await verifyPassword(password, hash)
  if (!valid) {
    return c.json({ error: 'invalid password' }, 401)
  }

  // Upsert the admin user
  const user = await db
    .insertInto('users')
    .values({ id: uuidv7(), sub: 'local:admin', name: 'Admin', email: 'admin@localhost' })
    .onConflict((oc) => oc.column('sub').doUpdateSet({ name: 'Admin' }))
    .returning(['id', 'email', 'name'])
    .executeTakeFirstOrThrow()

  const { rawToken } = await createSession(user.id)

  setCookie(c, 'session', rawToken, {
    httpOnly: true,
    sameSite: 'Lax',
    path: '/',
    maxAge: SESSION_MAX_AGE_SECONDS,
  })

  return c.json({ user, token: rawToken }, 200)
})

/**
 * POST /admin/reset-password — change the server password.
 * Requires the admin token in the AdminToken header.
 * Revokes all existing sessions.
 */
passwordRoutes.post('/admin/reset-password', async (c) => {
  const provided = c.req.header('AdminToken')
  if (!provided) {
    return c.json({ error: 'AdminToken header required' }, 401)
  }

  const stored = await getConfig('admin_token')
  if (!stored || provided !== stored) {
    return c.json({ error: 'invalid admin token' }, 401)
  }

  let body: { password?: string }
  try {
    body = await c.req.json()
  } catch {
    return c.json({ error: 'invalid json' }, 400)
  }

  const password = body.password?.trim()
  if (!password || password.length < 8) {
    return c.json({ error: 'password must be at least 8 characters' }, 422)
  }

  const hash = await hashPassword(password)
  await setConfig('password_hash', hash)

  // Revoke all sessions
  await db.deleteFrom('sessions').execute()

  log.info('password reset, all sessions revoked')
  return c.json({ ok: true }, 200)
})

/** Check whether a password has been configured (always true in dev mode). */
export async function isSetupComplete(): Promise<boolean> {
  if (env.AUTH_MODE === 'dev') return true
  const hash = await getConfig('password_hash')
  return !!hash
}
