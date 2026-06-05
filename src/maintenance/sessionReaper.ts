import { db } from '../db/connection.ts'
import { log } from '../logger.ts'

// Expired sessions are otherwise only deleted lazily, when their exact token
// is presented again (see validateSession). Abandoned tokens never come back,
// so this sweep clears them off the highest-QPS table. The DELETE is
// idempotent, so concurrent instances racing on it is harmless.
export async function runSessionReaperOnce(): Promise<{ deleted: number }> {
  const result = await db
    .deleteFrom('sessions')
    .where('expires_at', '<', new Date())
    .executeTakeFirst()

  const deleted = Number(result.numDeletedRows ?? 0)

  if (deleted > 0) {
    log.info('session-reaper: deleted expired sessions', { count: deleted })
  }

  return { deleted }
}

export interface SessionReaperHandle {
  stop(): void
}

// Start a background loop that runs the reaper once shortly after startup (so
// it doesn't compete with server boot), then every intervalMs.
export function startSessionReaper(
  intervalMs: number = 60 * 60 * 1000,
): SessionReaperHandle {
  let stopped = false
  let timer: NodeJS.Timeout | null = null

  const tick = async () => {
    if (stopped) return
    try {
      await runSessionReaperOnce()
    } catch (err) {
      log.error('session-reaper: cycle failed', { error: String(err) })
    }
    if (!stopped) timer = setTimeout(tick, intervalMs)
  }

  timer = setTimeout(tick, 60 * 1000)

  return {
    stop(): void {
      stopped = true
      if (timer) clearTimeout(timer)
      timer = null
    },
  }
}
