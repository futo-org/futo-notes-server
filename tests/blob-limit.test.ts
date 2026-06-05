import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'

// Set a tiny limit before importing anything that snapshots env. env.ts reads
// MAX_BLOB_BYTES at import time, so this must precede the dynamic imports below
// (same env-snapshot pattern as tests/auth-password.test.ts).
process.env.MAX_BLOB_BYTES = '16'

const { buildApp } = await import('../src/app.ts')
const { db, waitForDb } = await import('../src/db/connection.ts')
const { migrateToLatest } = await import('../src/db/migrate.ts')

const app = buildApp()

before(async () => {
  await waitForDb()
  await migrateToLatest()
})

after(async () => {
  await db.destroy()
})

async function devLogin(): Promise<{ token: string; user: { id: string } }> {
  const email = `blob-limit-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const response = await app.fetch(new Request('http://test.local/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Blob Limit Test' }),
  }))
  assert.equal(response.status, 200)
  return await response.json() as { token: string; user: { id: string } }
}

test('POST /api/blobs rejects an oversized body with 413', async () => {
  const { token } = await devLogin()
  const response = await app.fetch(new Request('http://test.local/api/blobs', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/octet-stream' },
    body: 'this body is definitely longer than sixteen bytes',
  }))
  assert.equal(response.status, 413)
  const data = await response.json() as { error: string }
  assert.equal(typeof data.error, 'string')
})

test('POST /api/blobs accepts a body within the limit', async () => {
  const { token } = await devLogin()
  const response = await app.fetch(new Request('http://test.local/api/blobs', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/octet-stream' },
    body: 'tiny',
  }))
  assert.equal(response.status, 201)
  const data = await response.json() as { key: string }
  assert.ok(data.key)
})

test('POST /api/blobs rejects an oversized chunked body with 413', async () => {
  // No Content-Length: exercises bodyLimit's streaming path, where the error is
  // thrown mid-body-read and handled by the app-level onError in app.ts.
  const { token } = await devLogin()
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(new TextEncoder().encode('x'.repeat(64)))
      controller.close()
    },
  })
  const response = await app.fetch(new Request('http://test.local/api/blobs', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/octet-stream' },
    body,
    duplex: 'half',
  } as RequestInit))
  assert.equal(response.status, 413)
  const data = await response.json() as { error: string }
  assert.equal(typeof data.error, 'string')
})

test('POST blob-objects rejects an oversized body with 413', async () => {
  const { token } = await devLogin()
  const auth = { Authorization: `Bearer ${token}` }
  const created = await app.fetch(new Request('http://test.local/api/collections', {
    method: 'POST',
    headers: auth,
  }))
  const { collection } = await created.json() as { collection: { id: string } }

  const response = await app.fetch(new Request(
    `http://test.local/api/collections/${collection.id}/blob-objects`,
    {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/octet-stream' },
      body: 'this body is definitely longer than sixteen bytes',
    },
  ))
  assert.equal(response.status, 413)
  const data = await response.json() as { error: string }
  assert.equal(typeof data.error, 'string')
})

test('POST blob-objects accepts a body within the limit', async () => {
  const { token } = await devLogin()
  const auth = { Authorization: `Bearer ${token}` }
  const created = await app.fetch(new Request('http://test.local/api/collections', {
    method: 'POST',
    headers: auth,
  }))
  const { collection } = await created.json() as { collection: { id: string } }

  const response = await app.fetch(new Request(
    `http://test.local/api/collections/${collection.id}/blob-objects`,
    {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/octet-stream' },
      body: 'tiny',
    },
  ))
  assert.equal(response.status, 201)
  const data = await response.json() as { object: { id: string } }
  assert.ok(data.object.id)
})
