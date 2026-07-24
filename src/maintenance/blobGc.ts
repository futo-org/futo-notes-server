import type { CollectionContents } from '../collection-contents/index.ts'
import { log } from '../logger.ts'

export type BlobMaintenanceResult = Awaited<
  ReturnType<CollectionContents['runMaintenance']>
>

export async function runBlobGcOnce(
  contents: CollectionContents,
): Promise<BlobMaintenanceResult> {
  const result = await contents.runMaintenance()
  const deleted = result.stagedDeleted
    + result.retainedDeleted
    + result.purgeableDeleted
  if (deleted > 0 || result.reconciled > 0 || result.mutationResultsDeleted > 0) {
    log.info('blob-maintenance: cycle completed', {
      ...result,
      deleted,
    })
  }
  return result
}

export interface BlobGcHandle {
  stop(): void
}

// Start a background loop that runs once after startup, then periodically.
// Lifetime policy belongs to CollectionContents; this file only schedules it.
export function startBlobGc(
  contents: CollectionContents,
  intervalMs: number = 6 * 60 * 60 * 1000,
): BlobGcHandle {
  let stopped = false
  let timer: NodeJS.Timeout | null = null

  const tick = async () => {
    if (stopped) return
    try {
      await runBlobGcOnce(contents)
    } catch (err) {
      log.error('blob-maintenance: cycle failed', { error: String(err) })
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
