import { Hono } from 'hono'
import { uuidv7 } from 'uuidv7'
import type { AuthContext } from '../auth/middleware.ts'
import type { BlobStore } from '../blob/interface.ts'

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export function createBlobRoutes(store: BlobStore): Hono<{ Variables: AuthContext }> {
  const app = new Hono<{ Variables: AuthContext }>()

  // Upload an opaque blob. Returns the storage key scoped to the authenticated user.
  app.post('/', async (c) => {
    const userId = c.var.user.id
    const body = await c.req.arrayBuffer()
    if (body.byteLength === 0) {
      return c.json({ error: 'empty body' }, 400)
    }

    const blobId = uuidv7()
    const key = `${userId}/${blobId}`
    await store.put(key, new Uint8Array(body))
    return c.json({ key }, 201)
  })

  // Download a blob. Only the owning user can access it.
  app.get('/:userId/:blobId', async (c) => {
    const userId = c.var.user.id
    const paramUserId = c.req.param('userId')
    const paramBlobId = c.req.param('blobId')

    // Ownership check — return 404 (not 403) to avoid existence leaks.
    if (paramUserId !== userId || !UUID_RE.test(paramBlobId)) {
      return c.json({ error: 'not found' }, 404)
    }

    const key = `${paramUserId}/${paramBlobId}`
    const data = await store.get(key)
    if (!data) return c.json({ error: 'not found' }, 404)

    return new Response(data, {
      status: 200,
      headers: { 'Content-Type': 'application/octet-stream' },
    })
  })

  // Delete a blob. Only the owning user can delete it.
  app.delete('/:userId/:blobId', async (c) => {
    const userId = c.var.user.id
    const paramUserId = c.req.param('userId')
    const paramBlobId = c.req.param('blobId')

    if (paramUserId !== userId || !UUID_RE.test(paramBlobId)) {
      return c.json({ error: 'not found' }, 404)
    }

    const key = `${paramUserId}/${paramBlobId}`
    await store.delete(key)
    return c.body(null, 204)
  })

  return app
}
