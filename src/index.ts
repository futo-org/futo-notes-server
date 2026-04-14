import { serve } from '@hono/node-server'
import { Hono } from 'hono'
import { authMiddleware, type AuthContext } from './auth/middleware.ts'
import { authRoutes } from './auth/routes.ts'
import { FsBlobStore } from './blob/fs.ts'
import { createBlobRoutes } from './blobs/routes.ts'
import { collectionsRoutes } from './collections/routes.ts'
import { waitForDb } from './db/connection.ts'
import { migrateToLatest } from './db/migrate.ts'
import { env } from './env.ts'
import { objectsRoutes } from './objects/routes.ts'

const app = new Hono<{ Variables: AuthContext }>()

app.get('/', (c) => c.text('stonefruit server\n'))

// Auth middleware applies to every /api/* path; it exempts /api/auth/dev/login internally.
app.use('/api/*', authMiddleware)

app.route('/api/auth', authRoutes)
app.route('/api/collections', collectionsRoutes)
app.route('/api/collections', objectsRoutes)
app.route('/api/blobs', createBlobRoutes(new FsBlobStore(env.BLOB_DIR)))

async function main() {
  await waitForDb()
  await migrateToLatest()
  serve({ fetch: app.fetch, port: env.PORT }, (info) => {
    console.log(`stonefruit listening on http://localhost:${info.port}`)
  })
}

main().catch((err) => {
  console.error(err)
  process.exit(1)
})
