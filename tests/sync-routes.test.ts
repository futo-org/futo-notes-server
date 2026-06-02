import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { sql } from 'kysely'
import { buildApp } from '../src/app.ts'
import { db, waitForDb } from '../src/db/connection.ts'
import { migrateToLatest } from '../src/db/migrate.ts'
import { notifier, type ChangeEvent } from '../src/sync/notifier.ts'

const app = buildApp()

before(async () => {
  await waitForDb()
  await migrateToLatest()
  await notifier.start()
})

after(async () => {
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
