import { Hono } from 'hono'
import { cors } from 'hono/cors'
import pkg from '../package.json' with { type: 'json' }
import { authMiddleware, type AuthContext } from './auth/middleware.ts'
import { passwordRoutes } from './auth/password-routes.ts'
import { authRoutes } from './auth/routes.ts'
import { FsBlobStore } from './blob/fs.ts'
import { createBlobRoutes } from './blobs/routes.ts'
import { collectionsRoutes } from './collections/routes.ts'
import { pool } from './db/connection.ts'
import { env } from './env.ts'
import { createObjectsRoutes } from './objects/routes.ts'

const VERSION: string = pkg.version

export function buildApp(): Hono<{ Variables: AuthContext }> {
  const app = new Hono<{ Variables: AuthContext }>()

  app.use('*', cors({ origin: '*', allowHeaders: ['Content-Type', 'Authorization'], allowMethods: ['GET', 'POST', 'PUT', 'DELETE', 'OPTIONS'] }))

  app.get('/', (c) => c.json({
    name: 'stonefruit',
    version: VERSION,
    auth_mode: env.AUTH_MODE,
    signup: 'closed',
    billing: false,
  }))

  app.get('/health', async (c) => {
    try {
      const client = await pool.connect()
      client.release()
      return c.json({ status: 'ok', db: 'connected' })
    } catch {
      return c.json({ status: 'degraded', db: 'unreachable' }, 503)
    }
  })

  // Auth middleware applies to every /api/* path; public paths are exempted internally.
  app.use('/api/*', authMiddleware)

  app.route('/api/auth', authRoutes)
  if (env.AUTH_MODE === 'password') {
    app.route('/api/auth/password', passwordRoutes)
  }
  const blobStore = new FsBlobStore(env.BLOB_DIR)
  app.route('/api/collections', collectionsRoutes)
  app.route('/api/collections', createObjectsRoutes(blobStore))
  app.route('/api/blobs', createBlobRoutes(blobStore))

  return app
}
