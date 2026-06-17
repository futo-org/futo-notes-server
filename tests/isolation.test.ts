import assert from 'node:assert/strict'
import { afterAll, beforeAll, test } from 'bun:test'
import { buildApp } from '../src/app.ts'
import { db, waitForDb } from '../src/db/connection.ts'
import { migrateToLatest } from '../src/db/migrate.ts'

// Cross-tenant isolation guard. DESIGN.md calls cross-tenant leakage the
// top-priority invariant: every query must be scoped by the authenticated
// user's id. A refactor that drops a `.where('user_id', ...)` should fail here.
// User B must never see, mutate, or even confirm the existence of user A's data
// — the server returns 404 (not 403) for anything B does not own.

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

async function devLogin(label: string): Promise<{ token: string; user: { id: string } }> {
  const email = `iso-${label}-${Date.now()}-${Math.random().toString(16).slice(2)}@example.test`
  return await json<{ token: string; user: { id: string } }>('/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, name: `Iso ${label}` }),
  })
}

async function uploadBlob(token: string, body: string): Promise<string> {
  const response = await app.fetch(new Request('http://test.local/api/blobs', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/octet-stream' },
    body,
  }))
  assert.equal(response.status, 201)
  const data = await response.json() as { key: string }
  return data.key
}

// Asserts a request as user B never leaks user A's data: never 200, always 404,
// and the response body never echoes one of A's identifiers.
async function assertNotFound(path: string, init: RequestInit, leakyIds: string[]): Promise<void> {
  const label = `${init.method ?? 'GET'} ${path}`
  const response = await app.fetch(new Request(`http://test.local${path}`, init))
  assert.notEqual(response.status, 200, `${label} must not return 200 to a non-owner`)
  assert.equal(response.status, 404, `${label} must return 404 to a non-owner`)
  const text = await response.text()
  for (const id of leakyIds) {
    assert.ok(!text.includes(id), `${label} leaked an owner-only id in its body`)
  }
}

test('user B cannot read, mutate, or discover user A data', async () => {
  const a = await devLogin('a')
  const b = await devLogin('b')
  assert.notEqual(a.user.id, b.user.id)
  const aAuth = { Authorization: `Bearer ${a.token}` }
  const bAuth = { Authorization: `Bearer ${b.token}` }

  // A owns a collection, vault key material, an object, and that object's blob.
  const created = await json<{ collection: { id: string } }>('/api/collections', {
    method: 'POST',
    headers: aAuth,
  })
  const cid = created.collection.id

  const keyMaterial = {
    key_salt: '00112233445566778899aabbccddeeff',
    key_kdf: { kdf: 'pbkdf2-sha256', iterations: 100000, hash: 'SHA-256' },
    encrypted_vault_key: 'aabbccddeeff00112233445566778899',
  }
  await json(`/api/collections/${cid}/key`, {
    method: 'PUT',
    headers: { ...aAuth, 'Content-Type': 'application/json' },
    body: JSON.stringify(keyMaterial),
  })

  const aBlobKey = await uploadBlob(a.token, 'a-ciphertext')
  const object = await json<{ object: { id: string; blob_key: string } }>(
    `/api/collections/${cid}/objects`,
    {
      method: 'POST',
      headers: { ...aAuth, 'Content-Type': 'application/json' },
      body: JSON.stringify({ blob_key: aBlobKey, size_bytes: 12 }),
    },
  )
  const oid = object.object.id

  // Identifiers that must never surface in any response served to B.
  const leakyIds = [cid, oid, aBlobKey, a.user.id, keyMaterial.encrypted_vault_key, keyMaterial.key_salt]

  // A valid blob key B owns — proves B's POST is rejected by collection
  // ownership, not by blob-key validation.
  const bBlobKey = await uploadBlob(b.token, 'b-ciphertext')

  const jsonHeaders = { ...bAuth, 'Content-Type': 'application/json' }
  const cases: Array<{ path: string; init: RequestInit }> = [
    // Collection
    { path: `/api/collections/${cid}`, init: { method: 'GET', headers: bAuth } },
    { path: `/api/collections/${cid}`, init: { method: 'DELETE', headers: bAuth } },
    // Collection key material
    { path: `/api/collections/${cid}/key`, init: { method: 'GET', headers: bAuth } },
    {
      path: `/api/collections/${cid}/key`,
      init: { method: 'PUT', headers: jsonHeaders, body: JSON.stringify(keyMaterial) },
    },
    // Object
    { path: `/api/collections/${cid}/objects/${oid}`, init: { method: 'GET', headers: bAuth } },
    {
      path: `/api/collections/${cid}/objects/${oid}`,
      init: {
        method: 'PUT',
        headers: jsonHeaders,
        body: JSON.stringify({ version: 2, blob_key: bBlobKey, size_bytes: 12 }),
      },
    },
    { path: `/api/collections/${cid}/objects/${oid}`, init: { method: 'DELETE', headers: bAuth } },
    // Object listing / pull for A's collection
    { path: `/api/collections/${cid}/objects`, init: { method: 'GET', headers: bAuth } },
    // Creating an object inside A's collection
    {
      path: `/api/collections/${cid}/objects`,
      init: {
        method: 'POST',
        headers: jsonHeaders,
        body: JSON.stringify({ blob_key: bBlobKey, size_bytes: 12 }),
      },
    },
    // Blob owned by A, addressed by A's real key
    { path: `/api/blobs/${aBlobKey}`, init: { method: 'GET', headers: bAuth } },
    { path: `/api/blobs/${aBlobKey}`, init: { method: 'DELETE', headers: bAuth } },
  ]

  for (const { path, init } of cases) {
    await assertNotFound(path, init, leakyIds)
  }

  // A's object/blob survived B's mutation and delete attempts.
  const stillThere = await json<{ object: { id: string; deleted: boolean; blob_key: string } }>(
    `/api/collections/${cid}/objects/${oid}`,
    { headers: aAuth },
  )
  assert.equal(stillThere.object.id, oid)
  assert.equal(stillThere.object.deleted, false)
  const aBlob = await app.fetch(new Request(`http://test.local/api/blobs/${aBlobKey}`, { headers: aAuth }))
  assert.equal(aBlob.status, 200)
  assert.equal(await aBlob.text(), 'a-ciphertext')

  // B's collection list never includes A's collection.
  const bCollections = await json<{ collections: Array<{ id: string; user_id: string }> }>('/api/collections', {
    headers: bAuth,
  })
  assert.ok(!bCollections.collections.some((col) => col.id === cid))
  assert.ok(!bCollections.collections.some((col) => col.user_id === a.user.id))

  // B's own pull from its own (empty) collection never carries A's objects.
  const bOwnCollection = await json<{ collection: { id: string } }>('/api/collections', {
    method: 'POST',
    headers: bAuth,
  })
  const bPull = await json<{ objects: Array<{ id: string; blob_key: string | null }> }>(
    `/api/collections/${bOwnCollection.collection.id}/objects`,
    { headers: bAuth },
  )
  assert.ok(!bPull.objects.some((obj) => obj.id === oid))
  assert.ok(!bPull.objects.some((obj) => obj.blob_key === aBlobKey))
})
