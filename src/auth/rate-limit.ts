import type { Context, MiddlewareHandler } from 'hono'
import { env } from '../env.ts'
import { log } from '../logger.ts'

/**
 * In-memory fixed-window rate limiter for the unauthenticated login path.
 *
 * `POST /api/auth/password/login` runs scrypt (~tens of ms, ~16 MB) on every
 * request and guards a single guessable secret, so it is both a brute-force
 * target and a CPU-amplification vector. This caps attempts per client per
 * window so a self-hosted deployment is protected out of the box, without the
 * operator having to add a reverse-proxy rule.
 *
 * The state is per-instance and intentionally soft: a restart resets the
 * counters, and a multi-instance deployment limits per instance (each instance
 * still bounds its own scrypt load). A shared store (Redis/Valkey) is the
 * scale-up path — the same trajectory sessions take in DESIGN.md — but is
 * unnecessary for the single-instance self-hosted target this protects.
 */

interface Bucket {
  count: number
  resetAt: number
}

const buckets = new Map<string, Bucket>()

// When the map grows past this, drop expired entries opportunistically. A
// background timer would dangle in tests that call app.fetch() without ever
// starting a server, so cleanup is inline instead.
const SWEEP_THRESHOLD = 10_000

function sweepExpired(now: number): void {
  for (const [key, bucket] of buckets) {
    if (now >= bucket.resetAt) buckets.delete(key)
  }
}

/**
 * The client identity to key the limiter on: a trusted X-Forwarded-For entry
 * when running behind a proxy, otherwise the socket peer address. When neither
 * is available (e.g. tests calling app.fetch() with no server, or a proxy that
 * doesn't set XFF while TRUST_PROXY is off) all callers share one bucket, which
 * still bounds total scrypt load on the login path.
 */
function clientKey(c: Context): string {
  if (env.TRUST_PROXY) {
    const forwarded = c.req.header('x-forwarded-for')
    if (forwarded) {
      const first = forwarded.split(',')[0]?.trim()
      if (first) return first
    }
  }

  // Under Bun.serve, Hono receives the Bun Server as the second fetch arg,
  // exposed as c.env; server.requestIP(req) gives the peer address. Guarded
  // because c.env is undefined when the app is driven via app.fetch() directly.
  try {
    const maybeServer = c.env as
      | {
          requestIP?: (req: Request) => { address?: string } | null
          server?: { requestIP?: (req: Request) => { address?: string } | null }
        }
      | undefined
    const server = maybeServer?.server ?? maybeServer
    const info = server?.requestIP?.(c.req.raw)
    if (info?.address) return info.address
  } catch {
    // fall through to the shared bucket
  }

  return 'unknown'
}

/**
 * Build a middleware that rejects with 429 once a client exceeds
 * `AUTH_RATE_LIMIT` requests within `AUTH_RATE_LIMIT_WINDOW_MS`. A limit of 0
 * disables it (the middleware becomes a pass-through). The limit/window are
 * read once at construction.
 */
export function authRateLimit(): MiddlewareHandler {
  const limit = env.AUTH_RATE_LIMIT
  const windowMs = env.AUTH_RATE_LIMIT_WINDOW_MS

  if (limit <= 0) {
    return (_c, next) => next()
  }

  return async (c, next) => {
    const now = Date.now()
    const key = clientKey(c)

    let bucket = buckets.get(key)
    if (!bucket || now >= bucket.resetAt) {
      bucket = { count: 0, resetAt: now + windowMs }
      buckets.set(key, bucket)
    }

    bucket.count++

    if (bucket.count > limit) {
      const retryAfter = Math.max(1, Math.ceil((bucket.resetAt - now) / 1000))
      log.warn('auth rate limit exceeded', { key, count: bucket.count })
      c.header('Retry-After', String(retryAfter))
      return c.json({ error: 'too many requests' }, 429)
    }

    if (buckets.size > SWEEP_THRESHOLD) sweepExpired(now)

    return next()
  }
}

/** Test-only: clear all rate-limit state. */
export function _resetRateLimit(): void {
  buckets.clear()
}
