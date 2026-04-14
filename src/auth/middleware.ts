import type { MiddlewareHandler } from 'hono'
import { getCookie } from 'hono/cookie'
import { env } from '../env.ts'
import { validateSession } from './session.ts'

/** Paths that never require authentication. */
const PUBLIC_PATHS = new Set<string>([
  '/api/auth/login',
])

/** Paths only public in dev auth mode. */
const DEV_PUBLIC_PATHS = new Set<string>([
  '/api/auth/dev/login',
])

export interface AuthContext {
  user: { id: string; email: string; name: string }
  sessionId: string
}

export const authMiddleware: MiddlewareHandler<{ Variables: AuthContext }> = async (c, next) => {
  const path = c.req.path
  if (PUBLIC_PATHS.has(path)) return next()
  if (env.AUTH_MODE === 'dev' && DEV_PUBLIC_PATHS.has(path)) return next()

  const token = getCookie(c, 'session')
    ?? c.req.header('Authorization')?.replace(/^Bearer\s+/i, '')
    ?? null
  if (!token) {
    return c.json({ error: 'unauthorized' }, 401)
  }

  const session = await validateSession(token)
  if (!session) {
    return c.json({ error: 'unauthorized' }, 401)
  }

  c.set('user', session.user)
  c.set('sessionId', session.sessionId)
  return next()
}
