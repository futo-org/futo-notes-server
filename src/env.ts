import 'dotenv/config'
import { readFileSync } from 'node:fs'
import type { ConnectionOptions } from 'node:tls'

export const AUTH_MODES = ['dev', 'password'] as const
export type AuthMode = (typeof AUTH_MODES)[number]

const rawAuthMode = process.env.AUTH_MODE ?? 'dev'

const dbSsl = process.env.DB_SSL === 'true'
const dbSslInsecure = process.env.DB_SSL_INSECURE === 'true'
const dbSslCaPath = process.env.DB_SSL_CA

let dbSslCaError: Error | null = null

/**
 * SSL options for the pg pool and the dedicated LISTEN connection. Built once
 * at import so both consumers share the exact same config. When DB_SSL is off
 * this is `false` (no TLS). When on, certificates are verified by default;
 * DB_SSL_CA supplies a private/self-signed CA, and DB_SSL_INSECURE explicitly
 * opts out of verification.
 */
function buildDbSslOptions(): boolean | ConnectionOptions {
  if (!dbSsl) return false
  const options: ConnectionOptions = { rejectUnauthorized: !dbSslInsecure }
  if (dbSslCaPath) {
    try {
      options.ca = readFileSync(dbSslCaPath, 'utf8')
    } catch (err) {
      // Deferred to validateEnv() so CLI subcommands (e.g. `hash`) that never
      // touch the DB still run when the CA file is missing or unreadable.
      dbSslCaError = new Error(`Could not read DB_SSL_CA file at ${dbSslCaPath}: ${err}`)
    }
  }
  return options
}

// Reads at import time. Validation happens in validateEnv(), called by
// runServer() — keeps CLI subcommands (e.g. `hash`) runnable without a
// full server env (no DATABASE_URL required).
export const env = {
  DATABASE_URL: process.env.DATABASE_URL ?? '',
  PORT: Number(process.env.PORT ?? 3000),
  BLOB_DIR: process.env.BLOB_DIR ?? './blobs',
  LOG_LEVEL: (process.env.LOG_LEVEL ?? 'info') as 'debug' | 'info' | 'warn' | 'error',
  AUTH_MODE: rawAuthMode as AuthMode,
  FUTO_NOTES_PASSWORD_HASH: process.env.FUTO_NOTES_PASSWORD_HASH,
  // Secure flag on the session cookie. Defaults true except in dev mode (so
  // localhost HTTP dev keeps working). Explicit true/false overrides the default.
  COOKIE_SECURE: process.env.COOKIE_SECURE !== undefined
    ? process.env.COOKIE_SECURE === 'true'
    : rawAuthMode !== 'dev',
  DB_POOL_MAX: Number(process.env.DB_POOL_MAX ?? 10),
  DB_POOL_IDLE_TIMEOUT_MS: Number(process.env.DB_POOL_IDLE_TIMEOUT_MS ?? 10000),
  DB_SSL: dbSsl,
  // Shared SSL config for the pg pool and the LISTEN connection (see above).
  DB_SSL_OPTIONS: buildDbSslOptions(),
  BLOB_RETENTION_DAYS: process.env.BLOB_RETENTION_DAYS,
  BLOB_GC_INTERVAL_MS: process.env.BLOB_GC_INTERVAL_MS,
  BLOB_GC_ENABLED: process.env.BLOB_GC_ENABLED !== 'false',
  // Max accepted request body size for blob uploads, in bytes (default 100 MiB).
  MAX_BLOB_BYTES: Number(process.env.MAX_BLOB_BYTES ?? 104857600),
}

export function validateEnv(): void {
  if (!env.DATABASE_URL) {
    throw new Error('DATABASE_URL is required')
  }
  if (dbSslCaError) {
    throw dbSslCaError
  }
  if (!(AUTH_MODES as readonly string[]).includes(rawAuthMode)) {
    throw new Error(`Invalid AUTH_MODE=${rawAuthMode}. Valid: ${AUTH_MODES.join(', ')}`)
  }
  if (env.AUTH_MODE === 'password' && !env.FUTO_NOTES_PASSWORD_HASH) {
    throw new Error('FUTO_NOTES_PASSWORD_HASH is required when AUTH_MODE=password')
  }
  if (!Number.isSafeInteger(env.MAX_BLOB_BYTES) || env.MAX_BLOB_BYTES < 1) {
    throw new Error('MAX_BLOB_BYTES must be a positive integer (bytes)')
  }
}
