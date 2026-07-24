import type { Kysely, Selectable, Transaction } from 'kysely'
import { sql } from 'kysely'
import { uuidv7 } from 'uuidv7'
import type { BlobStore } from '../blob/interface.ts'
import type { Database, ObjectsTable } from '../db/types.ts'
import { log } from '../logger.ts'
import type { Notifier } from '../sync/notifier.ts'

const STAGED_BLOB_RETENTION_MS = 24 * 60 * 60 * 1000
const RETAINED_BLOB_RETENTION_MS = 365 * 24 * 60 * 60 * 1000
const MUTATION_RESULT_RETENTION_MS = 30 * 24 * 60 * 60 * 1000
const MAINTENANCE_BATCH_SIZE = 500
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

// The one projection for object rows. Every read and write path selects exactly
// these columns, so the shape cannot drift between them — and this is the single
// place a change to the wire representation of the bigint fields would land.
export const OBJECT_COLUMNS = [
  'id',
  'collection_id',
  'version',
  'change_seq',
  'deleted',
  'blob_key',
  'size_bytes',
  'created_at',
  'updated_at',
] as const

type ObjectRecord = Pick<Selectable<ObjectsTable>, (typeof OBJECT_COLUMNS)[number]>

interface CollectionContentsDependencies {
  db: Kysely<Database>
  store: BlobStore
  notifier: Notifier
  now?: () => Date
}

interface ObjectMutationCommandBase {
  userId: string
  collectionId: string
  mutationId?: string
}

type BlobSource =
  | { stagedBlobKey: string; blobData?: never }
  | { stagedBlobKey?: never; blobData: Uint8Array }

interface CreateObjectIntent {
  kind: 'create'
}

interface UpdateObjectIntent {
  kind: 'update'
  objectId: string
  version: number
}

type CreateObjectCommand = ObjectMutationCommandBase & CreateObjectIntent & BlobSource

type UpdateObjectCommand = ObjectMutationCommandBase & UpdateObjectIntent & BlobSource

type DeleteObjectCommand = ObjectMutationCommandBase & {
  kind: 'delete'
  objectId: string
  expectedVersion?: number
}

type ObjectMutationCommand =
  | CreateObjectCommand
  | UpdateObjectCommand
  | DeleteObjectCommand

// The same commands once inline ciphertext has been staged, which happens
// outside the mutation transaction. Every create and update now names an
// already-staged blob key, so claiming it is a row lock and nothing more.
type StagedObjectMutationCommand =
  | (ObjectMutationCommandBase & CreateObjectIntent & { stagedBlobKey: string })
  | (ObjectMutationCommandBase & UpdateObjectIntent & { stagedBlobKey: string })
  | DeleteObjectCommand

export interface BlobCollectionResult {
  stagedDeleted: number
  retainedDeleted: number
  purgeableDeleted: number
  mutationResultsDeleted: number
}

export type ObjectMutationResult =
  | { kind: 'ok'; object: ObjectRecord; collectionVersion: number }
  | { kind: 'not_found' }
  | { kind: 'blob_not_staged' }
  | { kind: 'conflict'; currentVersion: number; currentBlobKey: string | null }
  | { kind: 'invalid_mutation_id' }
  | { kind: 'mutation_mismatch' }

function intentObjectId(command: StagedObjectMutationCommand): string | null {
  return command.kind === 'create' ? null : command.objectId
}

function intentVersion(command: StagedObjectMutationCommand): string | null {
  if (command.kind === 'update') return String(command.version)
  if (command.kind === 'delete' && command.expectedVersion !== undefined) {
    return String(command.expectedVersion)
  }
  return null
}

function encodeResult(result: ObjectMutationResult): Record<string, unknown> {
  return JSON.parse(JSON.stringify(result)) as Record<string, unknown>
}

function decodeResult(value: Record<string, unknown>): ObjectMutationResult {
  if (value.kind === 'ok') {
    const object = value.object as ObjectRecord
    return {
      kind: 'ok',
      object: {
        ...object,
        created_at: new Date(object.created_at),
        updated_at: new Date(object.updated_at),
      },
      collectionVersion: value.collectionVersion as number,
    }
  }
  return value as ObjectMutationResult
}

export class CollectionContents {
  private readonly db: Kysely<Database>
  private readonly store: BlobStore
  private readonly notifier: Notifier
  private readonly now: () => Date

  constructor(dependencies: CollectionContentsDependencies) {
    this.db = dependencies.db
    this.store = dependencies.store
    this.notifier = dependencies.notifier
    this.now = dependencies.now ?? (() => new Date())
  }

  async stageBlob(input: {
    userId: string
    data: Uint8Array
  }): Promise<{ blobKey: string; sizeBytes: number }> {
    const blobKey = `${input.userId}/${uuidv7()}`
    const now = this.now()
    // Record the ledger row BEFORE writing bytes. The reverse order leaves a
    // window where the file exists untracked, and a reconciliation pass landing
    // inside it adopts the key first, making this insert fail on the primary
    // key and turning a valid upload into a 500. A row whose bytes never
    // arrived is harmless by comparison: cleanup ignores a missing file and the
    // row ages out on the staging window.
    await this.db
      .insertInto('blob_ledger')
      .values({
        blob_key: blobKey,
        user_id: input.userId,
        size_bytes: String(input.data.byteLength),
        state: 'staged',
        collection_id: null,
        object_id: null,
        object_version: null,
        created_at: now,
        state_changed_at: now,
      })
      .execute()
    await this.store.put(blobKey, input.data)
    return { blobKey, sizeBytes: input.data.byteLength }
  }

  // Repair, not collection: adopts untracked storage into the ledger and never
  // deletes anything. Safe to run when destructive cleanup is switched off, and
  // it must be — it is the only path by which a blob uploaded before the ledger
  // existed becomes claimable again.
  async reconcileStorage(): Promise<{ reconciled: number }> {
    return { reconciled: await this.reconcileUntrackedBlobs() }
  }

  // The destructive half: everything here deletes storage bytes or rows.
  async collectGarbage(): Promise<BlobCollectionResult> {
    const stagedCutoff = new Date(this.now().getTime() - STAGED_BLOB_RETENTION_MS)
    const retainedCutoff = new Date(this.now().getTime() - RETAINED_BLOB_RETENTION_MS)
    const mutationCutoff = new Date(this.now().getTime() - MUTATION_RESULT_RETENTION_MS)
    const stagedDeleted = await this.deleteEligibleBlobs('staged', stagedCutoff)
    const retainedDeleted = await this.deleteEligibleBlobs('retained', retainedCutoff)
    const purgeableDeleted = await this.deleteEligibleBlobs('purgeable', this.now())
    const deletedMutationResults = await this.db
      .deleteFrom('mutation_results')
      .where('created_at', '<=', mutationCutoff)
      .returning('mutation_id')
      .execute()
    return {
      stagedDeleted,
      retainedDeleted,
      purgeableDeleted,
      mutationResultsDeleted: deletedMutationResults.length,
    }
  }

  // Both halves, in order. Convenience for callers that want a full cycle.
  async runMaintenance(): Promise<{ reconciled: number } & BlobCollectionResult> {
    const { reconciled } = await this.reconcileStorage()
    return { reconciled, ...(await this.collectGarbage()) }
  }

  async deleteCollection(input: {
    userId: string
    collectionId: string
  }): Promise<{ kind: 'deleted' | 'not_found' }> {
    return await this.db.transaction().execute(async (trx) => {
      const collection = await trx
        .selectFrom('collections')
        .where('id', '=', input.collectionId)
        .where('user_id', '=', input.userId)
        .select('id')
        .forUpdate()
        .executeTakeFirst()
      if (!collection) return { kind: 'not_found' }

      await trx
        .updateTable('blob_ledger')
        .set({
          state: 'purgeable',
          object_id: null,
          object_version: null,
          state_changed_at: this.now(),
        })
        .where('user_id', '=', input.userId)
        .where('collection_id', '=', input.collectionId)
        .where('state', 'in', ['claimed', 'retained'])
        .execute()
      await trx
        .deleteFrom('collections')
        .where('id', '=', input.collectionId)
        .where('user_id', '=', input.userId)
        .executeTakeFirstOrThrow()
      return { kind: 'deleted' }
    })
  }

  async deleteStagedBlob(input: {
    userId: string
    blobKey: string
  }): Promise<{ kind: 'deleted' | 'missing' | 'in_use' }> {
    return await this.db.transaction().execute(async (trx) => {
      const blob = await trx
        .selectFrom('blob_ledger')
        .where('blob_key', '=', input.blobKey)
        .where('user_id', '=', input.userId)
        .select(['blob_key', 'state'])
        .forUpdate()
        .executeTakeFirst()
      if (!blob) return { kind: 'missing' }
      if (blob.state !== 'staged') return { kind: 'in_use' }

      await this.store.delete(blob.blob_key)
      await trx
        .deleteFrom('blob_ledger')
        .where('blob_key', '=', blob.blob_key)
        .where('user_id', '=', input.userId)
        .where('state', '=', 'staged')
        .execute()
      return { kind: 'deleted' }
    })
  }

  async mutateObject(
    command: ObjectMutationCommand,
  ): Promise<ObjectMutationResult> {
    if (
      command.mutationId !== undefined
      && (command.mutationId.length < 1 || command.mutationId.length > 128)
    ) {
      return { kind: 'invalid_mutation_id' }
    }

    // Stage inline ciphertext before opening the mutation transaction. Writing
    // to blob storage inside it would hold the collection's row lock — and a
    // pooled connection — for the length of a multi-megabyte upload, serializing
    // every other mutation in the collection behind that I/O. A staged blob the
    // mutation then declines to claim (conflict, replay, missing object) simply
    // expires on the normal 24-hour staging window.
    const staged = await this.stageInlineBlob(command)

    return await this.db.transaction().execute(async (trx) => {
      if (staged.mutationId !== undefined) {
        await sql`
          select pg_advisory_xact_lock(
            hashtextextended(${`${staged.userId}:${staged.mutationId}`}, 0)
          )
        `.execute(trx)
        const existing = await trx
          .selectFrom('mutation_results')
          .where('user_id', '=', staged.userId)
          .where('mutation_id', '=', staged.mutationId)
          .select([
            'kind',
            'collection_id',
            'object_id',
            'requested_version',
            'result',
          ])
          .executeTakeFirst()
        if (existing) {
          const matches = existing.kind === staged.kind
            && existing.collection_id === staged.collectionId
            && existing.object_id === intentObjectId(staged)
            && existing.requested_version === intentVersion(staged)
          return matches ? decodeResult(existing.result) : { kind: 'mutation_mismatch' }
        }
      }

      const result = await this.executeObjectMutation(trx, staged)
      if (staged.mutationId !== undefined) {
        await trx
          .insertInto('mutation_results')
          .values({
            user_id: staged.userId,
            mutation_id: staged.mutationId,
            kind: staged.kind,
            collection_id: staged.collectionId,
            object_id: intentObjectId(staged),
            requested_version: intentVersion(staged),
            result: encodeResult(result),
            created_at: this.now(),
          })
          .execute()
      }
      return result
    })
  }

  // Turn a one-call mutation carrying raw ciphertext into the equivalent
  // two-call mutation against a staged blob key.
  private async stageInlineBlob(
    command: ObjectMutationCommand,
  ): Promise<StagedObjectMutationCommand> {
    if (command.kind === 'delete') return command
    const { userId, collectionId, mutationId } = command
    const stagedBlobKey = command.stagedBlobKey !== undefined
      ? command.stagedBlobKey
      : (await this.stageBlob({ userId, data: command.blobData })).blobKey
    if (command.kind === 'create') {
      return { userId, collectionId, mutationId, kind: 'create', stagedBlobKey }
    }
    return {
      userId,
      collectionId,
      mutationId,
      kind: 'update',
      objectId: command.objectId,
      version: command.version,
      stagedBlobKey,
    }
  }

  private async executeObjectMutation(
    trx: Transaction<Database>,
    command: StagedObjectMutationCommand,
  ): Promise<ObjectMutationResult> {
    const collection = await trx
      .selectFrom('collections')
      .where('id', '=', command.collectionId)
      .where('user_id', '=', command.userId)
      .select('id')
      .forUpdate()
      .executeTakeFirst()
    if (!collection) return { kind: 'not_found' }

    if (command.kind === 'delete') {
      const current = await trx
        .selectFrom('objects')
        .where('id', '=', command.objectId)
        .where('collection_id', '=', command.collectionId)
        .where('user_id', '=', command.userId)
        .select(OBJECT_COLUMNS)
        .forUpdate()
        .executeTakeFirst()
      if (!current) return { kind: 'not_found' }
      if (current.deleted) {
        return {
          kind: 'ok',
          object: current,
          collectionVersion: Number(current.change_seq),
        }
      }
      if (
        command.expectedVersion !== undefined
        && Number(current.version) !== command.expectedVersion
      ) {
        return {
          kind: 'conflict',
          currentVersion: Number(current.version),
          currentBlobKey: current.blob_key,
        }
      }

      const collectionRow = await trx
        .updateTable('collections')
        .set({ current_version: sql`current_version + 1` })
        .where('id', '=', command.collectionId)
        .where('user_id', '=', command.userId)
        .returning('current_version')
        .executeTakeFirstOrThrow()
      const object = await trx
        .updateTable('objects')
        .set({
          deleted: true,
          version: sql`version + 1`,
          change_seq: collectionRow.current_version,
          updated_at: this.now(),
        })
        .where('id', '=', command.objectId)
        .where('collection_id', '=', command.collectionId)
        .where('user_id', '=', command.userId)
        .returning(OBJECT_COLUMNS)
        .executeTakeFirstOrThrow()

      if (object.blob_key) {
        await trx
          .updateTable('blob_ledger')
          .set({ object_version: object.version })
          .where('blob_key', '=', object.blob_key)
          .where('user_id', '=', command.userId)
          .where('state', '=', 'claimed')
          .execute()
      }
      await this.notifier.publish(trx, {
        userId: command.userId,
        collectionId: command.collectionId,
        currentVersion: Number(collectionRow.current_version),
      })
      return {
        kind: 'ok',
        object,
        collectionVersion: Number(collectionRow.current_version),
      }
    }

    if (command.kind === 'update') {
      const current = await trx
        .selectFrom('objects')
        .where('id', '=', command.objectId)
        .where('collection_id', '=', command.collectionId)
        .where('user_id', '=', command.userId)
        .select(['id', 'version', 'blob_key'])
        .forUpdate()
        .executeTakeFirst()
      if (!current) return { kind: 'not_found' }
      if (Number(current.version) !== command.version - 1) {
        return {
          kind: 'conflict',
          currentVersion: Number(current.version),
          currentBlobKey: current.blob_key,
        }
      }

      const stagedBlob = await this.lockStagedBlob(trx, command)
      if (!stagedBlob) return { kind: 'blob_not_staged' }

      const collectionRow = await trx
        .updateTable('collections')
        .set({ current_version: sql`current_version + 1` })
        .where('id', '=', command.collectionId)
        .where('user_id', '=', command.userId)
        .returning('current_version')
        .executeTakeFirstOrThrow()
      const now = this.now()
      const object = await trx
        .updateTable('objects')
        .set({
          version: String(command.version),
          change_seq: collectionRow.current_version,
          blob_key: stagedBlob.blob_key,
          size_bytes: stagedBlob.size_bytes,
          updated_at: now,
        })
        .where('id', '=', command.objectId)
        .where('collection_id', '=', command.collectionId)
        .where('user_id', '=', command.userId)
        .returning(OBJECT_COLUMNS)
        .executeTakeFirstOrThrow()

      if (current.blob_key) {
        await trx
          .updateTable('blob_ledger')
          .set({
            state: 'retained',
            collection_id: command.collectionId,
            object_id: null,
            object_version: null,
            state_changed_at: now,
          })
          .where('blob_key', '=', current.blob_key)
          .where('user_id', '=', command.userId)
          .where('state', '=', 'claimed')
          .execute()
      }
      await trx
        .updateTable('blob_ledger')
        .set({
          state: 'claimed',
          collection_id: command.collectionId,
          object_id: object.id,
          object_version: object.version,
          state_changed_at: now,
        })
        .where('blob_key', '=', stagedBlob.blob_key)
        .where('user_id', '=', command.userId)
        .where('state', '=', 'staged')
        .executeTakeFirstOrThrow()

      await this.notifier.publish(trx, {
        userId: command.userId,
        collectionId: command.collectionId,
        currentVersion: Number(collectionRow.current_version),
      })
      return {
        kind: 'ok',
        object,
        collectionVersion: Number(collectionRow.current_version),
      }
    }

    const stagedBlob = await this.lockStagedBlob(trx, command)
    if (!stagedBlob) return { kind: 'blob_not_staged' }

    const collectionRow = await trx
      .updateTable('collections')
      .set({ current_version: sql`current_version + 1` })
      .where('id', '=', command.collectionId)
      .where('user_id', '=', command.userId)
      .returning('current_version')
      .executeTakeFirstOrThrow()

    const object = await trx
      .insertInto('objects')
      .values({
        id: uuidv7(),
        collection_id: command.collectionId,
        user_id: command.userId,
        blob_key: stagedBlob.blob_key,
        size_bytes: stagedBlob.size_bytes,
        change_seq: collectionRow.current_version,
      })
      .returning(OBJECT_COLUMNS)
      .executeTakeFirstOrThrow()

    await trx
      .updateTable('blob_ledger')
      .set({
        state: 'claimed',
        collection_id: command.collectionId,
        object_id: object.id,
        object_version: object.version,
        state_changed_at: this.now(),
      })
      .where('blob_key', '=', stagedBlob.blob_key)
      .where('user_id', '=', command.userId)
      .where('state', '=', 'staged')
      .executeTakeFirstOrThrow()

    await this.notifier.publish(trx, {
      userId: command.userId,
      collectionId: command.collectionId,
      currentVersion: Number(collectionRow.current_version),
    })
    return {
      kind: 'ok',
      object,
      collectionVersion: Number(collectionRow.current_version),
    }
  }

  private async deleteEligibleBlobs(
    state: 'staged' | 'retained' | 'purgeable',
    cutoff: Date,
  ): Promise<number> {
    let deleted = 0
    // Drain rather than trimming a single batch per cycle: deleting one
    // collection can make tens of thousands of blobs purgeable at once, and at
    // the default six-hour interval a one-batch sweep would take days to
    // reclaim them.
    while (true) {
      const candidates = await this.db
        .selectFrom('blob_ledger')
        .where('state', '=', state)
        .where('state_changed_at', '<=', cutoff)
        .select('blob_key')
        .limit(MAINTENANCE_BATCH_SIZE)
        .execute()
      if (candidates.length === 0) break

      let removedInBatch = 0
      let failedInBatch = 0
      for (const candidate of candidates) {
        try {
          const removed = await this.db.transaction().execute(async (trx) => {
            const row = await trx
              .selectFrom('blob_ledger')
              .where('blob_key', '=', candidate.blob_key)
              .where('state', '=', state)
              .where('state_changed_at', '<=', cutoff)
              .select('blob_key')
              .forUpdate()
              .executeTakeFirst()
            if (!row) return false
            await this.store.delete(row.blob_key)
            await trx
              .deleteFrom('blob_ledger')
              .where('blob_key', '=', row.blob_key)
              .where('state', '=', state)
              .execute()
            return true
          })
          if (removed) removedInBatch += 1
        } catch (error) {
          failedInBatch += 1
          // Keep the ledger row authoritative so the next cycle retries this
          // blob, while allowing unrelated candidates in the batch to progress.
          log.warn('blob-maintenance: delete failed', {
            blobKey: candidate.blob_key,
            state,
            error: String(error),
          })
        }
      }
      deleted += removedInBatch

      // Nothing moved, so the same rows would be selected forever. Leave them
      // for the next cycle instead of spinning on them.
      if (removedInBatch === 0) {
        if (failedInBatch > 0) {
          log.warn('blob-maintenance: batch made no progress', {
            state,
            candidates: candidates.length,
            failed: failedInBatch,
          })
        }
        break
      }
      if (candidates.length < MAINTENANCE_BATCH_SIZE) break
    }
    return deleted
  }

  private async lockStagedBlob(
    trx: Transaction<Database>,
    command: { userId: string; stagedBlobKey: string },
  ): Promise<{ blob_key: string; size_bytes: string } | undefined> {
    // Enforce the 24-hour claim window here rather than trusting the sweeper to
    // have run — it may be hours late, or deletion may be disabled entirely.
    // Cleanup deletes staged rows at `state_changed_at <= cutoff`, so claiming
    // requires strictly newer: exactly one of the two ever applies to a row.
    const cutoff = new Date(this.now().getTime() - STAGED_BLOB_RETENTION_MS)
    return await trx
      .selectFrom('blob_ledger')
      .where('blob_key', '=', command.stagedBlobKey)
      .where('user_id', '=', command.userId)
      .where('state', '=', 'staged')
      .where('state_changed_at', '>', cutoff)
      .select(['blob_key', 'size_bytes'])
      .forUpdate()
      .executeTakeFirst()
  }

  private async reconcileUntrackedBlobs(): Promise<number> {
    // Only keys the server itself could have minted are candidates.
    const candidates: Array<{ blobKey: string; userId: string }> = []
    for (const blobKey of await this.store.list('')) {
      const [userId, blobId, extra] = blobKey.split('/')
      if (userId === undefined || blobId === undefined || extra !== undefined) continue
      if (!UUID_RE.test(userId) || !UUID_RE.test(blobId)) continue
      candidates.push({ blobKey, userId })
    }

    // Resolve ledger membership in chunks, stopping once a full batch of
    // untracked keys is in hand. Probing one key per round trip cost a query
    // per stored blob every cycle, even with nothing to reconcile.
    const untracked: Array<{ blobKey: string; userId: string }> = []
    let capped = false
    for (let i = 0; i < candidates.length && !capped; i += MAINTENANCE_BATCH_SIZE) {
      const chunk = candidates.slice(i, i + MAINTENANCE_BATCH_SIZE)
      const rows = await this.db
        .selectFrom('blob_ledger')
        .where('blob_key', 'in', chunk.map((candidate) => candidate.blobKey))
        .select('blob_key')
        .execute()
      const known = new Set(rows.map((row) => row.blob_key))
      for (const candidate of chunk) {
        if (known.has(candidate.blobKey)) continue
        untracked.push(candidate)
        if (untracked.length >= MAINTENANCE_BATCH_SIZE) {
          capped = true
          break
        }
      }
    }
    if (untracked.length === 0) return 0
    if (capped) {
      log.info('blob-maintenance: reconcile batch full, remainder deferred', {
        batchSize: MAINTENANCE_BATCH_SIZE,
      })
    }

    // A file whose owner no longer exists can never be claimed, so leave it
    // alone rather than staging it against a dangling user_id.
    const ownerRows = await this.db
      .selectFrom('users')
      .where('id', 'in', [...new Set(untracked.map((candidate) => candidate.userId))])
      .select('id')
      .execute()
    const owners = new Set(ownerRows.map((row) => row.id))

    let reconciled = 0
    for (const candidate of untracked) {
      if (!owners.has(candidate.userId)) continue
      const data = await this.store.get(candidate.blobKey)
      if (!data) continue
      const now = this.now()
      const inserted = await this.db
        .insertInto('blob_ledger')
        .values({
          blob_key: candidate.blobKey,
          user_id: candidate.userId,
          size_bytes: String(data.byteLength),
          state: 'staged',
          collection_id: null,
          object_id: null,
          object_version: null,
          created_at: now,
          state_changed_at: now,
        })
        .onConflict((oc) => oc.column('blob_key').doNothing())
        .returning('blob_key')
        .executeTakeFirst()
      if (inserted) reconciled += 1
    }
    return reconciled
  }
}
