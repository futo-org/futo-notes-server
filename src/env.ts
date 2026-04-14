import 'dotenv/config'

function required(name: string): string {
  const v = process.env[name]
  if (!v) throw new Error(`Missing required env var: ${name}`)
  return v
}

export const env = {
  DATABASE_URL: required('DATABASE_URL'),
  PORT: Number(process.env.PORT ?? 3000),
  BLOB_DIR: process.env.BLOB_DIR ?? './blobs',
  LOG_LEVEL: (process.env.LOG_LEVEL ?? 'info') as 'debug' | 'info' | 'warn' | 'error',
  AUTH_MODE: (process.env.AUTH_MODE ?? 'dev') as 'dev' | 'password',
  DB_POOL_MAX: Number(process.env.DB_POOL_MAX ?? 10),
  DB_POOL_IDLE_TIMEOUT_MS: Number(process.env.DB_POOL_IDLE_TIMEOUT_MS ?? 10000),
  DB_SSL: process.env.DB_SSL === 'true',
}
