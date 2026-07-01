import assert from 'node:assert/strict'
import { afterAll, beforeAll, test } from 'bun:test'
import { buildApp } from '../src/app.ts'
import { FsBlobStore } from '../src/blob/fs.ts'
import { db, waitForDb } from '../src/db/connection.ts'
import { migrateToLatest } from '../src/db/migrate.ts'
import { env } from '../src/env.ts'
import { runBlobGcOnce } from '../src/maintenance/blobGc.ts'

const app = buildApp()

beforeAll(async () => {
  await waitForDb()
  await migrateToLatest()
})

afterAll(async () => {
  await db.destroy()
})

async function json<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await app.fetch(new Request(`http://test.local${path}`, init))
  if (!response.ok) {
    assert.fail(`${path} failed with HTTP ${response.status}: ${await response.text()}`)
  }
  return await response.json() as T
}

async function uploadBlob(token: string, body: string): Promise<string> {
  const response = await app.fetch(new Request('http://test.local/api/blobs', {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/octet-stream',
    },
    body,
  }))
  assert.equal(response.status, 201)
  const data = await response.json() as { key: string }
  return data.key
}

test('health responds without auth', async () => {
  const data = await json<{ status: string }>('/health')
  assert.equal(data.status, 'ok')
})

test('dev auth, collections, blobs, and global sync cursor round-trip', async () => {
  const email = `sync-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const login = await json<{ token: string; user: { id: string } }>('/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Sync Test' }),
  })
  assert.ok(login.token)

  const auth = { Authorization: `Bearer ${login.token}` }
  const created = await json<{ collection: { id: string; current_version: string } }>('/api/collections', {
    method: 'POST',
    headers: auth,
  })
  assert.equal(Number(created.collection.current_version), 0)

  const initialKey = await json<{ key: null | unknown }>(
    `/api/collections/${created.collection.id}/key`,
    { headers: auth },
  )
  assert.equal(initialKey.key, null)

  const keyMaterial = {
    key_salt: '00112233445566778899aabbccddeeff',
    key_kdf: { kdf: 'pbkdf2-sha256', iterations: 100000, hash: 'SHA-256' },
    encrypted_vault_key: 'aabbccddeeff00112233445566778899',
  }
  const savedKey = await json<{
    key: { key_salt: string; key_kdf: Record<string, unknown>; encrypted_vault_key: string }
  }>(`/api/collections/${created.collection.id}/key`, {
    method: 'PUT',
    headers: { ...auth, 'Content-Type': 'application/json' },
    body: JSON.stringify(keyMaterial),
  })
  assert.deepEqual(savedKey.key.key_kdf, keyMaterial.key_kdf)

  const fetchedKey = await json<{
    key: { key_salt: string; key_kdf: Record<string, unknown>; encrypted_vault_key: string }
  }>(`/api/collections/${created.collection.id}/key`, { headers: auth })
  assert.equal(fetchedKey.key.key_salt, keyMaterial.key_salt)
  assert.equal(fetchedKey.key.encrypted_vault_key, keyMaterial.encrypted_vault_key)

  const blobOne = await uploadBlob(login.token, 'encrypted-one')
  const firstObject = await json<{ object: { id: string; version: string; change_seq: string } }>(
    `/api/collections/${created.collection.id}/objects`,
    {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ blob_key: blobOne, size_bytes: 13 }),
    },
  )
  assert.equal(Number(firstObject.object.version), 1)
  assert.equal(Number(firstObject.object.change_seq), 1)

  const blobTwo = await uploadBlob(login.token, 'encrypted-two')
  const secondObject = await json<{ object: { id: string; version: string; change_seq: string } }>(
    `/api/collections/${created.collection.id}/objects`,
    {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ blob_key: blobTwo, size_bytes: 13 }),
    },
  )
  assert.equal(Number(secondObject.object.version), 1)
  assert.equal(Number(secondObject.object.change_seq), 2)

  const sinceFirst = await json<{ objects: Array<{ id: string; change_seq: string }> }>(
    `/api/collections/${created.collection.id}/objects?sinceVersion=1`,
    { headers: auth },
  )
  assert.deepEqual(sinceFirst.objects.map((object) => object.id), [secondObject.object.id])

  const downloaded = await app.fetch(new Request(`http://test.local/api/blobs/${blobTwo}`, {
    headers: auth,
  }))
  assert.equal(downloaded.status, 200)
  assert.equal(await downloaded.text(), 'encrypted-two')
})

test('blob-objects single-round-trip create, update, and conflict', async () => {
  const email = `bulk-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const login = await json<{ token: string; user: { id: string } }>('/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Bulk Test' }),
  })
  const auth = { Authorization: `Bearer ${login.token}` }

  const created = await json<{ collection: { id: string } }>('/api/collections', {
    method: 'POST',
    headers: auth,
  })
  const cid = created.collection.id

  // Create via blob-objects — one request.
  const createRes = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}/blob-objects`,
    {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/octet-stream' },
      body: 'ciphertext-one',
    },
  ))
  assert.equal(createRes.status, 201)
  const createBody = await createRes.json() as {
    object: { id: string; version: string; change_seq: string; blob_key: string; size_bytes: string }
    collectionVersion: number
  }
  assert.equal(Number(createBody.object.version), 1)
  assert.equal(Number(createBody.object.change_seq), 1)
  assert.equal(Number(createBody.object.size_bytes), 'ciphertext-one'.length)
  assert.equal(createBody.collectionVersion, 1)

  // Blob is fetchable.
  const blobRes = await app.fetch(new Request(
    `http://test.local/api/blobs/${createBody.object.blob_key}`,
    { headers: auth },
  ))
  assert.equal(blobRes.status, 200)
  assert.equal(await blobRes.text(), 'ciphertext-one')

  // Update via blob-objects — one request.
  const updateRes = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}/blob-objects/${createBody.object.id}?version=2`,
    {
      method: 'PUT',
      headers: { ...auth, 'Content-Type': 'application/octet-stream' },
      body: 'ciphertext-two',
    },
  ))
  assert.equal(updateRes.status, 200)
  const updateBody = await updateRes.json() as {
    object: { version: string; blob_key: string }
    collectionVersion: number
  }
  assert.equal(Number(updateBody.object.version), 2)
  assert.notEqual(updateBody.object.blob_key, createBody.object.blob_key)

  // Stale version → 409 with current state.
  const conflictRes = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}/blob-objects/${createBody.object.id}?version=2`,
    {
      method: 'PUT',
      headers: { ...auth, 'Content-Type': 'application/octet-stream' },
      body: 'ciphertext-stale',
    },
  ))
  assert.equal(conflictRes.status, 409)
  const conflictBody = await conflictRes.json() as {
    error: string
    currentVersion: number
    currentBlobKey: string
  }
  assert.equal(conflictBody.currentVersion, 2)
  assert.equal(conflictBody.currentBlobKey, updateBody.object.blob_key)

  // Empty body rejected.
  const emptyRes = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}/blob-objects`,
    {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/octet-stream' },
      body: new Uint8Array(0),
    },
  ))
  assert.equal(emptyRes.status, 400)

  // Missing version rejected on PUT.
  const missingVersionRes = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}/blob-objects/${createBody.object.id}`,
    {
      method: 'PUT',
      headers: { ...auth, 'Content-Type': 'application/octet-stream' },
      body: 'x',
    },
  ))
  assert.equal(missingVersionRes.status, 400)
})

test('deleting a collection orphans its objects blobs for GC', async () => {
  const email = `coll-del-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const login = await json<{ token: string; user: { id: string } }>('/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Collection Delete Test' }),
  })
  const auth = { Authorization: `Bearer ${login.token}` }
  const created = await json<{ collection: { id: string } }>('/api/collections', {
    method: 'POST',
    headers: auth,
  })
  const cid = created.collection.id

  // Two objects, each with its own blob.
  const blobOne = await uploadBlob(login.token, 'coll-del-one')
  const objOne = await json<{ object: { blob_key: string } }>(
    `/api/collections/${cid}/objects`,
    {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ blob_key: blobOne, size_bytes: 12 }),
    },
  )
  const blobTwo = await uploadBlob(login.token, 'coll-del-two')
  const objTwo = await json<{ object: { blob_key: string } }>(
    `/api/collections/${cid}/objects`,
    {
      method: 'POST',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ blob_key: blobTwo, size_bytes: 12 }),
    },
  )

  // Delete the collection — object rows cascade away.
  const delRes = await app.fetch(new Request(`http://test.local/api/collections/${cid}`, {
    method: 'DELETE',
    headers: auth,
  }))
  assert.equal(delRes.status, 204)

  // Each blob now has an orphaned_blobs row scoped to this user.
  for (const blobKey of [objOne.object.blob_key, objTwo.object.blob_key]) {
    const rows = await db
      .selectFrom('orphaned_blobs')
      .where('blob_key', '=', blobKey)
      .selectAll()
      .execute()
    assert.equal(rows.length, 1)
    assert.equal(rows[0].user_id, login.user.id)
  }
})

test('POST /collections is idempotent — one vault per account', async () => {
  const email = `single-vault-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const login = await json<{ token: string }>('/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Single Vault' }),
  })
  const auth = { Authorization: `Bearer ${login.token}` }

  const first = await json<{ collection: { id: string } }>('/api/collections', {
    method: 'POST',
    headers: auth,
  })
  // A second create returns the SAME vault, not a fork.
  const second = await json<{ collection: { id: string } }>('/api/collections', {
    method: 'POST',
    headers: auth,
  })
  assert.equal(second.collection.id, first.collection.id)

  const list = await json<{ collections: Array<{ id: string }> }>('/api/collections', {
    headers: auth,
  })
  assert.equal(list.collections.length, 1)
})

test('PUT /collections/:id/key is first-write-wins (never overwrites the vault key)', async () => {
  const email = `key-fww-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const login = await json<{ token: string }>('/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Key First Write Wins' }),
  })
  const auth = { Authorization: `Bearer ${login.token}` }
  const created = await json<{ collection: { id: string } }>('/api/collections', {
    method: 'POST',
    headers: auth,
  })
  const cid = created.collection.id
  const kdf = { kdf: 'pbkdf2-sha256', iterations: 100000, hash: 'SHA-256' }

  const first = await json<{ key: { encrypted_vault_key: string } }>(
    `/api/collections/${cid}/key`,
    {
      method: 'PUT',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ key_salt: 'aaaa', key_kdf: kdf, encrypted_vault_key: 'first-key' }),
    },
  )
  assert.equal(first.key.encrypted_vault_key, 'first-key')

  // A racing second client's PUT must NOT overwrite — it gets the authoritative
  // (first) key back so it adopts the one vault key.
  const second = await json<{ key: { encrypted_vault_key: string } }>(
    `/api/collections/${cid}/key`,
    {
      method: 'PUT',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ key_salt: 'bbbb', key_kdf: kdf, encrypted_vault_key: 'second-key' }),
    },
  )
  assert.equal(second.key.encrypted_vault_key, 'first-key')

  const fetched = await json<{ key: { encrypted_vault_key: string } }>(
    `/api/collections/${cid}/key`,
    { headers: auth },
  )
  assert.equal(fetched.key.encrypted_vault_key, 'first-key')
})

test('orphaned blobs are retained and reclaimed after retention window', async () => {
  const email = `orphan-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const login = await json<{ token: string }>('/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Orphan Test' }),
  })
  const auth = { Authorization: `Bearer ${login.token}` }
  const created = await json<{ collection: { id: string } }>('/api/collections', {
    method: 'POST',
    headers: auth,
  })
  const cid = created.collection.id

  // Create → v1 — first blob reference.
  const v1Res = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}/blob-objects`,
    { method: 'POST', headers: { ...auth, 'Content-Type': 'application/octet-stream' }, body: 'v1-body' },
  ))
  const v1 = (await v1Res.json()) as { object: { id: string; blob_key: string } }

  // Update → v2 — v1's blobKey becomes orphaned.
  const v2Res = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}/blob-objects/${v1.object.id}?version=2`,
    { method: 'PUT', headers: { ...auth, 'Content-Type': 'application/octet-stream' }, body: 'v2-body' },
  ))
  const v2 = (await v2Res.json()) as { object: { blob_key: string } }
  assert.notEqual(v2.object.blob_key, v1.object.blob_key)

  // v1 blob still reachable — needed for conflict resolution.
  const stillThere = await app.fetch(new Request(
    `http://test.local/api/blobs/${v1.object.blob_key}`,
    { headers: auth },
  ))
  assert.equal(stillThere.status, 200)
  assert.equal(await stillThere.text(), 'v1-body')

  // Ledger records the orphan.
  const ledgerRows = await db
    .selectFrom('orphaned_blobs')
    .where('blob_key', '=', v1.object.blob_key)
    .selectAll()
    .execute()
  assert.equal(ledgerRows.length, 1)

  // Stale update leaks its own blob — it lands in orphaned_blobs too.
  const staleRes = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}/blob-objects/${v1.object.id}?version=2`,
    { method: 'PUT', headers: { ...auth, 'Content-Type': 'application/octet-stream' }, body: 'stale-body' },
  ))
  assert.equal(staleRes.status, 409)
  const staleLeakCount = await db
    .selectFrom('orphaned_blobs')
    .where('user_id', '=', ledgerRows[0].user_id)
    .select(({ fn }) => fn.count<number>('blob_key').as('count'))
    .executeTakeFirstOrThrow()
  assert.ok(Number(staleLeakCount.count) >= 2)

  // GC with a 0-day retention deletes everything orphaned so far. This is the
  // one-shot GC runner the scheduled loop calls.
  const store = new FsBlobStore(env.BLOB_DIR)
  const before = await db.selectFrom('orphaned_blobs').selectAll().execute()
  assert.ok(before.length >= 2)
  const result = await runBlobGcOnce(store, 0)
  assert.ok(result.deleted >= 2)

  // Blob is gone from storage.
  const gone = await app.fetch(new Request(
    `http://test.local/api/blobs/${v1.object.blob_key}`,
    { headers: auth },
  ))
  assert.equal(gone.status, 404)

  // v2 — the live blob — is still reachable. GC must never touch live blobs.
  const live = await app.fetch(new Request(
    `http://test.local/api/blobs/${v2.object.blob_key}`,
    { headers: auth },
  ))
  assert.equal(live.status, 200)
})
