import type { MiddlewareHandler } from 'hono'
import { getCookie } from 'hono/cookie'
import { validateSession } from './session.ts'

/** Paths under /api/* that do not require authentication. */
const PUBLIC_PATHS = new Set<string>(['/api/auth/dev/login'])

export interface AuthContext {
  user: { id: string; email: string; name: string }
  sessionId: string
}

export const authMiddleware: MiddlewareHandler<{ Variables: AuthContext }> = async (c, next) => {
  if (PUBLIC_PATHS.has(c.req.path)) {
    return next()
  }

  const token = getCookie(c, 'session')
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
