import { type Kysely } from 'kysely'

/**
 * Reserved migration number.
 *
 * This migration briefly enforced one collection per user before the change
 * reached a stable release. Stable upgrades may still discover this filename,
 * so its forward migration must remain present and must never discard valid
 * client data. Migration 009 repairs databases that already ran the original
 * destructive version from a development `:latest` image.
 */
export async function up(_db: Kysely<unknown>): Promise<void> {}

export async function down(_db: Kysely<unknown>): Promise<void> {}
