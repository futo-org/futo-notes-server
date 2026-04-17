import 'dotenv/config'

function required(name: string): string {
  const v = process.env[name]
  if (!v) throw new Error(`Missing required env var: ${name}`)
  return v
}

export const AUTH_MODES = ['dev', 'password'] as const
export type AuthMode = (typeof AUTH_MODES)[number]

const rawAuthMode = process.env.AUTH_MODE ?? 'dev'

export const env = {
  DATABASE_URL: required('DATABASE_URL'),
  PORT: Number(process.env.PORT ?? 3000),
  BLOB_DIR: process.env.BLOB_DIR ?? './blobs',
  LOG_LEVEL: (process.env.LOG_LEVEL ?? 'info') as 'debug' | 'info' | 'warn' | 'error',
  AUTH_MODE: rawAuthMode as AuthMode,
  STONEFRUIT_PASSWORD_HASH: process.env.STONEFRUIT_PASSWORD_HASH,
  DB_POOL_MAX: Number(process.env.DB_POOL_MAX ?? 10),
  DB_POOL_IDLE_TIMEOUT_MS: Number(process.env.DB_POOL_IDLE_TIMEOUT_MS ?? 10000),
  DB_SSL: process.env.DB_SSL === 'true',
}

export function validateEnv(): void {
  if (!(AUTH_MODES as readonly string[]).includes(rawAuthMode)) {
    throw new Error(`Invalid AUTH_MODE=${rawAuthMode}. Valid: ${AUTH_MODES.join(', ')}`)
  }
  if (env.AUTH_MODE === 'password' && !env.STONEFRUIT_PASSWORD_HASH) {
    throw new Error('STONEFRUIT_PASSWORD_HASH is required when AUTH_MODE=password')
  }
}
