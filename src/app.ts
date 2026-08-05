import { Hono } from 'hono'
import { cors } from 'hono/cors'
import { HTTPException } from 'hono/http-exception'
import pkg from '../package.json' with { type: 'json' }
import { authMiddleware, type AuthContext } from './auth/middleware.ts'
import { passwordRoutes } from './auth/password-routes.ts'
import { authRateLimit } from './auth/rate-limit.ts'
import { authRoutes } from './auth/routes.ts'
import { FsBlobStore } from './blob/fs.ts'
import { createBlobRoutes } from './blobs/routes.ts'
import { CollectionContents } from './collection-contents/index.ts'
import { createCollectionsRoutes } from './collections/routes.ts'
import { db, pool } from './db/connection.ts'
import { env } from './env.ts'
import { log } from './logger.ts'
import { createObjectsRoutes } from './objects/routes.ts'
import { notifier } from './sync/notifier.ts'
import { syncRoutes } from './sync/routes.ts'

const VERSION: string = pkg.version

export function buildApp(): Hono<{ Variables: AuthContext }> {
  const app = new Hono<{ Variables: AuthContext }>()

  // Chunked over-limit uploads surface as a BodyLimitError thrown mid-body-read
  // (hono's bodyLimit errors the request stream; its route-level onError still
  // produces the 413). Handle it here so the default handler doesn't log a stack
  // trace for every oversized upload. The class isn't exported, so match by name.
  app.onError((err, c) => {
    if (err.name === 'BodyLimitError') {
      return c.json({ error: 'blob too large' }, 413)
    }
    if (err instanceof HTTPException) {
      return err.getResponse()
    }
    log.error('unhandled error', { error: String(err) })
    return c.json({ error: 'internal server error' }, 500)
  })

  app.use('*', cors({ origin: '*', allowHeaders: ['Content-Type', 'Authorization', 'Mutation-Id'], allowMethods: ['GET', 'POST', 'PUT', 'DELETE', 'OPTIONS'] }))

  // Rate-limit password login before the (public) auth middleware or the route
  // handler run, so a flood can't brute-force credentials or amplify CPU. Only
  // this path exists in password mode; dev-mode login is passwordless and
  // has no credential to guess, so it is left unlimited.
  if (env.AUTH_MODE === 'password') {
    app.use('/api/auth/password/login', authRateLimit())
  }

  app.get('/', (c) => c.json({
    name: 'futo-notes',
    version: VERSION,
    auth_mode: env.AUTH_MODE,
    signup: 'closed',
    billing: false,
    mutation_ids: {
      supported: true,
      required: false,
      retention_days: 30,
      successful_create_outcomes: 'durable',
    },
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
  const contents = new CollectionContents({ db, store: blobStore, notifier })
  app.route('/api/collections', createCollectionsRoutes(contents))
  app.route('/api/collections', createObjectsRoutes(contents))
  app.route('/api/blobs', createBlobRoutes(blobStore, contents))
  app.route('/api/sync', syncRoutes)

  return app
}
