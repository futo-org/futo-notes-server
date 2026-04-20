import { sql } from 'kysely'
import type { BlobStore } from '../blob/interface.ts'
import { db } from '../db/connection.ts'
import { env } from '../env.ts'
import { log } from '../logger.ts'

// Orphaned blobs are retained for this many days so that a client performing
// a three-way merge can still fetch the common ancestor by its blobKey. One
// year is plenty — conflict resolution happens during sync, and any device
// that hasn't synced in a year has bigger problems than a merge ancestor.
const DEFAULT_RETENTION_DAYS = 365

const BATCH_SIZE = 500

export function getRetentionDays(): number {
  const raw = Number(env.BLOB_RETENTION_DAYS)
  if (!Number.isFinite(raw) || raw <= 0) return DEFAULT_RETENTION_DAYS
  return raw
}

export async function runBlobGcOnce(
  store: BlobStore,
  retentionDays: number = getRetentionDays(),
): Promise<{ deleted: number; bytes: number }> {
  const cutoff = sql<Date>`now() - ${sql.lit(`${retentionDays} days`)}::interval`

  let totalDeleted = 0
  let totalBytes = 0

  // Page through expired rows so one oversized run can't hold a big result
  // set in memory or a long transaction open.
  while (true) {
    const batch = await db
      .selectFrom('orphaned_blobs')
      .where('orphaned_at', '<', cutoff)
      .select(['blob_key', 'size_bytes'])
      .limit(BATCH_SIZE)
      .execute()

    if (batch.length === 0) break

    for (const row of batch) {
      try {
        await store.delete(row.blob_key)
      } catch (err) {
        log.warn('blob-gc: store.delete failed', {
          blobKey: row.blob_key,
          error: String(err),
        })
        // Leave the ledger row in place; we'll retry next cycle.
        continue
      }
      await db
        .deleteFrom('orphaned_blobs')
        .where('blob_key', '=', row.blob_key)
        .execute()
      totalDeleted += 1
      totalBytes += Number(row.size_bytes ?? 0)
    }
  }

  if (totalDeleted > 0) {
    log.info('blob-gc: deleted orphaned blobs', {
      count: totalDeleted,
      bytes: totalBytes,
      retentionDays,
    })
  }

  return { deleted: totalDeleted, bytes: totalBytes }
}

export interface BlobGcHandle {
  stop(): void
}

// Start a background loop that runs the GC once immediately (after a short
// delay so it doesn't compete with server startup), then every intervalMs.
export function startBlobGc(
  store: BlobStore,
  intervalMs: number = 6 * 60 * 60 * 1000,
): BlobGcHandle {
  let stopped = false
  let timer: NodeJS.Timeout | null = null

  const tick = async () => {
    if (stopped) return
    try {
      await runBlobGcOnce(store)
    } catch (err) {
      log.error('blob-gc: cycle failed', { error: String(err) })
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
