import assert from 'node:assert/strict'
import { afterAll, beforeAll, test } from 'bun:test'
import { sql } from 'kysely'
import { buildApp } from '../src/app.ts'
import { db, waitForDb } from '../src/db/connection.ts'
import { migrateToLatest } from '../src/db/migrate.ts'
import { notifier, type ChangeEvent } from '../src/sync/notifier.ts'

const app = buildApp()

beforeAll(async () => {
  await waitForDb()
  await migrateToLatest()
  await notifier.start()
})

afterAll(async () => {
  await notifier.stop()
  await db.destroy()
})

async function devLogin(): Promise<{ token: string; user: { id: string } }> {
  const email = `sync-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const response = await app.fetch(new Request('http://test.local/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Sync Test' }),
  }))
  assert.equal(response.status, 200)
  return await response.json() as { token: string; user: { id: string } }
}

async function createCollection(token: string): Promise<string> {
  const response = await app.fetch(new Request('http://test.local/api/collections', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}` },
  }))
  assert.equal(response.status, 201)
  const { collection } = await response.json() as { collection: { id: string } }
  return collection.id
}

async function createObjects(token: string, cid: string, count: number): Promise<void> {
  const auth = { Authorization: `Bearer ${token}` }
  for (let i = 0; i < count; i++) {
    const blob = await app.fetch(new Request('http://test.local/api/blobs', {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/octet-stream' },
      body: `ciphertext-${i}`,
    }))
    const { key: blobKey } = await blob.json() as { key: string }
    const res = await app.fetch(new Request(`http://test.local/api/collections/${cid}/objects`, {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ blob_key: blobKey, size_bytes: 12 }),
    }))
    assert.equal(res.status, 201)
  }
}

type PullPage = {
  objects: Array<{ id: string; change_seq: string }>
  hasMore?: boolean
  nextCursor?: number
}

async function pull(token: string, cid: string, query: string): Promise<PullPage> {
  const response = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}/objects${query}`,
    { headers: { Authorization: `Bearer ${token}` } },
  ))
  assert.equal(response.status, 200)
  return await response.json() as PullPage
}

test('pull without ?limit returns all objects and no paging fields', async () => {
  const { token } = await devLogin()
  const cid = await createCollection(token)
  await createObjects(token, cid, 5)

  const page = await pull(token, cid, '?sinceVersion=0')
  assert.equal(page.objects.length, 5)
  assert.equal(page.hasMore, undefined)
  assert.equal(page.nextCursor, undefined)
})

test('pull with ?limit honors the cap and reports hasMore=true short of the end', async () => {
  const { token } = await devLogin()
  const cid = await createCollection(token)
  await createObjects(token, cid, 5)

  const page = await pull(token, cid, '?sinceVersion=0&limit=2')
  assert.equal(page.objects.length, 2)
  assert.equal(page.hasMore, true)
  // change_seq runs 1..5; the second row's change_seq is 2.
  assert.equal(page.nextCursor, 2)
})

test('pull with ?limit at the exact boundary reports hasMore=false', async () => {
  const { token } = await devLogin()
  const cid = await createCollection(token)
  await createObjects(token, cid, 3)

  const page = await pull(token, cid, '?sinceVersion=0&limit=3')
  assert.equal(page.objects.length, 3)
  assert.equal(page.hasMore, false)
  assert.equal(page.nextCursor, 3)
})

test('resuming from nextCursor yields the remainder exactly once', async () => {
  const { token } = await devLogin()
  const cid = await createCollection(token)
  await createObjects(token, cid, 5)

  const seen: string[] = []
  let cursor = 0
  // Page through with limit=2 until exhausted.
  for (let guard = 0; guard < 10; guard++) {
    const page = await pull(token, cid, `?sinceVersion=${cursor}&limit=2`)
    for (const obj of page.objects) seen.push(obj.id)
    if (!page.hasMore) break
    assert.ok(page.nextCursor !== undefined)
    cursor = page.nextCursor
  }

  // Every object seen exactly once, in change_seq order, no duplicates/gaps.
  const all = await pull(token, cid, '?sinceVersion=0')
  assert.deepEqual(seen, all.objects.map((o) => o.id))
  assert.equal(new Set(seen).size, seen.length)
})

test('pull rejects an invalid ?limit with 400', async () => {
  const { token } = await devLogin()
  const cid = await createCollection(token)

  for (const bad of ['0', '-1', 'abc', '1.5']) {
    const response = await app.fetch(new Request(
      `http://test.local/api/collections/${cid}/objects?sinceVersion=0&limit=${bad}`,
      { headers: { Authorization: `Bearer ${token}` } },
    ))
    assert.equal(response.status, 400, `limit=${bad} should be rejected`)
  }
})

test('GET /api/sync/events requires auth', async () => {
  const response = await app.fetch(new Request('http://test.local/api/sync/events'))
  assert.equal(response.status, 401)
})

test('GET /api/sync/events sets SSE headers and emits ready event', async () => {
  const { token } = await devLogin()
  const stream = await app.fetch(new Request('http://test.local/api/sync/events', {
    headers: { Authorization: `Bearer ${token}` },
  }))
  assert.equal(stream.status, 200)
  assert.match(stream.headers.get('Content-Type') ?? '', /text\/event-stream/)
  assert.match(stream.headers.get('Cache-Control') ?? '', /no-cache/)
  assert.equal(stream.headers.get('X-Accel-Buffering'), 'no')

  assert.ok(stream.body)
  const reader = stream.body.getReader()
  const decoder = new TextDecoder()
  const chunk = decoder.decode((await reader.read()).value)
  assert.match(chunk, /event: ready/)
  await reader.cancel()
})

test('notifier delivers a transactional publish to in-process subscribers', async () => {
  const userId = `test-user-${Date.now()}-${Math.random().toString(16).slice(2)}`
  const received: ChangeEvent[] = []
  const unsubscribe = notifier.subscribe(userId, (event) => {
    received.push(event)
  })

  try {
    await db.transaction().execute(async (trx) => {
      await notifier.publish(trx, { userId, collectionId: 'col-abc', currentVersion: 7 })
    })

    const deadline = Date.now() + 2000
    while (received.length === 0 && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 25))
    }
    assert.deepEqual(received, [{ userId, collectionId: 'col-abc', currentVersion: 7 }])
  } finally {
    unsubscribe()
  }
})

test('NOTIFY is not fired on a rolled-back transaction', async () => {
  const userId = `test-user-${Date.now()}-${Math.random().toString(16).slice(2)}`
  const received: ChangeEvent[] = []
  const unsubscribe = notifier.subscribe(userId, (event) => {
    received.push(event)
  })

  try {
    await db.transaction().execute(async (trx) => {
      await notifier.publish(trx, { userId, collectionId: 'col-xyz', currentVersion: 99 })
      throw new Error('boom')
    }).catch(() => {})

    // Give plenty of time for any (incorrect) delivery to arrive.
    await new Promise((r) => setTimeout(r, 300))
    assert.deepEqual(received, [])
  } finally {
    unsubscribe()
  }
})

test('object mutations publish change events to an open SSE stream', async () => {
  const { token } = await devLogin()
  const auth = { Authorization: `Bearer ${token}` }

  const createCol = await app.fetch(new Request('http://test.local/api/collections', {
    method: 'POST',
    headers: auth,
  }))
  const { collection } = await createCol.json() as { collection: { id: string } }

  const stream = await app.fetch(new Request('http://test.local/api/sync/events', { headers: auth }))
  assert.equal(stream.status, 200)
  assert.ok(stream.body)
  const reader = stream.body.getReader()
  const decoder = new TextDecoder()

  // Drain the initial ready event.
  await reader.read()

  // Trigger a mutation.
  const blob = await app.fetch(new Request('http://test.local/api/blobs', {
    method: 'POST',
    headers: { ...auth, 'Content-Type': 'application/octet-stream' },
    body: 'ciphertext-1',
  }))
  const { key: blobKey } = await blob.json() as { key: string }

  await app.fetch(new Request(`http://test.local/api/collections/${collection.id}/objects`, {
    method: 'POST',
    headers: { ...auth, 'Content-Type': 'application/json' },
    body: JSON.stringify({ blob_key: blobKey, size_bytes: 12 }),
  }))

  // Read until we see the change event (or time out).
  const deadline = Date.now() + 3000
  let saw = false
  while (!saw && Date.now() < deadline) {
    const result = await Promise.race([
      reader.read(),
      new Promise<{ done: true; value: undefined }>((resolve) =>
        setTimeout(() => resolve({ done: true, value: undefined }), Math.max(0, deadline - Date.now())),
      ),
    ])
    if (result.done || !result.value) break
    const text = decoder.decode(result.value)
    if (text.includes('event: change') && text.includes(collection.id)) {
      saw = true
    }
  }
  assert.ok(saw, 'expected a change event for the new object')
  await reader.cancel()
})

test('rapid same-collection mutations still deliver the latest version', async () => {
  const { token } = await devLogin()
  const auth = { Authorization: `Bearer ${token}` }

  const createCol = await app.fetch(new Request('http://test.local/api/collections', {
    method: 'POST',
    headers: auth,
  }))
  const { collection } = await createCol.json() as { collection: { id: string } }

  const stream = await app.fetch(new Request('http://test.local/api/sync/events', { headers: auth }))
  const reader = stream.body!.getReader()
  const decoder = new TextDecoder()
  await reader.read() // drain ready

  // Three creates bump the collection version to 1, 2, 3. The stream coalesces
  // by collection, but must always surface the newest version eventually.
  for (let i = 0; i < 3; i++) {
    const blob = await app.fetch(new Request('http://test.local/api/blobs', {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/octet-stream' },
      body: `ciphertext-${i}`,
    }))
    const { key: blobKey } = await blob.json() as { key: string }
    await app.fetch(new Request(`http://test.local/api/collections/${collection.id}/objects`, {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ blob_key: blobKey, size_bytes: 12 }),
    }))
  }

  const deadline = Date.now() + 3000
  let maxVersion = 0
  while (maxVersion < 3 && Date.now() < deadline) {
    const result = await Promise.race([
      reader.read(),
      new Promise<{ done: true; value: undefined }>((resolve) =>
        setTimeout(() => resolve({ done: true, value: undefined }), Math.max(0, deadline - Date.now())),
      ),
    ])
    if (result.done || !result.value) break
    for (const line of decoder.decode(result.value).split('\n')) {
      if (!line.startsWith('data: ')) continue
      const data = line.slice(6).trim()
      if (!data) continue
      const parsed = JSON.parse(data) as { collectionId: string; currentVersion: number }
      assert.equal(parsed.collectionId, collection.id)
      maxVersion = Math.max(maxVersion, parsed.currentVersion)
    }
  }
  assert.equal(maxVersion, 3, 'expected the stream to surface the final collection version')
  await reader.cancel()
})

// Runs last: it kills the shared notifier's connection. The notifier self-heals
// within the backoff window, but later tests would race the reconnect.
test('listener reconnects after its Postgres connection is dropped', async () => {
  const userId = `reconnect-${Date.now()}-${Math.random().toString(16).slice(2)}`
  const received: ChangeEvent[] = []
  let lostFired = 0
  const unsubscribe = notifier.subscribe(userId, (e) => received.push(e))
  const unsubscribeLost = notifier.onListenerLost(() => { lostFired++ })

  try {
    // Kill the notifier's dedicated LISTEN backend (identified by app name).
    await sql`
      SELECT pg_terminate_backend(pid) FROM pg_stat_activity
      WHERE application_name = 'futo-notes-notifier' AND pid <> pg_backend_pid()
    `.execute(db)

    // The dropped socket should fire onListenerLost so open streams can bail.
    const lostDeadline = Date.now() + 3000
    while (lostFired === 0 && Date.now() < lostDeadline) {
      await new Promise((r) => setTimeout(r, 25))
    }
    assert.ok(lostFired >= 1, 'expected onListenerLost to fire on connection drop')

    // Once the backoff reconnect re-LISTENs, a fresh publish is delivered again.
    // Publishes issued before the reconnect completes are lost, so we retry.
    const deadline = Date.now() + 8000
    while (received.length === 0 && Date.now() < deadline) {
      await db.transaction().execute(async (trx) => {
        await notifier.publish(trx, { userId, collectionId: 'recon', currentVersion: 1 })
      })
      await new Promise((r) => setTimeout(r, 250))
    }
    assert.ok(received.length >= 1, 'expected delivery to resume after reconnect')
  } finally {
    unsubscribe()
    unsubscribeLost()
  }
})
