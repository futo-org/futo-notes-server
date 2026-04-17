import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { buildApp } from '../src/app.ts'
import { db, waitForDb } from '../src/db/connection.ts'
import { migrateToLatest } from '../src/db/migrate.ts'

const app = buildApp()

before(async () => {
  await waitForDb()
  await migrateToLatest()
})

after(async () => {
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
  const data = await json<{ status: string; setup_complete: boolean }>('/health')
  assert.equal(data.status, 'ok')
  assert.equal(data.setup_complete, true)
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
