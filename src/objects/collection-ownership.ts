import { db } from '../db/connection.ts'

interface CollectionBelongsToUserParams {
  userId: string
  collectionId: string
}

export async function collectionBelongsToUser({
  userId,
  collectionId,
}: CollectionBelongsToUserParams): Promise<boolean> {
  const collection = await db
    .selectFrom('collections')
    .where('id', '=', collectionId)
    .where('user_id', '=', userId)
    .select('id')
    .executeTakeFirst()
  return collection !== undefined
}
