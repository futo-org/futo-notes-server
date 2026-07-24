import { readFileSync } from 'node:fs'
import type { ConnectionOptions } from 'node:tls'

export const AUTH_MODES = ['dev', 'password'] as const
export type AuthMode = (typeof AUTH_MODES)[number]

const rawAuthMode = process.env.AUTH_MODE ?? 'dev'

function nonEmpty(value: string | undefined): string | undefined {
  return value === undefined || value.length === 0 ? undefined : value
}

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
  // Compose forwards the unused password alternative as an empty string.
  // Normalize blanks so an empty alternative cannot shadow the configured one.
  FUTO_NOTES_PASSWORD: nonEmpty(process.env.FUTO_NOTES_PASSWORD),
  FUTO_NOTES_PASSWORD_HASH: nonEmpty(process.env.FUTO_NOTES_PASSWORD_HASH),
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
  BLOB_GC_INTERVAL_MS: process.env.BLOB_GC_INTERVAL_MS,
  BLOB_GC_ENABLED: process.env.BLOB_GC_ENABLED !== 'false',
  // Max accepted request body size for blob uploads, in bytes (default 100 MiB).
  MAX_BLOB_BYTES: Number(process.env.MAX_BLOB_BYTES ?? 104857600),
  // Complete encoded-response cap for one POST /api/blobs/batch response, in bytes
  // (default 32 MiB). Entries past the cap return status=omitted; the first
  // blob of a response always ships so clients make progress.
  MAX_BATCH_BYTES: Number(process.env.MAX_BATCH_BYTES ?? 33554432),
  // Login rate limiting. Caps password attempts per client per window,
  // blunting brute force and hash-mode CPU amplification. 0 disables.
  AUTH_RATE_LIMIT: Number(process.env.AUTH_RATE_LIMIT ?? 10),
  AUTH_RATE_LIMIT_WINDOW_MS: Number(process.env.AUTH_RATE_LIMIT_WINDOW_MS ?? 60000),
  // Trust X-Forwarded-For for the client IP — enable only behind a reverse
  // proxy you control that sets it (otherwise clients can spoof the header to
  // dodge the limit). Off → the limiter keys on the socket peer address.
  TRUST_PROXY: process.env.TRUST_PROXY === 'true',
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
  if (env.AUTH_MODE === 'password' && env.FUTO_NOTES_PASSWORD && env.FUTO_NOTES_PASSWORD_HASH) {
    throw new Error('Set only one of FUTO_NOTES_PASSWORD or FUTO_NOTES_PASSWORD_HASH when AUTH_MODE=password')
  }
  if (env.AUTH_MODE === 'password' && !env.FUTO_NOTES_PASSWORD && !env.FUTO_NOTES_PASSWORD_HASH) {
    throw new Error('FUTO_NOTES_PASSWORD or FUTO_NOTES_PASSWORD_HASH is required when AUTH_MODE=password')
  }
  if (!Number.isSafeInteger(env.MAX_BLOB_BYTES) || env.MAX_BLOB_BYTES < 1 || env.MAX_BLOB_BYTES > 0xffff_ffff) {
    throw new Error('MAX_BLOB_BYTES must be a positive integer no greater than 4294967295 (bytes)')
  }
  if (!Number.isSafeInteger(env.MAX_BATCH_BYTES) || env.MAX_BATCH_BYTES < 1) {
    throw new Error('MAX_BATCH_BYTES must be a positive integer (bytes)')
  }
  if (!Number.isFinite(env.AUTH_RATE_LIMIT) || env.AUTH_RATE_LIMIT < 0) {
    throw new Error('AUTH_RATE_LIMIT must be a non-negative number (0 disables rate limiting)')
  }
  if (!Number.isFinite(env.AUTH_RATE_LIMIT_WINDOW_MS) || env.AUTH_RATE_LIMIT_WINDOW_MS < 1) {
    throw new Error('AUTH_RATE_LIMIT_WINDOW_MS must be a positive number of milliseconds')
  }
}
