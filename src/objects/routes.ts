import { Hono, type Context } from 'hono'
import { bodyLimit } from 'hono/body-limit'
import type { AuthContext } from '../auth/middleware.ts'
import {
  OBJECT_COLUMNS,
  type CollectionContents,
  type ObjectMutationOutcome,
  type ObjectMutationResult,
} from '../collection-contents/index.ts'
import { isValidMutationId } from '../collection-contents/mutation-id.ts'
import { db } from '../db/connection.ts'
import { env } from '../env.ts'
import { isUuidIdentifier } from '../identifiers/is-uuid-identifier.ts'
import { createBatchBlobObjectRoutes } from './batch-upload/routes.ts'
import { collectionBelongsToUser } from './collection-ownership.ts'

const MAX_PULL_LIMIT = 1000

const blobLimit = bodyLimit({
  maxSize: env.MAX_BLOB_BYTES,
  onError: (c) => c.json({ error: 'blob too large' }, 413),
})

interface CreateBody {
  blob_key?: string
  size_bytes?: number
}

interface UpdateBody {
  version?: number
  blob_key?: string
  size_bytes?: number
}

type AppContext = Context<{ Variables: AuthContext }>

function isNonNegativeSafeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

function isBlobKeyOwnedByUser(blobKey: unknown, userId: string): blobKey is string {
  if (typeof blobKey !== 'string') return false
  const [keyUserId, blobId, extra] = blobKey.split('/')
  return extra === undefined && keyUserId === userId && isUuidIdentifier(blobId)
}

function mutationResponse(
  c: AppContext,
  result: ObjectMutationResult,
  successStatus: 200 | 201 = 200,
): Response {
  switch (result.kind) {
    case 'ok':
      return c.json(
        { object: result.object, collectionVersion: result.collectionVersion },
        successStatus,
      )
    case 'not_found':
      return c.json({ error: 'not found' }, 404)
    case 'blob_not_staged':
      return c.json({ error: 'blob is not staged' }, 409)
    case 'conflict':
      return c.json({
        error: 'version conflict',
        currentVersion: result.currentVersion,
        currentBlobKey: result.currentBlobKey,
      }, 409)
    case 'invalid_mutation_id':
      return c.json({ error: 'invalid Mutation-Id' }, 400)
    case 'mutation_mismatch':
      return c.json({ error: 'Mutation-Id reused for different intent' }, 409)
  }
}

function createMutationResponse(
  c: AppContext,
  outcome: ObjectMutationOutcome,
  successStatus: 200 | 201 = 201,
): Response {
  if (outcome.result.kind !== 'ok') return mutationResponse(c, outcome.result)
  return c.json({
    object: outcome.result.object,
    collectionVersion: outcome.result.collectionVersion,
    replayed: outcome.replayed,
  }, successStatus)
}

function deleteMutationResponse(
  c: AppContext,
  result: ObjectMutationResult,
): Response {
  if (result.kind !== 'ok') return mutationResponse(c, result)
  return c.json({
    object: {
      id: result.object.id,
      version: result.object.version,
      change_seq: result.object.change_seq,
      deleted: result.object.deleted,
    },
    collectionVersion: result.collectionVersion,
  })
}

export function createObjectsRoutes(
  contents: CollectionContents,
): Hono<{ Variables: AuthContext }> {
  const routes = new Hono<{ Variables: AuthContext }>()
  routes.route('/', createBatchBlobObjectRoutes(contents))

  routes.get('/:cid/create-mutations/:mutationId', async (c) => {
    const mutationId = c.req.param('mutationId')
    if (!isValidMutationId(mutationId)) {
      return c.json({ error: 'invalid Mutation-Id' }, 400)
    }
    const outcome = await contents.getCreateMutationOutcome({
      userId: c.var.user.id,
      collectionId: c.req.param('cid'),
      mutationId,
    })
    if (outcome.kind === 'missing') return c.json({ error: 'not found' }, 404)
    if (outcome.kind === 'pending') {
      return c.json({ error: 'mutation still in progress' }, 409)
    }
    return createMutationResponse(c, outcome.outcome, 200)
  })

  routes.get('/:cid/objects', async (c) => {
    const userId = c.var.user.id
    const cid = c.req.param('cid')
    if (!(await collectionBelongsToUser({ userId, collectionId: cid }))) {
      return c.json({ error: 'not found' }, 404)
    }
    const sinceVersion = Number(c.req.query('sinceVersion') ?? 0)
    if (!isNonNegativeSafeInteger(sinceVersion)) {
      return c.json({ error: 'invalid sinceVersion' }, 400)
    }

    const limitRaw = c.req.query('limit')
    let limit: number | null = null
    if (limitRaw !== undefined) {
      const parsed = Number(limitRaw)
      if (!Number.isSafeInteger(parsed) || parsed < 1) {
        return c.json({ error: 'invalid limit' }, 400)
      }
      limit = Math.min(parsed, MAX_PULL_LIMIT)
    }

    const query = db
      .selectFrom('objects')
      .where('collection_id', '=', cid)
      .where('user_id', '=', userId)
      .where('change_seq', '>', String(sinceVersion))
      .select(OBJECT_COLUMNS)
      .orderBy('change_seq', 'asc')

    if (limit !== null) {
      const rows = await query.limit(limit + 1).execute()
      const hasMore = rows.length > limit
      const page = hasMore ? rows.slice(0, limit) : rows
      const nextCursor = page.length > 0
        ? Number(page[page.length - 1].change_seq)
        : sinceVersion
      return c.json({ objects: page, hasMore, nextCursor })
    }

    return c.json({ objects: await query.execute() })
  })

  routes.get('/:cid/objects/:oid', async (c) => {
    const row = await db
      .selectFrom('objects')
      .where('id', '=', c.req.param('oid'))
      .where('collection_id', '=', c.req.param('cid'))
      .where('user_id', '=', c.var.user.id)
      .select(OBJECT_COLUMNS)
      .executeTakeFirst()
    if (!row) return c.json({ error: 'not found' }, 404)
    return c.json({ object: row })
  })

  routes.post('/:cid/objects', async (c) => {
    const userId = c.var.user.id
    let body: CreateBody
    try {
      body = await c.req.json()
    } catch {
      return c.json({ error: 'invalid json' }, 400)
    }
    if (
      !isBlobKeyOwnedByUser(body.blob_key, userId)
      || !isNonNegativeSafeInteger(body.size_bytes)
    ) {
      return c.json({ error: 'valid blob_key and size_bytes required' }, 400)
    }
    const outcome = await contents.mutateObjectWithReplay({
      kind: 'create',
      userId,
      collectionId: c.req.param('cid'),
      stagedBlobKey: body.blob_key,
      mutationId: c.req.header('Mutation-Id'),
    })
    return createMutationResponse(c, outcome)
  })

  routes.put('/:cid/objects/:oid', async (c) => {
    const userId = c.var.user.id
    let body: UpdateBody
    try {
      body = await c.req.json()
    } catch {
      return c.json({ error: 'invalid json' }, 400)
    }
    if (
      typeof body.version !== 'number'
      || !Number.isSafeInteger(body.version)
      || body.version < 1
      || !isBlobKeyOwnedByUser(body.blob_key, userId)
      || !isNonNegativeSafeInteger(body.size_bytes)
    ) {
      return c.json({ error: 'valid version, blob_key, size_bytes required' }, 400)
    }
    const result = await contents.mutateObject({
      kind: 'update',
      userId,
      collectionId: c.req.param('cid'),
      objectId: c.req.param('oid'),
      version: body.version,
      stagedBlobKey: body.blob_key,
      mutationId: c.req.header('Mutation-Id'),
    })
    return mutationResponse(c, result)
  })

  routes.delete('/:cid/objects/:oid', async (c) => {
    const expectedRaw = c.req.query('version')
    let expectedVersion: number | undefined
    if (expectedRaw !== undefined) {
      const parsed = Number(expectedRaw)
      if (!Number.isSafeInteger(parsed) || parsed < 1) {
        return c.json({ error: 'invalid version' }, 400)
      }
      expectedVersion = parsed
    }
    const result = await contents.mutateObject({
      kind: 'delete',
      userId: c.var.user.id,
      collectionId: c.req.param('cid'),
      objectId: c.req.param('oid'),
      expectedVersion,
      mutationId: c.req.header('Mutation-Id'),
    })
    return deleteMutationResponse(c, result)
  })

  routes.post('/:cid/blob-objects', blobLimit, async (c) => {
    const body = new Uint8Array(await c.req.arrayBuffer())
    if (body.byteLength === 0) {
      return c.json({ error: 'empty body' }, 400)
    }
    const outcome = await contents.mutateObjectWithReplay({
      kind: 'create',
      userId: c.var.user.id,
      collectionId: c.req.param('cid'),
      blobData: body,
      mutationId: c.req.header('Mutation-Id'),
    })
    return createMutationResponse(c, outcome)
  })

  routes.put('/:cid/blob-objects/:oid', blobLimit, async (c) => {
    const version = Number(c.req.query('version'))
    if (!Number.isSafeInteger(version) || version < 1) {
      return c.json({ error: 'valid ?version query required' }, 400)
    }
    const body = new Uint8Array(await c.req.arrayBuffer())
    if (body.byteLength === 0) {
      return c.json({ error: 'empty body' }, 400)
    }
    const result = await contents.mutateObject({
      kind: 'update',
      userId: c.var.user.id,
      collectionId: c.req.param('cid'),
      objectId: c.req.param('oid'),
      version,
      blobData: body,
      mutationId: c.req.header('Mutation-Id'),
    })
    return mutationResponse(c, result)
  })

  return routes
}
