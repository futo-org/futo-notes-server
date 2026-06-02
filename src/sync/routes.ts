import { Hono } from 'hono'
import { streamSSE } from 'hono/streaming'
import type { AuthContext } from '../auth/middleware.ts'
import { log } from '../logger.ts'
import { notifier, type ChangeEvent } from './notifier.ts'

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

    const queue: ChangeEvent[] = []
    const unsubscribe = notifier.subscribe(userId, (event) => {
      queue.push(event)
      wake?.()
    })

    try {
      await stream.writeSSE({ event: 'ready', data: '' })

      while (!aborted) {
        while (!aborted && queue.length > 0) {
          const event = queue.shift()!
          await stream.writeSSE({
            event: 'change',
            data: JSON.stringify({
              collectionId: event.collectionId,
              currentVersion: event.currentVersion,
            }),
          })
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
        if (queue.length === 0) {
          await stream.writeSSE({ event: 'ping', data: '' })
        }
      }
    } catch (err) {
      log.debug('sync stream closed', { err: String(err) })
    } finally {
      unsubscribe()
    }
  })
})
