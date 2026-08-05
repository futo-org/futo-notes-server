import { Hono, type Context } from 'hono'
import { bodyLimit } from 'hono/body-limit'
import type { AuthContext } from '../../auth/middleware.ts'
import type { CollectionContents } from '../../collection-contents/index.ts'
import { env } from '../../env.ts'
import { log } from '../../logger.ts'
import { collectionBelongsToUser } from '../collection-ownership.ts'
import { applyBatchUploadEntry, type BatchUploadResult } from './apply-entry.ts'
import { parseBatchUploadEntries, type BatchUploadEntry } from './frames.ts'

const batchUploadLimit = bodyLimit({
  maxSize: env.MAX_BATCH_BYTES,
  onError: (c) => c.json({ error: 'request too large' }, 413),
})

type BlobObjectEnv = { Variables: AuthContext }
type BatchBlobObjectContext = Context<BlobObjectEnv, '/:cid/blob-objects/batch'>

interface ApplyBatchEntriesParams {
  contents: CollectionContents
  userId: string
  collectionId: string
  entries: BatchUploadEntry[]
}

async function applyBatchEntries({
  contents,
  userId,
  collectionId,
  entries,
}: ApplyBatchEntriesParams): Promise<BatchUploadResult[]> {
  const results: BatchUploadResult[] = []
  for (const entry of entries) {
    try {
      results.push(await applyBatchUploadEntry({ contents, userId, collectionId, entry }))
    } catch (error) {
      log.error('batch blob-object upload failed', {
        userId,
        collectionId,
        error: String(error),
      })
      results.push({ status: 'error', error: 'internal server error' })
    }
  }
  return results
}

async function handleBatchBlobObjectUpload(
  c: BatchBlobObjectContext,
  contents: CollectionContents,
) {
  const userId = c.var.user.id
  const collectionId = c.req.param('cid')
  if (!(await collectionBelongsToUser({ userId, collectionId }))) {
    return c.json({ error: 'not found' }, 404)
  }

  const parsed = parseBatchUploadEntries(new Uint8Array(await c.req.arrayBuffer()))
  if (!Array.isArray(parsed)) return c.json({ error: parsed.error }, 400)
  if (parsed.length === 0) {
    return c.json({ error: 'batch must contain at least one entry' }, 400)
  }

  const results = await applyBatchEntries({ contents, userId, collectionId, entries: parsed })
  return c.json({ results })
}

/** Creates the framed batch upload route for collection blob objects. */
export function createBatchBlobObjectRoutes(
  contents: CollectionContents,
): Hono<{ Variables: AuthContext }> {
  const routes = new Hono<{ Variables: AuthContext }>()
  routes.post('/:cid/blob-objects/batch', batchUploadLimit, (c) =>
    handleBatchBlobObjectUpload(c, contents),
  )
  return routes
}
