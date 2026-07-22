import { Hono } from 'hono'
import { bodyLimit } from 'hono/body-limit'
import { uuidv7 } from 'uuidv7'
import type { AuthContext } from '../auth/middleware.ts'
import type { BlobStore } from '../blob/interface.ts'
import { env } from '../env.ts'
import { log } from '../logger.ts'

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

const blobLimit = bodyLimit({
  maxSize: env.MAX_BLOB_BYTES,
  onError: (c) => c.json({ error: 'blob too large' }, 413),
})

// Batch fetch key-count limit. Clients bin-pack requests well under this and
// under env.MAX_BATCH_BYTES using the size_bytes they already hold from the
// objects listing; the byte cap is a backstop against stale/lying sizes,
// enforced via status=omitted frames.
export const MAX_BATCH_KEYS = 200

// Max accepted length of a single requested key, in UTF-16 code units. Valid
// keys ("{uuid}/{uuid}") are 73 chars; the ceiling exists because rejected
// keys are echoed back in a frame whose keyLen field is a u16 — an unbounded
// key would overflow it and corrupt the frame stream.
export const MAX_BATCH_KEY_CHARS = 128

// Request-body cap for a batch request: 200 keys of ≤128 chars plus JSON
// syntax fits comfortably; anything bigger is garbage.
const batchLimit = bodyLimit({
  maxSize: 65536,
  onError: (c) => c.json({ error: 'request too large' }, 413),
})

// Per-entry status byte in a batch response frame.
export const BATCH_STATUS_OK = 0
export const BATCH_STATUS_MISSING = 1
export const BATCH_STATUS_OMITTED = 2
type BatchStatus =
  | typeof BATCH_STATUS_OK
  | typeof BATCH_STATUS_MISSING
  | typeof BATCH_STATUS_OMITTED

const textEncoder = new TextEncoder()
const FRAME_FIXED_BYTES = 2 + 1 + 4

// One response frame: [u16 keyLen][key utf8][u8 status][u32 blobLen][bytes],
// integers big-endian. Missing/omitted frames carry blobLen=0 and no bytes.
function encodeFrame(key: string, status: BatchStatus, blob: Uint8Array | null): Uint8Array {
  const keyBytes = textEncoder.encode(key)
  const blobLen = blob?.byteLength ?? 0
  const frame = new Uint8Array(2 + keyBytes.length + 1 + 4 + blobLen)
  const view = new DataView(frame.buffer)
  view.setUint16(0, keyBytes.length)
  frame.set(keyBytes, 2)
  frame[2 + keyBytes.length] = status
  view.setUint32(2 + keyBytes.length + 1, blobLen)
  if (blob) frame.set(blob, 2 + keyBytes.length + 1 + 4)
  return frame
}

export function createBlobRoutes(store: BlobStore): Hono<{ Variables: AuthContext }> {
  const app = new Hono<{ Variables: AuthContext }>()

  // Upload an opaque blob. Returns the storage key scoped to the authenticated user.
  app.post('/', blobLimit, async (c) => {
    const userId = c.var.user.id
    const body = await c.req.arrayBuffer()
    if (body.byteLength === 0) {
      return c.json({ error: 'empty body' }, 400)
    }

    const blobId = uuidv7()
    const key = `${userId}/${blobId}`
    await store.put(key, new Uint8Array(body))
    return c.json({ key }, 201)
  })

  // Batch download: body {keys: [...]}, response is a stream of binary
  // frames (see encodeFrame), one per requested key IN REQUEST ORDER.
  // Not-owned / malformed / absent keys yield status=missing rather than an
  // error — same no-existence-leak policy as the single GET's 404 — so one
  // bad key can't sink the other 199. Cumulative payload is capped at
  // env.MAX_BATCH_BYTES: once exceeded, remaining entries come back as
  // status=omitted and the client re-requests them in a fresh batch. The cap
  // includes complete frame overhead. The first blob of a response is always
  // sent even if it alone exceeds the cap, so a client that must fetch an
  // over-cap blob still makes progress.
  app.post('/batch', batchLimit, async (c) => {
    const userId = c.var.user.id
    let body: { keys?: unknown }
    try {
      body = await c.req.json()
    } catch {
      return c.json({ error: 'invalid json' }, 400)
    }
    const keys = body.keys
    if (
      !Array.isArray(keys) || keys.length === 0 ||
      !keys.every((k) => typeof k === 'string' && k.length <= MAX_BATCH_KEY_CHARS)
    ) {
      return c.json({ error: `keys: non-empty array of strings (max ${MAX_BATCH_KEY_CHARS} chars each) required` }, 400)
    }
    if (keys.length > MAX_BATCH_KEYS) {
      return c.json({ error: `too many keys (max ${MAX_BATCH_KEYS})` }, 400)
    }

    const frameOverheads = keys.map((key) => FRAME_FIXED_BYTES + textEncoder.encode(key).byteLength)
    const remainingOverheads = new Array<number>(keys.length).fill(0)
    for (let i = keys.length - 2; i >= 0; i--) {
      remainingOverheads[i] = remainingOverheads[i + 1] + frameOverheads[i + 1]
    }
    let index = 0
    let sentBytes = 0
    let sentBlob = false
    let capReached = false
    const stream = new ReadableStream<Uint8Array>({
      async pull(controller) {
        if (index >= keys.length) {
          controller.close()
          return
        }
        const frameIndex = index++
        const key = keys[frameIndex]
        const [keyUserId, blobId, extra] = key.split('/')
        if (extra !== undefined || keyUserId !== userId || !UUID_RE.test(blobId ?? '')) {
          const frame = encodeFrame(key, BATCH_STATUS_MISSING, null)
          sentBytes += frame.byteLength
          controller.enqueue(frame)
          return
        }
        if (capReached) {
          const frame = encodeFrame(key, BATCH_STATUS_OMITTED, null)
          sentBytes += frame.byteLength
          controller.enqueue(frame)
          return
        }
        // A store failure (non-ENOENT) aborts the stream: the 200 is already
        // sent, so a clean error frame can't signal it — erroring the
        // controller kills the connection mid-body, which clients detect as
        // a truncated transfer and retry.
        let data: Uint8Array | null
        try {
          data = await store.get(key)
        } catch (err) {
          log.error('batch blob read failed', { key, err: String(err) })
          controller.error(err)
          return
        }
        if (!data) {
          const frame = encodeFrame(key, BATCH_STATUS_MISSING, null)
          sentBytes += frame.byteLength
          controller.enqueue(frame)
          return
        }
        const completeResponseBytes = sentBytes + frameOverheads[frameIndex] +
          data.byteLength + remainingOverheads[frameIndex]
        if (sentBlob && completeResponseBytes > env.MAX_BATCH_BYTES) {
          const frame = encodeFrame(key, BATCH_STATUS_OMITTED, null)
          sentBytes += frame.byteLength
          capReached = true
          controller.enqueue(frame)
          return
        }
        const frame = encodeFrame(key, BATCH_STATUS_OK, data)
        sentBytes += frame.byteLength
        sentBlob = true
        if (completeResponseBytes > env.MAX_BATCH_BYTES) capReached = true
        controller.enqueue(frame)
      },
    })
    return new Response(stream, {
      status: 200,
      headers: { 'Content-Type': 'application/octet-stream' },
    })
  })

  // Download a blob. Only the owning user can access it.
  app.get('/:userId/:blobId', async (c) => {
    const userId = c.var.user.id
    const paramUserId = c.req.param('userId')
    const paramBlobId = c.req.param('blobId')

    // Ownership check — return 404 (not 403) to avoid existence leaks.
    if (paramUserId !== userId || !UUID_RE.test(paramBlobId)) {
      return c.json({ error: 'not found' }, 404)
    }

    const key = `${paramUserId}/${paramBlobId}`
    const data = await store.get(key)
    if (!data) return c.json({ error: 'not found' }, 404)

    return new Response(data, {
      status: 200,
      headers: { 'Content-Type': 'application/octet-stream' },
    })
  })

  // Delete a blob. Only the owning user can delete it.
  app.delete('/:userId/:blobId', async (c) => {
    const userId = c.var.user.id
    const paramUserId = c.req.param('userId')
    const paramBlobId = c.req.param('blobId')

    if (paramUserId !== userId || !UUID_RE.test(paramBlobId)) {
      return c.json({ error: 'not found' }, 404)
    }

    const key = `${paramUserId}/${paramBlobId}`
    await store.delete(key)
    return c.body(null, 204)
  })

  return app
}
