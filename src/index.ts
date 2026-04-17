import { serve } from '@hono/node-server'
import { buildApp } from './app.ts'
import { db, waitForDb } from './db/connection.ts'
import { migrateToLatest } from './db/migrate.ts'
import { env } from './env.ts'
import { log } from './logger.ts'

async function main() {
  await waitForDb()
  await migrateToLatest()

  const app = buildApp()
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
