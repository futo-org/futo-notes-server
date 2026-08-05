import assert from 'node:assert/strict'
import { afterAll, beforeAll, test } from 'bun:test'

// env.ts snapshots these limits at import time. Keep the request cap large
// enough for 201 minimal frames while making both limit paths cheap to test.
process.env.MAX_BLOB_BYTES = '1024'
process.env.MAX_BATCH_BYTES = '65536'

const { buildApp } = await import('../src/app.ts')
const { FsBlobStore } = await import('../src/blob/fs.ts')
const { db, waitForDb } = await import('../src/db/connection.ts')
const { migrateToLatest } = await import('../src/db/migrate.ts')
const { MAX_BATCH_ENTRIES } = await import('../src/objects/batch-upload/frames.ts')
const { notifier } = await import('../src/sync/notifier.ts')
const { uuidv7 } = await import('uuidv7')

const app = buildApp()
const encoder = new TextEncoder()

beforeAll(async () => {
  await waitForDb()
  await migrateToLatest()
})

afterAll(async () => {
  await db.destroy()
})

interface Login {
  token: string
  user: { id: string }
}

interface ObjectRow {
  id: string
  version: string
  change_seq: string
  blob_key: string
  size_bytes: string
}

type Result =
  | { status: 'created' | 'replayed' | 'updated'; object: ObjectRow; collectionVersion: number }
  | { status: 'conflict'; currentVersion: number; currentBlobKey: string | null }
  | { status: 'not_found' | 'too_large' }
  | { status: 'error'; error: string }

async function devLogin(label: string): Promise<Login> {
  const response = await app.fetch(new Request('http://test.local/api/auth/dev/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      email: `blob-objects-batch-${label}-${uuidv7()}@example.test`,
      name: `Batch ${label}`,
    }),
  }))
  assert.equal(response.status, 200)
  return await response.json() as Login
}

async function createCollection(token: string): Promise<string> {
  const response = await app.fetch(new Request('http://test.local/api/collections', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}` },
  }))
  assert.equal(response.status, 201)
  return (await response.json() as { collection: { id: string } }).collection.id
}

function concatBytes(parts: Uint8Array[]): Uint8Array {
  const result = new Uint8Array(parts.reduce((sum, part) => sum + part.byteLength, 0))
  let offset = 0
  for (const part of parts) {
    result.set(part, offset)
    offset += part.byteLength
  }
  return result
}

function frame(
  op: number,
  identifier: string,
  version: number,
  blob: Uint8Array | string,
  framedBlobLength = typeof blob === 'string' ? encoder.encode(blob).byteLength : blob.byteLength,
): Uint8Array {
  const identifierBytes = encoder.encode(identifier)
  const blobBytes = typeof blob === 'string' ? encoder.encode(blob) : blob
  const result = new Uint8Array(11 + identifierBytes.byteLength + blobBytes.byteLength)
  const view = new DataView(result.buffer)
  let offset = 0
  view.setUint8(offset, op)
  offset += 1
  view.setUint16(offset, identifierBytes.byteLength)
  offset += 2
  result.set(identifierBytes, offset)
  offset += identifierBytes.byteLength
  view.setUint32(offset, version)
  offset += 4
  view.setUint32(offset, framedBlobLength)
  offset += 4
  result.set(blobBytes, offset)
  return result
}

const createFrame = (
  blob: Uint8Array | string,
  mutationId = uuidv7(),
): Uint8Array => frame(0, mutationId, 0, blob)

const updateFrame = (
  objectId: string,
  version: number,
  blob: Uint8Array | string,
): Uint8Array => frame(1, objectId, version, blob)

async function batchRequest(
  token: string | null,
  collectionId: string,
  body: Uint8Array,
): Promise<Response> {
  const headers: Record<string, string> = { 'Content-Type': 'application/octet-stream' }
  if (token) headers.Authorization = `Bearer ${token}`
  return app.fetch(new Request(
    `http://test.local/api/collections/${collectionId}/blob-objects/batch`,
    { method: 'POST', headers, body },
  ))
}

async function uploadBatch(
  token: string,
  collectionId: string,
  frames: Uint8Array[],
): Promise<Result[]> {
  const response = await batchRequest(token, collectionId, concatBytes(frames))
  if (response.status !== 200) {
    assert.fail(`batch failed with HTTP ${response.status}: ${await response.text()}`)
  }
  return (await response.json() as { results: Result[] }).results
}

function written(
  result: Result,
): Extract<Result, { status: 'created' | 'replayed' | 'updated' }> {
  assert.ok(result.status === 'created' || result.status === 'replayed' || result.status === 'updated')
  return result
}

async function createSingle(
  token: string,
  collectionId: string,
  mutationId: string,
  blob: string,
): Promise<{ object: ObjectRow; collectionVersion: number; replayed: boolean }> {
  const response = await app.fetch(new Request(
    `http://test.local/api/collections/${collectionId}/blob-objects`,
    {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/octet-stream',
        'Mutation-Id': mutationId,
      },
      body: blob,
    },
  ))
  assert.equal(response.status, 201)
  return await response.json() as {
    object: ObjectRow
    collectionVersion: number
    replayed: boolean
  }
}

async function fetchBlob(token: string, blobKey: string): Promise<Uint8Array> {
  const response = await app.fetch(new Request(`http://test.local/api/blobs/${blobKey}`, {
    headers: { Authorization: `Bearer ${token}` },
  }))
  assert.equal(response.status, 200)
  return new Uint8Array(await response.arrayBuffer())
}

test('batch creates preserve request order and exact opaque bytes', async () => {
  const login = await devLogin('creates')
  const collectionId = await createCollection(login.token)
  const blobs = ['first', 'second', 'third']
  const results = await uploadBatch(
    login.token,
    collectionId,
    blobs.map((blob) => createFrame(blob)),
  )

  assert.deepEqual(results.map((result) => result.status), ['created', 'created', 'created'])
  assert.deepEqual(results.map((result) => written(result).collectionVersion), [1, 2, 3])
  for (let index = 0; index < results.length; index++) {
    const result = written(results[index])
    assert.equal(result.object.version, '1')
    assert.equal(result.object.change_seq, String(index + 1))
    assert.equal(result.object.size_bytes, String(blobs[index].length))
    assert.deepEqual(
      await fetchBlob(login.token, result.object.blob_key),
      encoder.encode(blobs[index]),
    )
  }
})

test('batch and classic creates replay through the same Mutation ID system', async () => {
  const login = await devLogin('replay')
  const collectionId = await createCollection(login.token)
  const batchMutationId = 'batch.create_retry~v1'
  const classicMutationId = 'classic.create_retry~v1'

  const [batchCreated] = await uploadBatch(login.token, collectionId, [
    createFrame('batch original', batchMutationId),
  ])
  const classicReplay = await createSingle(
    login.token,
    collectionId,
    batchMutationId,
    'classic retry',
  )
  assert.equal(batchCreated.status, 'created')
  assert.equal(classicReplay.replayed, true)
  assert.equal(classicReplay.object.id, written(batchCreated).object.id)
  assert.deepEqual(
    await fetchBlob(login.token, classicReplay.object.blob_key),
    encoder.encode('batch original'),
  )
  const recoveredResponse = await app.fetch(new Request(
    `http://test.local/api/collections/${collectionId}/create-mutations/${batchMutationId}`,
    { headers: { Authorization: `Bearer ${login.token}` } },
  ))
  assert.equal(recoveredResponse.status, 200)
  const recovered = await recoveredResponse.json() as {
    object: ObjectRow
    collectionVersion: number
    replayed: boolean
  }
  assert.equal(recovered.replayed, true)
  assert.equal(recovered.object.id, written(batchCreated).object.id)

  const classicCreated = await createSingle(
    login.token,
    collectionId,
    classicMutationId,
    'classic original',
  )
  const [batchReplay] = await uploadBatch(login.token, collectionId, [
    createFrame('batch retry', classicMutationId),
  ])
  assert.equal(classicCreated.replayed, false)
  assert.equal(batchReplay.status, 'replayed')
  assert.equal(written(batchReplay).object.id, classicCreated.object.id)
  assert.deepEqual(
    await fetchBlob(login.token, written(batchReplay).object.blob_key),
    encoder.encode('classic original'),
  )
})

test('batch create Mutation IDs use transport-safe 1-128 character syntax', async () => {
  const login = await devLogin('mutation-id-syntax')
  const collectionId = await createCollection(login.token)
  const [maximum] = await uploadBatch(login.token, collectionId, [
    createFrame('maximum safe id', 'a'.repeat(128)),
  ])
  assert.equal(maximum.status, 'created')

  for (const mutationId of ['', 'a'.repeat(129), 'legacy:id+with space']) {
    const response = await batchRequest(
      login.token,
      collectionId,
      createFrame('invalid id', mutationId),
    )
    assert.equal(response.status, 400)
    assert.deepEqual(await response.json(), { error: 'invalid create mutation id' })
  }
})

test('classic retries and lookups preserve previously accepted Mutation IDs', async () => {
  const login = await devLogin('legacy-mutation-id')
  const collectionId = await createCollection(login.token)
  const mutationId = 'legacy:id+with space'
  const createUrl = `http://test.local/api/collections/${collectionId}/blob-objects`
  const firstResponse = await app.fetch(new Request(
    createUrl,
    {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${login.token}`,
        'Content-Type': 'application/octet-stream',
        'Mutation-Id': mutationId,
      },
      body: 'original body',
    },
  ))
  assert.equal(firstResponse.status, 201)
  const first = await firstResponse.json() as { object: ObjectRow; replayed: boolean }
  assert.equal(first.replayed, false)

  const replay = await createSingle(login.token, collectionId, mutationId, 'retry body')
  assert.equal(replay.replayed, true)
  assert.equal(replay.object.id, first.object.id)

  const lookup = await app.fetch(new Request(
    `http://test.local/api/collections/${collectionId}/create-mutations/${encodeURIComponent(mutationId)}`,
    { headers: { Authorization: `Bearer ${login.token}` } },
  ))
  assert.equal(lookup.status, 200)
  assert.equal((await lookup.json() as { object: ObjectRow }).object.id, first.object.id)
})

test('create lookup reports an in-progress Mutation ID without returning 404', async () => {
  const login = await devLogin('pending-mutation-id')
  const collectionId = await createCollection(login.token)
  const mutationId = uuidv7()
  await db
    .insertInto('mutation_results')
    .values({
      user_id: login.user.id,
      mutation_id: mutationId,
      kind: 'create',
      collection_id: collectionId,
      object_id: null,
      requested_version: null,
      result: { kind: 'pending' },
      created_at: new Date(),
    })
    .execute()

  const response = await app.fetch(new Request(
    `http://test.local/api/collections/${collectionId}/create-mutations/${mutationId}`,
    { headers: { Authorization: `Bearer ${login.token}` } },
  ))
  assert.equal(response.status, 409)
  assert.deepEqual(await response.json(), { error: 'mutation still in progress' })
})

test('Mutation IDs are tenant-scoped without making object IDs client-controlled', async () => {
  const first = await devLogin('tenant-a')
  const second = await devLogin('tenant-b')
  const firstCollection = await createCollection(first.token)
  const secondCollection = await createCollection(second.token)
  const mutationId = uuidv7()

  const [firstResult] = await uploadBatch(first.token, firstCollection, [
    createFrame('first tenant', mutationId),
  ])
  const [secondResult] = await uploadBatch(second.token, secondCollection, [
    createFrame('second tenant', mutationId),
  ])

  assert.equal(firstResult.status, 'created')
  assert.equal(secondResult.status, 'created')
  assert.notEqual(written(firstResult).object.id, written(secondResult).object.id)
})

test('batch maps successful updates, conflicts, and missing objects independently', async () => {
  const login = await devLogin('updates')
  const collectionId = await createCollection(login.token)
  const [seed] = await uploadBatch(login.token, collectionId, [createFrame('seed')])
  const current = written(seed).object

  const results = await uploadBatch(login.token, collectionId, [
    updateFrame(current.id, 2, 'updated'),
    updateFrame(current.id, 2, 'stale'),
    updateFrame(uuidv7(), 1, 'missing'),
    createFrame('after'),
  ])

  assert.deepEqual(
    results.map((result) => result.status),
    ['updated', 'conflict', 'not_found', 'created'],
  )
  assert.equal(written(results[0]).object.version, '2')
  const conflict = results[1]
  assert.equal(conflict.status, 'conflict')
  if (conflict.status === 'conflict') {
    assert.equal(conflict.currentVersion, 2)
    assert.equal(conflict.currentBlobKey, written(results[0]).object.blob_key)
  }
  assert.deepEqual(
    await fetchBlob(login.token, written(results[0]).object.blob_key),
    encoder.encode('updated'),
  )
})

test('batch mutations use the authoritative claimed, retained, and staged blob states', async () => {
  const login = await devLogin('ledger')
  const collectionId = await createCollection(login.token)
  const [created] = await uploadBatch(login.token, collectionId, [createFrame('version one')])
  const original = written(created).object
  const [updated, conflict] = await uploadBatch(login.token, collectionId, [
    updateFrame(original.id, 2, 'version two'),
    updateFrame(original.id, 2, 'stale version'),
  ])
  assert.equal(updated.status, 'updated')
  assert.equal(conflict.status, 'conflict')

  const rows = await db
    .selectFrom('blob_ledger')
    .where('user_id', '=', login.user.id)
    .select(['blob_key', 'state'])
    .execute()
  const states = new Map(rows.map((row) => [row.blob_key, row.state]))
  assert.equal(states.get(original.blob_key), 'retained')
  assert.equal(states.get(written(updated).object.blob_key), 'claimed')
  assert.equal(rows.filter((row) => row.state === 'staged').length, 1)
})

test('too-large and operational failures do not stop later entries', async () => {
  const login = await devLogin('isolated-failures')
  const collectionId = await createCollection(login.token)
  const originalPut = FsBlobStore.prototype.put
  let putCount = 0
  FsBlobStore.prototype.put = async function (key, data) {
    putCount += 1
    if (putCount === 1) throw new Error('sensitive storage detail')
    await originalPut.call(this, key, data)
  }

  try {
    const results = await uploadBatch(login.token, collectionId, [
      createFrame('fails'),
      createFrame(new Uint8Array(1025).fill(0xaa)),
      createFrame('succeeds'),
    ])
    assert.deepEqual(results[0], { status: 'error', error: 'internal server error' })
    assert.deepEqual(results[1], { status: 'too_large' })
    assert.equal(results[2].status, 'created')
  } finally {
    FsBlobStore.prototype.put = originalPut
  }
})

test('successful entries publish transactionally while conflicts and replays do not', async () => {
  const login = await devLogin('notifications')
  const collectionId = await createCollection(login.token)
  const mutationId = uuidv7()
  const events: number[] = []
  const originalPublish = notifier.publish
  notifier.publish = async (_trx, event) => {
    events.push(event.currentVersion)
  }

  try {
    const [created, second] = await uploadBatch(login.token, collectionId, [
      createFrame('first', mutationId),
      createFrame('second'),
    ])
    assert.deepEqual(events, [1, 2])

    events.length = 0
    const results = await uploadBatch(login.token, collectionId, [
      createFrame('retry', mutationId),
      updateFrame(written(created).object.id, 1, 'conflict'),
    ])
    assert.deepEqual(results.map((result) => result.status), ['replayed', 'conflict'])
    assert.deepEqual(events, [])
    assert.equal(written(second).collectionVersion, 2)
  } finally {
    notifier.publish = originalPublish
  }
})

test('malformed framing rejects the complete request', async () => {
  const login = await devLogin('malformed')
  const collectionId = await createCollection(login.token)
  const valid = createFrame('ok')
  const invalidBodies = [
    valid.subarray(0, valid.byteLength - 1),
    frame(2, uuidv7(), 0, 'x'),
    frame(0, '', 0, 'x'),
    frame(0, 'x'.repeat(129), 0, 'x'),
    frame(0, uuidv7(), 1, 'x'),
    frame(1, 'not-a-uuid', 1, 'x'),
    frame(1, uuidv7(), 0, 'x'),
    frame(0, uuidv7(), 0, new Uint8Array([1]), 0),
    concatBytes([valid, new Uint8Array([0])]),
  ]

  for (const body of invalidBodies) {
    const response = await batchRequest(login.token, collectionId, body)
    assert.equal(response.status, 400)
    assert.equal(typeof (await response.json() as { error: unknown }).error, 'string')
  }
})

test('batch entry and request byte limits reject before mutation', async () => {
  const login = await devLogin('limits')
  const collectionId = await createCollection(login.token)
  assert.equal(
    (await batchRequest(login.token, collectionId, new Uint8Array(0))).status,
    400,
  )

  const maximum = Array.from({ length: MAX_BATCH_ENTRIES }, () => createFrame('x'))
  const tooMany = await batchRequest(
    login.token,
    collectionId,
    concatBytes([...maximum, new Uint8Array([0xff])]),
  )
  assert.equal(tooMany.status, 400)
  assert.deepEqual(await tooMany.json(), {
    error: `too many entries (max ${MAX_BATCH_ENTRIES})`,
  })

  const oversized = frame(0, uuidv7(), 0, new Uint8Array(70000).fill(0xcc))
  assert.equal((await batchRequest(login.token, collectionId, oversized)).status, 413)
})

test('batch upload requires auth and collection ownership', async () => {
  const owner = await devLogin('owner')
  const caller = await devLogin('caller')
  const collectionId = await createCollection(owner.token)
  const body = createFrame('x')

  assert.equal((await batchRequest(null, collectionId, body)).status, 401)
  assert.equal((await batchRequest(caller.token, collectionId, body)).status, 404)
  assert.equal((await batchRequest(owner.token, uuidv7(), body)).status, 404)
})
