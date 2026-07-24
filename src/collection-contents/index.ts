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

type ObjectRecord = Pick<
  Selectable<ObjectsTable>,
  | 'id'
  | 'collection_id'
  | 'version'
  | 'change_seq'
  | 'deleted'
  | 'blob_key'
  | 'size_bytes'
  | 'created_at'
  | 'updated_at'
>

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

type CreateObjectCommand = ObjectMutationCommandBase & BlobSource & {
  kind: 'create'
}

type UpdateObjectCommand = ObjectMutationCommandBase & BlobSource & {
  kind: 'update'
  objectId: string
  version: number
}

type DeleteObjectCommand = ObjectMutationCommandBase & {
  kind: 'delete'
  objectId: string
  expectedVersion?: number
}

type ObjectMutationCommand =
  | CreateObjectCommand
  | UpdateObjectCommand
  | DeleteObjectCommand

export type ObjectMutationResult =
  | { kind: 'ok'; object: ObjectRecord; collectionVersion: number }
  | { kind: 'not_found' }
  | { kind: 'blob_not_staged' }
  | { kind: 'conflict'; currentVersion: number; currentBlobKey: string | null }
  | { kind: 'invalid_mutation_id' }
  | { kind: 'mutation_mismatch' }

function intentObjectId(command: ObjectMutationCommand): string | null {
  return command.kind === 'create' ? null : command.objectId
}

function intentVersion(command: ObjectMutationCommand): string | null {
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
    await this.store.put(blobKey, input.data)
    const now = this.now()
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
    return { blobKey, sizeBytes: input.data.byteLength }
  }

  async runMaintenance(): Promise<{
    reconciled: number
    stagedDeleted: number
    retainedDeleted: number
    purgeableDeleted: number
    mutationResultsDeleted: number
  }> {
    const reconciled = await this.reconcileUntrackedBlobs()
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
      reconciled,
      stagedDeleted,
      retainedDeleted,
      purgeableDeleted,
      mutationResultsDeleted: deletedMutationResults.length,
    }
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
    return await this.db.transaction().execute(async (trx) => {
      if (command.mutationId !== undefined) {
        if (command.mutationId.length < 1 || command.mutationId.length > 128) {
          return { kind: 'invalid_mutation_id' }
        }
        await sql`
          select pg_advisory_xact_lock(
            hashtextextended(${`${command.userId}:${command.mutationId}`}, 0)
          )
        `.execute(trx)
        const existing = await trx
          .selectFrom('mutation_results')
          .where('user_id', '=', command.userId)
          .where('mutation_id', '=', command.mutationId)
          .select([
            'kind',
            'collection_id',
            'object_id',
            'requested_version',
            'result',
          ])
          .executeTakeFirst()
        if (existing) {
          const matches = existing.kind === command.kind
            && existing.collection_id === command.collectionId
            && existing.object_id === intentObjectId(command)
            && existing.requested_version === intentVersion(command)
          return matches ? decodeResult(existing.result) : { kind: 'mutation_mismatch' }
        }
      }

      const result = await this.executeObjectMutation(trx, command)
      if (command.mutationId !== undefined) {
        await trx
          .insertInto('mutation_results')
          .values({
            user_id: command.userId,
            mutation_id: command.mutationId,
            kind: command.kind,
            collection_id: command.collectionId,
            object_id: intentObjectId(command),
            requested_version: intentVersion(command),
            result: encodeResult(result),
            created_at: this.now(),
          })
          .execute()
      }
      return result
    })
  }

  private async executeObjectMutation(
    trx: Transaction<Database>,
    command: ObjectMutationCommand,
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
          .select([
            'id',
            'collection_id',
            'version',
            'change_seq',
            'deleted',
            'blob_key',
            'size_bytes',
            'created_at',
            'updated_at',
          ])
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
          .returning([
            'id',
            'collection_id',
            'version',
            'change_seq',
            'deleted',
            'blob_key',
            'size_bytes',
            'created_at',
            'updated_at',
          ])
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

        const stagedBlob = await this.resolveBlob(trx, command)
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
          .returning([
            'id',
            'collection_id',
            'version',
            'change_seq',
            'deleted',
            'blob_key',
            'size_bytes',
            'created_at',
            'updated_at',
          ])
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

      const stagedBlob = await this.resolveBlob(trx, command)
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
        .returning([
          'id',
          'collection_id',
          'version',
          'change_seq',
          'deleted',
          'blob_key',
          'size_bytes',
          'created_at',
          'updated_at',
        ])
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
    const candidates = await this.db
      .selectFrom('blob_ledger')
      .where('state', '=', state)
      .where('state_changed_at', '<=', cutoff)
      .select('blob_key')
      .limit(MAINTENANCE_BATCH_SIZE)
      .execute()
    let deleted = 0
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
        if (removed) deleted += 1
      } catch (error) {
        // Keep the ledger row authoritative so the next cycle retries this
        // blob, while allowing unrelated candidates in the batch to progress.
        log.warn('blob-maintenance: delete failed', {
          blobKey: candidate.blob_key,
          state,
          error: String(error),
        })
      }
    }
    return deleted
  }

  private async resolveBlob(
    trx: Transaction<Database>,
    command: CreateObjectCommand | UpdateObjectCommand,
  ): Promise<{ blob_key: string; size_bytes: string } | undefined> {
    if (command.stagedBlobKey !== undefined) {
      return await trx
        .selectFrom('blob_ledger')
        .where('blob_key', '=', command.stagedBlobKey)
        .where('user_id', '=', command.userId)
        .where('state', '=', 'staged')
        .select(['blob_key', 'size_bytes'])
        .forUpdate()
        .executeTakeFirst()
    }

    const blobKey = `${command.userId}/${uuidv7()}`
    await this.store.put(blobKey, command.blobData)
    const now = this.now()
    return await trx
      .insertInto('blob_ledger')
      .values({
        blob_key: blobKey,
        user_id: command.userId,
        size_bytes: String(command.blobData.byteLength),
        state: 'staged',
        collection_id: null,
        object_id: null,
        object_version: null,
        created_at: now,
        state_changed_at: now,
      })
      .returning(['blob_key', 'size_bytes'])
      .executeTakeFirstOrThrow()
  }

  private async reconcileUntrackedBlobs(): Promise<number> {
    const keys = await this.store.list('')
    let reconciled = 0
    for (const blobKey of keys) {
      if (reconciled >= MAINTENANCE_BATCH_SIZE) break
      const [userId, blobId, extra] = blobKey.split('/')
      if (extra !== undefined || !UUID_RE.test(userId ?? '') || !UUID_RE.test(blobId ?? '')) {
        continue
      }
      const known = await this.db
        .selectFrom('blob_ledger')
        .where('blob_key', '=', blobKey)
        .where('user_id', '=', userId)
        .select('blob_key')
        .executeTakeFirst()
      if (known) continue
      const user = await this.db
        .selectFrom('users')
        .where('id', '=', userId)
        .select('id')
        .executeTakeFirst()
      if (!user) continue
      const data = await this.store.get(blobKey)
      if (!data) continue
      const now = this.now()
      const inserted = await this.db
        .insertInto('blob_ledger')
        .values({
          blob_key: blobKey,
          user_id: userId,
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
