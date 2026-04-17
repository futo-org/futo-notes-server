import { Hono } from 'hono'
import { cors } from 'hono/cors'
import { authMiddleware, type AuthContext } from './auth/middleware.ts'
import { authRoutes } from './auth/routes.ts'
import { isSetupComplete, signupRoutes } from './auth/signup-routes.ts'
import { FsBlobStore } from './blob/fs.ts'
import { createBlobRoutes } from './blobs/routes.ts'
import { collectionsRoutes } from './collections/routes.ts'
import { pool } from './db/connection.ts'
import { env } from './env.ts'
import { objectsRoutes } from './objects/routes.ts'
import { startRoutes } from './routes/start.ts'

export function buildApp(): Hono<{ Variables: AuthContext }> {
  const app = new Hono<{ Variables: AuthContext }>()

  app.use('*', cors({ origin: '*', allowHeaders: ['Content-Type', 'Authorization'], allowMethods: ['GET', 'POST', 'PUT', 'DELETE', 'OPTIONS'] }))

  app.get('/', (c) => c.text('stonefruit server\n'))

  app.get('/health', async (c) => {
    try {
      const client = await pool.connect()
      client.release()
      const setupComplete = await isSetupComplete()
      return c.json({ status: 'ok', db: 'connected', setup_complete: setupComplete })
    } catch {
      return c.json({ status: 'degraded', db: 'unreachable', setup_complete: false }, 503)
    }
  })

  // /start is a public HTML signup page.
  app.route('/', startRoutes)

  // Auth middleware applies to every /api/* path; public paths are exempted internally.
  app.use('/api/*', authMiddleware)

  // Signup + login routes (paths start with /api — they'll pass through middleware
  // as public routes).
  app.route('/', signupRoutes)

  app.route('/api/auth', authRoutes)
  app.route('/api/collections', collectionsRoutes)
  app.route('/api/collections', objectsRoutes)
  app.route('/api/blobs', createBlobRoutes(new FsBlobStore(env.BLOB_DIR)))

  return app
}
