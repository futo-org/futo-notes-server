import { type Kysely, sql } from 'kysely'

/**
 * Enforce one vault (collection) per account.
 *
 * The client protocol is single-vault ("take the first collection or create
 * one"), but the server let one account hold several collections, so two
 * devices setting up sync concurrently could each mint their own vault and
 * never see each other's notes (silent split-brain).
 *
 * This collapses any pre-existing split — keeping the EARLIEST collection per
 * user (which is exactly the one the client's connect() picks), deleting the
 * rest. `objects.collection_id` is `ON DELETE CASCADE`, so a loser vault's
 * objects go with it; a device that was pinned to a deleted vault re-pushes its
 * local notes to the survivor on its next sync (the existing reset→reconcile
 * pipeline). Then it adds `UNIQUE(user_id)` so a second vault can never be
 * created for an account again.
 */
export async function up(db: Kysely<unknown>): Promise<void> {
  // Record every blob a loser vault's objects reference as orphaned BEFORE the
  // delete cascades those object rows away — otherwise the blobKeys are lost and
  // the blob GC (src/maintenance/blobGc.ts) can never reclaim the files. Mirrors
  // the DELETE /api/collections/:id handler. A "loser" is any collection with an
  // earlier sibling for the same user (same predicate as the delete below).
  await sql`
    insert into orphaned_blobs (blob_key, user_id, size_bytes)
    select o.blob_key, o.user_id, coalesce(o.size_bytes, 0)
    from objects o
    join collections c on c.id = o.collection_id
    where o.blob_key is not null
      and exists (
        select 1 from collections other
        where other.user_id = c.user_id
          and (other.created_at < c.created_at
               or (other.created_at = c.created_at and other.id < c.id))
      )
    on conflict (blob_key) do nothing
  `.execute(db)

  const result = await sql`
    delete from collections c
    where exists (
      select 1 from collections other
      where other.user_id = c.user_id
        and (other.created_at < c.created_at
             or (other.created_at = c.created_at and other.id < c.id))
    )
  `.execute(db)
  const removed = Number(result.numAffectedRows ?? 0n)
  if (removed > 0) {
    // eslint-disable-next-line no-console
    console.log(
      `[migration 008] collapsed ${removed} duplicate collection(s) to enforce one vault per account`,
    )
  }

  // The non-unique user index becomes redundant once user_id is UNIQUE.
  await db.schema.dropIndex('idx_collections_user').execute()
  await db.schema
    .alterTable('collections')
    .addUniqueConstraint('collections_user_id_unique', ['user_id'])
    .execute()
}

export async function down(db: Kysely<unknown>): Promise<void> {
  await db.schema
    .alterTable('collections')
    .dropConstraint('collections_user_id_unique')
    .execute()
  await db.schema
    .createIndex('idx_collections_user')
    .on('collections')
    .column('user_id')
    .execute()
}
