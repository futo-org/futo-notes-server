import { Hono } from 'hono'
import { deleteCookie, setCookie } from 'hono/cookie'
import { uuidv7 } from 'uuidv7'
import { db } from '../db/connection.ts'
import { env } from '../env.ts'
import type { AuthContext } from './middleware.ts'
import { createSession, destroySession } from './session.ts'

const SESSION_MAX_AGE_SECONDS = 7 * 24 * 60 * 60

export const authRoutes = new Hono<{ Variables: AuthContext }>()

/**
 * POC-only. Upserts a user by email and opens a session.
 * Only available when AUTH_MODE=dev.
 */
if (env.AUTH_MODE === 'dev') {
  authRoutes.post('/dev/login', async (c) => {
    let body: { email?: string; name?: string }
    try {
      body = await c.req.json()
    } catch {
      return c.json({ error: 'invalid json' }, 400)
    }
    const email = body.email?.trim().toLowerCase()
    const name = body.name?.trim() || email?.split('@')[0]
    if (!email || !name) {
      return c.json({ error: 'email is required' }, 400)
    }

    const sub = `dev:${email}`

    const user = await db
      .insertInto('users')
      .values({ id: uuidv7(), sub, name, email })
      .onConflict((oc) => oc.column('email').doUpdateSet({ name }))
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
}

authRoutes.get('/', (c) => {
  return c.json({ user: c.var.user })
})

authRoutes.post('/logout', async (c) => {
  await destroySession(c.var.sessionId)
  deleteCookie(c, 'session', { path: '/' })
  return c.body(null, 204)
})
