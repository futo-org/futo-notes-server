import type {
  CollectionContents,
  ObjectMutationOutcome,
  ObjectMutationResult,
} from '../../collection-contents/index.ts'
import { env } from '../../env.ts'
import type { BatchUploadEntry } from './frames.ts'

type SuccessfulMutation = Extract<ObjectMutationResult, { kind: 'ok' }>

interface ApplyBatchUploadEntryParams {
  contents: CollectionContents
  userId: string
  collectionId: string
  entry: BatchUploadEntry
}

/** Per-entry result returned in request order by the batch upload endpoint. */
export type BatchUploadResult =
  | { status: 'created'; object: SuccessfulMutation['object']; collectionVersion: number }
  | { status: 'replayed'; object: SuccessfulMutation['object']; collectionVersion: number }
  | { status: 'updated'; object: SuccessfulMutation['object']; collectionVersion: number }
  | { status: 'conflict'; currentVersion: number; currentBlobKey: string | null }
  | { status: 'not_found' }
  | { status: 'too_large' }
  | { status: 'error'; error: string }

function createBatchResult(outcome: ObjectMutationOutcome): BatchUploadResult {
  const { result } = outcome
  if (result.kind === 'ok') {
    return {
      status: outcome.replayed ? 'replayed' : 'created',
      object: result.object,
      collectionVersion: result.collectionVersion,
    }
  }
  if (result.kind === 'mutation_mismatch') {
    return { status: 'error', error: 'mutation id reused for different intent' }
  }
  return { status: 'error', error: 'create failed' }
}

function updateBatchResult(result: ObjectMutationResult): BatchUploadResult {
  switch (result.kind) {
    case 'ok':
      return {
        status: 'updated',
        object: result.object,
        collectionVersion: result.collectionVersion,
      }
    case 'conflict':
      return {
        status: 'conflict',
        currentVersion: result.currentVersion,
        currentBlobKey: result.currentBlobKey,
      }
    case 'not_found':
      return { status: 'not_found' }
    default:
      return { status: 'error', error: 'update failed' }
  }
}

/** Applies one framed batch upload through the Collection Contents coordinator. */
export async function applyBatchUploadEntry({
  contents,
  userId,
  collectionId,
  entry,
}: ApplyBatchUploadEntryParams): Promise<BatchUploadResult> {
  if (entry.blob.byteLength > env.MAX_BLOB_BYTES) {
    return { status: 'too_large' }
  }

  if (entry.op === 'create') {
    const outcome = await contents.mutateObjectWithReplay({
      kind: 'create',
      userId,
      collectionId,
      mutationId: entry.mutationId,
      blobData: entry.blob,
    })
    return createBatchResult(outcome)
  }

  return updateBatchResult(await contents.mutateObject({
    kind: 'update',
    userId,
    collectionId,
    objectId: entry.objectId,
    version: entry.version,
    blobData: entry.blob,
  }))
}
