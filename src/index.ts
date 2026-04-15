import { serve } from '@hono/node-server'
import { Hono } from 'hono'
import { cors } from 'hono/cors'
import { authMiddleware, type AuthContext } from './auth/middleware.ts'
import { authRoutes } from './auth/routes.ts'
import { isSetupComplete, signupRoutes } from './auth/signup-routes.ts'
import { FsBlobStore } from './blob/fs.ts'
import { createBlobRoutes } from './blobs/routes.ts'
import { collectionsRoutes } from './collections/routes.ts'
import { db, pool, waitForDb } from './db/connection.ts'
import { migrateToLatest } from './db/migrate.ts'
import { env } from './env.ts'
import { log } from './logger.ts'
import { objectsRoutes } from './objects/routes.ts'
import { startRoutes } from './routes/start.ts'

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

export { app }

async function main() {
  await waitForDb()
  await migrateToLatest()

  const server = serve({ fetch: app.fetch, port: env.PORT }, (info) => {
    log.info(`stonefruit listening on http://localhost:${info.port}`)
  })

  const shutdown = async (signal: string) => {
    log.info(`${signal} received, shutting down...`)
    const timeout = setTimeout(() => {
      log.error('shutdown timed out, forcing exit')
      process.exit(1)
    }, 10000)

    server.close(() => {
      log.info('http server closed')
    })
    await db.destroy()
    log.info('db pool drained')
    clearTimeout(timeout)
    process.exit(0)
  }

  process.on('SIGTERM', () => shutdown('SIGTERM'))
  process.on('SIGINT', () => shutdown('SIGINT'))
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main().catch((err) => {
    log.error('fatal startup error', { error: String(err) })
    process.exit(1)
  })
}
