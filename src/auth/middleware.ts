import type { MiddlewareHandler } from 'hono'
import { getCookie } from 'hono/cookie'
import { env } from '../env.ts'
import { validateSession } from './session.ts'

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
    // Distinguish a dead bearer session from a request that supplied no
    // credentials. Clients that securely retain login material can use this
    // stable signal to obtain a fresh token without treating the account
    // password as changed or wiping their sync cursor.
    c.header('WWW-Authenticate', 'Bearer realm="futo-notes", error="invalid_token"')
    return c.json({ error: 'session expired or invalid', code: 'invalid_session' }, 401)
  }

  c.set('user', session.user)
  c.set('sessionId', session.sessionId)
  return next()
}
