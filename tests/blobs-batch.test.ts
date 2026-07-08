import assert from 'node:assert/strict'
import { afterAll, beforeAll, test } from 'bun:test'

// Tiny batch-byte cap so the omitted-tail path is exercisable with small
// fixtures. env.ts snapshots at import time, so this must precede the dynamic
// imports below (same env-snapshot pattern as tests/blob-limit.test.ts).
process.env.MAX_BATCH_BYTES = '1024'

const { buildApp } = await import('../src/app.ts')
const { db, waitForDb } = await import('../src/db/connection.ts')
const { migrateToLatest } = await import('../src/db/migrate.ts')
const { MAX_BATCH_KEYS, MAX_BATCH_KEY_CHARS, BATCH_STATUS_OK, BATCH_STATUS_MISSING, BATCH_STATUS_OMITTED } =
  await import('../src/blobs/routes.ts')

const app = buildApp()

beforeAll(async () => {
  await waitForDb()
  await migrateToLatest()
})

afterAll(async () => {
  await db.destroy()
})

async function devLogin(): Promise<{ token: string; user: { id: string } }> {
  const email = `blobs-batch-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const response = await app.fetch(new Request('http://test.local/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Blobs Batch Test' }),
  }))
  assert.equal(response.status, 200)
  return await response.json() as { token: string; user: { id: string } }
}

async function uploadBlob(token: string, bytes: Uint8Array): Promise<string> {
  const response = await app.fetch(new Request('http://test.local/api/blobs', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/octet-stream' },
    body: bytes,
  }))
  assert.equal(response.status, 201)
  const data = await response.json() as { key: string }
  return data.key
}

async function fetchBatch(token: string, keys: string[]): Promise<Response> {
  return app.fetch(new Request('http://test.local/api/blobs/batch', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ keys }),
  }))
}

interface Frame { key: string; status: number; blob: Uint8Array }

function parseFrames(body: Uint8Array): Frame[] {
  const frames: Frame[] = []
  const view = new DataView(body.buffer, body.byteOffset, body.byteLength)
  let off = 0
  while (off < body.byteLength) {
    const keyLen = view.getUint16(off)
    off += 2
    const key = new TextDecoder().decode(body.subarray(off, off + keyLen))
    off += keyLen
    const status = body[off]
    off += 1
    const blobLen = view.getUint32(off)
    off += 4
    const blob = body.subarray(off, off + blobLen)
    off += blobLen
    frames.push({ key, status, blob })
  }
  assert.equal(off, body.byteLength, 'frames must consume the body exactly')
  return frames
}

test('batch returns requested blobs byte-exact, in request order', async () => {
  const { token } = await devLogin()
  // Both must fit under the 1024-byte MAX_BATCH_BYTES override together.
  const blobA = new Uint8Array([1, 2, 3, 4, 5])
  const blobB = new Uint8Array(300).fill(0xab)
  const keyA = await uploadBlob(token, blobA)
  const keyB = await uploadBlob(token, blobB)

  const response = await fetchBatch(token, [keyB, keyA])
  assert.equal(response.status, 200)
  assert.equal(response.headers.get('Content-Type'), 'application/octet-stream')
  const frames = parseFrames(new Uint8Array(await response.arrayBuffer()))

  assert.equal(frames.length, 2)
  assert.equal(frames[0].key, keyB)
  assert.equal(frames[0].status, BATCH_STATUS_OK)
  assert.deepEqual(frames[0].blob, blobB)
  assert.equal(frames[1].key, keyA)
  assert.equal(frames[1].status, BATCH_STATUS_OK)
  assert.deepEqual(frames[1].blob, blobA)
})

test('missing, malformed, and foreign keys yield status=missing without failing the batch', async () => {
  const { token, user } = await devLogin()
  const other = await devLogin()
  const mine = await uploadBlob(token, new Uint8Array([9, 9, 9]))
  const foreign = await uploadBlob(other.token, new Uint8Array([7, 7, 7]))
  const absent = `${user.id}/01890000-0000-7000-8000-000000000000`
  const malformed = 'not-even-a-key'

  const response = await fetchBatch(token, [foreign, absent, malformed, mine])
  assert.equal(response.status, 200)
  const frames = parseFrames(new Uint8Array(await response.arrayBuffer()))

  assert.equal(frames.length, 4)
  assert.equal(frames[0].status, BATCH_STATUS_MISSING)
  assert.equal(frames[0].blob.length, 0)
  assert.equal(frames[1].status, BATCH_STATUS_MISSING)
  assert.equal(frames[2].status, BATCH_STATUS_MISSING)
  assert.equal(frames[3].status, BATCH_STATUS_OK)
  assert.deepEqual(frames[3].blob, new Uint8Array([9, 9, 9]))
})

test('rejects empty, non-array, and oversized key lists', async () => {
  const { token, user } = await devLogin()
  assert.equal((await fetchBatch(token, [])).status, 400)

  const notArray = await app.fetch(new Request('http://test.local/api/blobs/batch', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ keys: 'nope' }),
  }))
  assert.equal(notArray.status, 400)

  const tooMany = Array.from({ length: MAX_BATCH_KEYS + 1 }, () =>
    `${user.id}/01890000-0000-7000-8000-000000000000`)
  assert.equal((await fetchBatch(token, tooMany)).status, 400)
})

test('rejects over-long keys and oversized request bodies', async () => {
  const { token } = await devLogin()
  // A key past MAX_BATCH_KEY_CHARS would overflow the u16 keyLen field when
  // echoed back in a missing frame — must be rejected up front, not framed.
  const longKey = 'x'.repeat(MAX_BATCH_KEY_CHARS + 1)
  assert.equal((await fetchBatch(token, [longKey])).status, 400)

  const hugeBody = await app.fetch(new Request('http://test.local/api/blobs/batch', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ keys: ['x'.repeat(70000)] }),
  }))
  assert.equal(hugeBody.status, 413)
})

test('requires auth', async () => {
  const response = await app.fetch(new Request('http://test.local/api/blobs/batch', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ keys: ['x/y'] }),
  }))
  assert.equal(response.status, 401)
})

test('cumulative byte cap: entries past the cap return status=omitted', async () => {
  const { token } = await devLogin()
  // Cap is 1024 (env override above). 600 + 600 crosses it on the second blob.
  const blobA = new Uint8Array(600).fill(0x01)
  const blobB = new Uint8Array(600).fill(0x02)
  const blobC = new Uint8Array(10).fill(0x03)
  const keyA = await uploadBlob(token, blobA)
  const keyB = await uploadBlob(token, blobB)
  const keyC = await uploadBlob(token, blobC)

  const response = await fetchBatch(token, [keyA, keyB, keyC])
  assert.equal(response.status, 200)
  const frames = parseFrames(new Uint8Array(await response.arrayBuffer()))

  assert.equal(frames.length, 3)
  assert.equal(frames[0].status, BATCH_STATUS_OK)
  assert.deepEqual(frames[0].blob, blobA)
  // Second blob crosses the cap → omitted, and everything after it too.
  assert.equal(frames[1].status, BATCH_STATUS_OMITTED)
  assert.equal(frames[1].blob.length, 0)
  assert.equal(frames[2].status, BATCH_STATUS_OMITTED)
})

test('first blob always ships even when it alone exceeds the cap', async () => {
  const { token } = await devLogin()
  const big = new Uint8Array(4096).fill(0x5a) // > 1024 cap by itself
  const key = await uploadBlob(token, big)

  const response = await fetchBatch(token, [key, key])
  const frames = parseFrames(new Uint8Array(await response.arrayBuffer()))
  assert.equal(frames.length, 2)
  // Progress guarantee: first blob ships despite exceeding the cap alone.
  assert.equal(frames[0].status, BATCH_STATUS_OK)
  assert.deepEqual(frames[0].blob, big)
  assert.equal(frames[1].status, BATCH_STATUS_OMITTED)
})
