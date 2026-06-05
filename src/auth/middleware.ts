import type { MiddlewareHandler } from 'hono'
import { getCookie, setCookie } from 'hono/cookie'
import { env } from '../env.ts'
import { SESSION_TTL_MS, validateSession } from './session.ts'

/** Paths only public in dev auth mode. */
const DEV_PUBLIC_PATHS = new Set<string>([
  '/api/auth/dev/login',
])

/** Paths only public in password auth mode. */
const PASSWORD_PUBLIC_PATHS = new Set<string>([
  '/api/auth/password/login',
])

export interface AuthContext {
  user: { id: string; email: string; name: string }
  sessionId: string
}

export const authMiddleware: MiddlewareHandler<{ Variables: AuthContext }> = async (c, next) => {
  const path = c.req.path
  if (env.AUTH_MODE === 'dev' && DEV_PUBLIC_PATHS.has(path)) return next()
  if (env.AUTH_MODE === 'password' && PASSWORD_PUBLIC_PATHS.has(path)) return next()

  const cookieToken = getCookie(c, 'session')
  const token = cookieToken
    ?? c.req.header('Authorization')?.replace(/^Bearer\s+/i, '')
    ?? null
  if (!token) {
    return c.json({ error: 'unauthorized' }, 401)
  }

  const session = await validateSession(token)
  if (!session) {
    return c.json({ error: 'unauthorized' }, 401)
  }

  if (session.renewed && cookieToken) {
    setCookie(c, 'session', cookieToken, {
      httpOnly: true,
      secure: env.COOKIE_SECURE,
      sameSite: 'Lax',
      path: '/',
      maxAge: Math.floor(SESSION_TTL_MS / 1000),
    })
  }

  c.set('user', session.user)
  c.set('sessionId', session.sessionId)
  return next()
}
