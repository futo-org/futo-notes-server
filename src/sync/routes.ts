import { Hono } from 'hono'
import { streamSSE } from 'hono/streaming'
import type { AuthContext } from '../auth/middleware.ts'
import { log } from '../logger.ts'
import { notifier } from './notifier.ts'

const HEARTBEAT_MS = 25_000

export const syncRoutes = new Hono<{ Variables: AuthContext }>()

// SSE stream of change events for the authenticated user. Clients should
// treat each `change` event as a hint to call GET /api/collections/:id/objects
// with their last-seen cursor — the event itself only carries routing
// information (collectionId + currentVersion), never object content.
syncRoutes.get('/events', (c) => {
  const userId = c.var.user.id

  // Disable buffering for nginx / similar proxies that don't auto-detect SSE.
  // Cache-Control: no-cache is set by streamSSE itself.
  c.header('X-Accel-Buffering', 'no')

  return streamSSE(c, async (stream) => {
    let aborted = false
    let wake: (() => void) | null = null

    stream.onAbort(() => {
      aborted = true
      wake?.()
    })

    // These are doorbell events — the client reacts to any `change` by pulling
    // from its own cursor, so only the latest version per collection matters.
    // Coalesce into a map (collectionId → newest version) instead of an
    // unbounded queue, bounding memory under a slow reader / fast writer.
    const pending = new Map<string, number>()
    const unsubscribe = notifier.subscribe(userId, (event) => {
      const prev = pending.get(event.collectionId)
      if (prev === undefined || event.currentVersion > prev) {
        pending.set(event.collectionId, event.currentVersion)
      }
      wake?.()
    })

    // The upstream LISTEN connection dropped — we can no longer guarantee
    // delivery. End the stream so the client reconnects and re-pulls; sitting
    // open while emitting only heartbeats would falsely look healthy.
    const unsubscribeLost = notifier.onListenerLost(() => {
      aborted = true
      wake?.()
    })

    try {
      await stream.writeSSE({ event: 'ready', data: '' })

      while (!aborted) {
        while (!aborted && pending.size > 0) {
          // Snapshot and clear so events arriving mid-write aren't lost; they
          // land in a fresh map and flush on the next pass.
          const batch = [...pending]
          pending.clear()
          for (const [collectionId, currentVersion] of batch) {
            if (aborted) break
            await stream.writeSSE({
              event: 'change',
              data: JSON.stringify({ collectionId, currentVersion }),
            })
          }
        }
        if (aborted) break

        let timer: ReturnType<typeof setTimeout> | undefined
        await new Promise<void>((resolve) => {
          wake = () => {
            if (timer) clearTimeout(timer)
            resolve()
          }
          timer = setTimeout(resolve, HEARTBEAT_MS)
        })
        wake = null

        if (aborted) break
        if (pending.size === 0) {
          await stream.writeSSE({ event: 'ping', data: '' })
        }
      }
    } catch (err) {
      log.debug('sync stream closed', { err: String(err) })
    } finally {
      unsubscribe()
      unsubscribeLost()
    }
  })
})
