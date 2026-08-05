import type { Kysely, Selectable } from 'kysely'
import type { Database, MutationResultsTable } from '../db/types.ts'

interface FindCreateMutationResultParams {
  db: Kysely<Database>
  userId: string
  collectionId: string
  mutationId: string
}

export async function findCreateMutationResult({
  db,
  userId,
  collectionId,
  mutationId,
}: FindCreateMutationResultParams): Promise<Pick<
  Selectable<MutationResultsTable>,
  'result' | 'created_at'
> | null> {
  const match = await db
    .selectFrom('mutation_results')
    .where('user_id', '=', userId)
    .where('collection_id', '=', collectionId)
    .where('mutation_id', '=', mutationId)
    .where('kind', '=', 'create')
    .select(['result', 'created_at'])
    .executeTakeFirst()
  return match ?? null
}
