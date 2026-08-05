import { isValidBatchMutationId } from '../../collection-contents/mutation-id.ts'
import { isUuidIdentifier } from '../../identifiers/is-uuid-identifier.ts'

/** Maximum number of framed mutations accepted by one batch upload request. */
export const MAX_BATCH_ENTRIES = 200

/** One decoded create or update from the batch upload wire format. */
export type BatchUploadEntry =
  | { op: 'create'; mutationId: string; version: 0; blob: Uint8Array }
  | { op: 'update'; objectId: string; version: number; blob: Uint8Array }

const textDecoder = new TextDecoder('utf-8', { fatal: true })

/** Parses the complete batch upload body or returns one framing error. */
export function parseBatchUploadEntries(
  body: Uint8Array,
): BatchUploadEntry[] | { error: string } {
  const entries: BatchUploadEntry[] = []
  const view = new DataView(body.buffer, body.byteOffset, body.byteLength)
  let offset = 0

  const requireBytes = (count: number): boolean => offset + count <= body.byteLength

  while (offset < body.byteLength) {
    if (entries.length >= MAX_BATCH_ENTRIES) {
      return { error: `too many entries (max ${MAX_BATCH_ENTRIES})` }
    }
    if (!requireBytes(1)) return { error: 'truncated operation' }
    const op = view.getUint8(offset)
    offset += 1
    if (op !== 0 && op !== 1) return { error: 'invalid operation' }

    if (!requireBytes(2)) return { error: 'truncated object id length' }
    const identifierLength = view.getUint16(offset)
    offset += 2
    if (!requireBytes(identifierLength)) return { error: 'truncated identifier' }

    let identifier: string
    try {
      identifier = textDecoder.decode(body.subarray(offset, offset + identifierLength))
    } catch {
      return { error: 'invalid identifier' }
    }
    if (op === 0 && !isValidBatchMutationId(identifier)) {
      return { error: 'invalid create mutation id' }
    }
    if (op === 1 && !isUuidIdentifier(identifier)) return { error: 'invalid update object id' }
    offset += identifierLength

    if (!requireBytes(4)) return { error: 'truncated version' }
    const version = view.getUint32(offset)
    offset += 4
    if (op === 0 && version !== 0) return { error: 'create version must be zero' }
    if (op === 1 && version < 1) return { error: 'update version must be positive' }

    if (!requireBytes(4)) return { error: 'truncated blob length' }
    const blobLength = view.getUint32(offset)
    offset += 4
    if (blobLength === 0) return { error: 'blob must not be empty' }
    if (!requireBytes(blobLength)) return { error: 'truncated blob' }

    const blob = body.subarray(offset, offset + blobLength)
    offset += blobLength
    entries.push(
      op === 0
        ? { op: 'create', mutationId: identifier, version: 0, blob }
        : { op: 'update', objectId: identifier, version, blob },
    )
  }

  return entries
}
