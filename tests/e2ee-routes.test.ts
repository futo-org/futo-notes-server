import assert from 'node:assert/strict'
import { afterAll, beforeAll, test } from 'bun:test'
import { uuidv7 } from 'uuidv7'
import { buildApp } from '../src/app.ts'
import { FsBlobStore } from '../src/blob/fs.ts'
import { db, waitForDb } from '../src/db/connection.ts'
import { migrateToLatest } from '../src/db/migrate.ts'
import { env } from '../src/env.ts'

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

  const deleteClaimed = await app.fetch(new Request(
    `http://test.local/api/blobs/${blobTwo}`,
    { method: 'DELETE', headers: auth },
  ))
  assert.equal(deleteClaimed.status, 409)
  assert.deepEqual(await deleteClaimed.json(), { error: 'blob is in use' })

  const stagedOnly = await uploadBlob(login.token, 'never-claimed')
  const deleteStaged = await app.fetch(new Request(
    `http://test.local/api/blobs/${stagedOnly}`,
    { method: 'DELETE', headers: auth },
  ))
  assert.equal(deleteStaged.status, 204)
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

test('Mutation-Id replays a one-call create without replacing its ciphertext', async () => {
  const email = `mutation-id-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const login = await json<{ token: string; user: { id: string } }>('/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Mutation ID Test' }),
  })
  const auth = { Authorization: `Bearer ${login.token}` }
  const created = await json<{ collection: { id: string } }>('/api/collections', {
    method: 'POST',
    headers: auth,
  })
  const mutationId = uuidv7()
  const firstResponse = await app.fetch(new Request(
    `http://test.local/api/collections/${created.collection.id}/blob-objects`,
    {
      method: 'POST',
      headers: {
        ...auth,
        'Content-Type': 'application/octet-stream',
        'Mutation-Id': mutationId,
      },
      body: 'original-ciphertext',
    },
  ))
  assert.equal(firstResponse.status, 201)
  const first = await firstResponse.json() as {
    object: { id: string; blob_key: string }
    collectionVersion: number
  }

  const retryResponse = await app.fetch(new Request(
    `http://test.local/api/collections/${created.collection.id}/blob-objects`,
    {
      method: 'POST',
      headers: {
        ...auth,
        'Content-Type': 'application/octet-stream',
        'Mutation-Id': mutationId,
      },
      body: 'different-retry-body',
    },
  ))
  assert.equal(retryResponse.status, 201)
  assert.deepEqual(await retryResponse.json(), first)

  const blob = await app.fetch(new Request(
    `http://test.local/api/blobs/${first.object.blob_key}`,
    { headers: auth },
  ))
  assert.equal(blob.status, 200)
  assert.equal(await blob.text(), 'original-ciphertext')
})

test('deleting a collection removes its objects and leaves blobs for maintenance', async () => {
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

  const missingCollection = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}`,
    { headers: auth },
  ))
  assert.equal(missingCollection.status, 404)

  // The transaction only makes the blobs purgeable. Scheduled maintenance
  // removes their bytes after the collection deletion commits.
  for (const blobKey of [objOne.object.blob_key, objTwo.object.blob_key]) {
    const blob = await app.fetch(new Request(
      `http://test.local/api/blobs/${blobKey}`,
      { headers: auth },
    ))
    assert.equal(blob.status, 200)
  }
})

test('POST /collections creates independent collections for an account', async () => {
  const email = `plural-collections-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const login = await json<{ token: string; user: { id: string } }>('/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: 'Plural Collections' }),
  })
  const auth = { Authorization: `Bearer ${login.token}` }

  const firstResponse = await app.fetch(new Request('http://test.local/api/collections', {
    method: 'POST',
    headers: auth,
  }))
  assert.equal(firstResponse.status, 201)
  const first = await firstResponse.json() as { collection: { id: string } }

  const secondResponse = await app.fetch(new Request('http://test.local/api/collections', {
    method: 'POST',
    headers: auth,
  }))
  assert.equal(secondResponse.status, 201)
  const second = await secondResponse.json() as { collection: { id: string } }
  assert.notEqual(second.collection.id, first.collection.id)

  const list = await json<{ collections: Array<{ id: string }> }>('/api/collections', {
    headers: auth,
  })
  assert.deepEqual(
    list.collections.map((collection) => collection.id),
    [first.collection.id, second.collection.id],
  )

  await db.deleteFrom('users').where('id', '=', login.user.id).execute()
})

test('PUT /collections/:id/key supports idempotent claims, conflicts, and guarded rotation', async () => {
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

  const first = await json<{ key: { encrypted_vault_key: string; key_updated_at: string } }>(
    `/api/collections/${cid}/key`,
    {
      method: 'PUT',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ key_salt: 'aaaa', key_kdf: kdf, encrypted_vault_key: 'first-key' }),
    },
  )
  assert.equal(first.key.encrypted_vault_key, 'first-key')

  // An exact retry is safe after a lost response and does not invent a new
  // revision token.
  const retry = await json<{ key: { encrypted_vault_key: string; key_updated_at: string } }>(
    `/api/collections/${cid}/key`,
    {
      method: 'PUT',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ key_salt: 'aaaa', key_kdf: kdf, encrypted_vault_key: 'first-key' }),
    },
  )
  assert.deepEqual(retry.key, first.key)

  // A racing second client must receive an explicit conflict and the
  // authoritative key, rather than a misleading successful response.
  const second = await app.fetch(new Request(`http://test.local/api/collections/${cid}/key`, {
    method: 'PUT',
    headers: { ...auth, 'Content-Type': 'application/json' },
    body: JSON.stringify({ key_salt: 'bbbb', key_kdf: kdf, encrypted_vault_key: 'second-key' }),
  }))
  assert.equal(second.status, 409)
  const conflict = await second.json() as {
    error: string
    currentKey: { encrypted_vault_key: string; key_updated_at: string }
  }
  assert.equal(conflict.error, 'key conflict')
  assert.equal(conflict.currentKey.encrypted_vault_key, 'first-key')
  assert.equal(conflict.currentKey.key_updated_at, first.key.key_updated_at)

  // A client that names the exact revision it read may deliberately rotate.
  const rotated = await json<{ key: { encrypted_vault_key: string; key_updated_at: string } }>(
    `/api/collections/${cid}/key`,
    {
      method: 'PUT',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({
        key_salt: 'bbbb',
        key_kdf: kdf,
        encrypted_vault_key: 'second-key',
        previous_key_updated_at: first.key.key_updated_at,
      }),
    },
  )
  assert.equal(rotated.key.encrypted_vault_key, 'second-key')
  assert.notEqual(rotated.key.key_updated_at, first.key.key_updated_at)

  const stale = await app.fetch(new Request(`http://test.local/api/collections/${cid}/key`, {
    method: 'PUT',
    headers: { ...auth, 'Content-Type': 'application/json' },
    body: JSON.stringify({
      key_salt: 'cccc',
      key_kdf: kdf,
      encrypted_vault_key: 'stale-key',
      previous_key_updated_at: first.key.key_updated_at,
    }),
  }))
  assert.equal(stale.status, 409)
  const staleConflict = await stale.json() as { currentKey: { encrypted_vault_key: string } }
  assert.equal(staleConflict.currentKey.encrypted_vault_key, 'second-key')

  const fetched = await json<{ key: { encrypted_vault_key: string } }>(
    `/api/collections/${cid}/key`,
    { headers: auth },
  )
  assert.equal(fetched.key.encrypted_vault_key, 'second-key')
})

test('concurrent different vault-key claims produce one winner and one conflict', async () => {
  const login = await json<{ token: string }>('/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: `key-race-${uuidv7()}@example.test`, name: 'Key Claim Race' }),
  })
  const auth = { Authorization: `Bearer ${login.token}` }
  const created = await json<{ collection: { id: string } }>('/api/collections', {
    method: 'POST',
    headers: auth,
  })
  const request = (encryptedVaultKey: string) => app.fetch(new Request(
    `http://test.local/api/collections/${created.collection.id}/key`,
    {
      method: 'PUT',
      headers: { ...auth, 'Content-Type': 'application/json' },
      body: JSON.stringify({
        key_salt: `salt-${encryptedVaultKey}`,
        key_kdf: { kdf: 'pbkdf2-sha256', iterations: 100000 },
        encrypted_vault_key: encryptedVaultKey,
      }),
    },
  ))

  const responses = await Promise.all([request('client-a'), request('client-b')])
  assert.deepEqual(responses.map((response) => response.status).sort(), [200, 409])
  const winner = await responses.find((response) => response.status === 200)!.json() as {
    key: { encrypted_vault_key: string }
  }
  const loser = await responses.find((response) => response.status === 409)!.json() as {
    currentKey: { encrypted_vault_key: string }
  }
  assert.equal(loser.currentKey.encrypted_vault_key, winner.key.encrypted_vault_key)
})

test('superseded blobs are retained and stale raw updates do not leak blobs', async () => {
  const email = `orphan-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  const login = await json<{ token: string; user: { id: string } }>('/api/auth/dev/login', {
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

  // Update → v2 — v1's blob becomes a retained merge ancestor.
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

  const store = new FsBlobStore(env.BLOB_DIR)
  const filesBeforeStaleUpdate = await store.list(`${login.user.id}/`)

  // The version check happens before inline ciphertext is stored.
  const staleRes = await app.fetch(new Request(
    `http://test.local/api/collections/${cid}/blob-objects/${v1.object.id}?version=2`,
    { method: 'PUT', headers: { ...auth, 'Content-Type': 'application/octet-stream' }, body: 'stale-body' },
  ))
  assert.equal(staleRes.status, 409)
  assert.deepEqual(
    await store.list(`${login.user.id}/`),
    filesBeforeStaleUpdate,
  )

  // Both the retained ancestor and current blob remain fetchable.
  const ancestor = await app.fetch(new Request(
    `http://test.local/api/blobs/${v1.object.blob_key}`,
    { headers: auth },
  ))
  assert.equal(ancestor.status, 200)
  const live = await app.fetch(new Request(
    `http://test.local/api/blobs/${v2.object.blob_key}`,
    { headers: auth },
  ))
  assert.equal(live.status, 200)
})
