import type { CollectionContents } from '../collection-contents/index.ts'
import { log } from '../logger.ts'

export type BlobMaintenanceResult = Awaited<
  ReturnType<CollectionContents['runMaintenance']>
>

export interface BlobMaintenanceOptions {
  // Gates blob-byte/ledger garbage collection only. Reconciliation and the
  // fixed Mutation-ID expiry policy always run.
  collectGarbage: boolean
}

export async function runBlobMaintenanceOnce(
  contents: CollectionContents,
  options: BlobMaintenanceOptions = { collectGarbage: true },
): Promise<BlobMaintenanceResult> {
  const { reconciled } = await contents.reconcileStorage()
  const mutationResultsDeleted = await contents.expireMutationResults()
  const result = {
    reconciled,
    ...(options.collectGarbage
      ? await contents.collectGarbage()
      : {
        stagedDeleted: 0,
        retainedDeleted: 0,
        purgeableDeleted: 0,
      }),
    mutationResultsDeleted,
  }
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

export interface BlobMaintenanceHandle {
  stop(): void
}

// Start a background loop that runs once after startup, then periodically.
// Lifetime policy belongs to CollectionContents; this file only schedules it.
export function startBlobMaintenance(
  contents: CollectionContents,
  intervalMs: number = 6 * 60 * 60 * 1000,
  options: BlobMaintenanceOptions = { collectGarbage: true },
): BlobMaintenanceHandle {
  let stopped = false
  let timer: NodeJS.Timeout | null = null

  const tick = async () => {
    if (stopped) return
    try {
      await runBlobMaintenanceOnce(contents, options)
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
