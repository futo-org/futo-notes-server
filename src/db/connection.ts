import { Kysely, PostgresDialect } from 'kysely'
import pg from 'pg'
import { env } from '../env.ts'
import { log } from '../logger.ts'
import type { Database } from './types.ts'

export const pool = new pg.Pool({
  connectionString: env.DATABASE_URL,
  max: env.DB_POOL_MAX,
  idleTimeoutMillis: env.DB_POOL_IDLE_TIMEOUT_MS,
  ssl: env.DB_SSL_OPTIONS,
})

export const db = new Kysely<Database>({
  dialect: new PostgresDialect({ pool }),
})

/**
 * Tries to connect to Postgres with a short retry loop so the server can
 * start while the container is still coming up.
 */
export async function waitForDb(attempts = 5, delayMs = 1000): Promise<void> {
  let lastErr: unknown
  for (let i = 0; i < attempts; i++) {
    try {
      const client = await pool.connect()
      client.release()
      log.info('connected to postgres')
      return
    } catch (err) {
      lastErr = err
      log.warn('postgres not ready, retrying...', { attempt: i + 1 })
      await new Promise((r) => setTimeout(r, delayMs))
    }
  }
  throw new Error(`Could not connect to Postgres after ${attempts} attempts: ${lastErr}`)
}
