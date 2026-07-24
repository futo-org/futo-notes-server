import type { Hono } from 'hono'
import type { AuthContext } from './auth/middleware.ts'
import { hashPassword } from './auth/password.ts'
import { FsBlobStore } from './blob/fs.ts'
import { CollectionContents } from './collection-contents/index.ts'
import { db, waitForDb } from './db/connection.ts'
import { migrateToLatest } from './db/migrate.ts'
import { env, validateEnv } from './env.ts'
import { log } from './logger.ts'
import { startBlobMaintenance, type BlobMaintenanceHandle } from './maintenance/blobMaintenance.ts'
import { startSessionReaper, type SessionReaperHandle } from './maintenance/sessionReaper.ts'
import { notifier } from './sync/notifier.ts'

/**
 * Runs CLI-only subcommands (hash, etc.) if argv matches one. Returns true if
 * the process should exit after the call. Called by each entrypoint before
 * runServer so `node dist/index.js hash <pw>` prints the hash and exits.
 */
export async function runCliSubcommand(): Promise<boolean> {
  const sub = process.argv[2]
  if (!sub) return false

  switch (sub) {
    case 'hash': {
      const pw = process.argv[3]
      if (!pw) {
        process.stderr.write('usage: hash <password>\n')
        process.exit(2)
      }
      const hash = await hashPassword(pw)
      process.stdout.write(hash + '\n')
      process.exit(0)
    }
  }
  return false
}

export async function runServer(app: Hono<{ Variables: AuthContext }>, label = 'futo-notes'): Promise<void> {
  validateEnv()
  await waitForDb()
  await migrateToLatest()

  let blobMaintenance: BlobMaintenanceHandle | null = null
  if (env.BLOB_GC_ENABLED) {
    const intervalMs = env.BLOB_GC_INTERVAL_MS
      ? Number(env.BLOB_GC_INTERVAL_MS)
      : undefined
    const contents = new CollectionContents({
      db,
      store: new FsBlobStore(env.BLOB_DIR),
      notifier,
    })
    blobMaintenance = startBlobMaintenance(contents, intervalMs)
  }

  const sessionReaper: SessionReaperHandle = startSessionReaper()

  await notifier.start()

  const server = Bun.serve({ fetch: app.fetch, port: env.PORT, idleTimeout: 0 })
  log.info(`${label} listening on http://localhost:${server.port}`)

  const shutdown = async (signal: string) => {
    log.info(`${signal} received, shutting down...`)
    const timeout = setTimeout(() => {
      log.error('shutdown timed out, forcing exit')
      process.exit(1)
    }, 10000)

    blobMaintenance?.stop()
    sessionReaper.stop()
    await server.stop(true)
    log.info('http server closed')
    await notifier.stop()
    await db.destroy()
    log.info('db pool drained')
    clearTimeout(timeout)
    process.exit(0)
  }

  process.on('SIGTERM', () => shutdown('SIGTERM'))
  process.on('SIGINT', () => shutdown('SIGINT'))
}
