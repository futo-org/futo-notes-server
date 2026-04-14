import { Hono } from 'hono'
import { uuidv7 } from 'uuidv7'
import type { AuthContext } from '../auth/middleware.ts'
import { db } from '../db/connection.ts'

export const collectionsRoutes = new Hono<{ Variables: AuthContext }>()

collectionsRoutes.post('/', async (c) => {
  const userId = c.var.user.id
  const row = await db
    .insertInto('collections')
    .values({ id: uuidv7(), user_id: userId })
    .returning(['id', 'user_id', 'created_at'])
    .executeTakeFirstOrThrow()
  return c.json({ collection: row }, 201)
})

collectionsRoutes.get('/', async (c) => {
  const userId = c.var.user.id
  const rows = await db
    .selectFrom('collections')
    .where('user_id', '=', userId)
    .select(['id', 'user_id', 'created_at'])
    .orderBy('created_at', 'asc')
    .execute()
  return c.json({ collections: rows })
})

collectionsRoutes.get('/:id', async (c) => {
  const userId = c.var.user.id
  const id = c.req.param('id')
  const row = await db
    .selectFrom('collections')
    .where('id', '=', id)
    .where('user_id', '=', userId)
    .select(['id', 'user_id', 'created_at'])
    .executeTakeFirst()
  if (!row) return c.json({ error: 'not found' }, 404)
  return c.json({ collection: row })
})

collectionsRoutes.delete('/:id', async (c) => {
  const userId = c.var.user.id
  const id = c.req.param('id')
  const deleted = await db
    .deleteFrom('collections')
    .where('id', '=', id)
    .where('user_id', '=', userId)
    .returning('id')
    .executeTakeFirst()
  if (!deleted) return c.json({ error: 'not found' }, 404)
  // Objects cascade via FK. Blob cleanup is deferred (see DESIGN.md).
  return c.body(null, 204)
})
