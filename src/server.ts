import { serve } from '@hono/node-server'
import type { Hono } from 'hono'
import type { AuthContext } from './auth/middleware.ts'
import { db, waitForDb } from './db/connection.ts'
import { migrateToLatest } from './db/migrate.ts'
import { env, validateEnv } from './env.ts'
import { log } from './logger.ts'

export async function runServer(app: Hono<{ Variables: AuthContext }>, label = 'stonefruit'): Promise<void> {
  validateEnv()
  await waitForDb()
  await migrateToLatest()

  const server = serve({ fetch: app.fetch, port: env.PORT }, (info) => {
    log.info(`${label} listening on http://localhost:${info.port}`)
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
