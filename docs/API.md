# Client Integration Guide

How to build a client against the FUTO Notes sync server. This is the integrator's reference — for *why* the server is shaped this way (threat model, scaling), see [DESIGN.md](../DESIGN.md).

The server is a **generic E2EE sync backend**: it stores opaque encrypted blobs plus version metadata and never sees plaintext. Your client owns all encryption and all conflict resolution; the server only coordinates versions and tells you when something changed.

## Contents

- [Conventions](#conventions) — base URL, JSON, errors, auth, value encoding
- [Capability discovery](#capability-discovery)
- [Auth](#auth)
- [Collections](#collections)
- [Vault key material](#vault-key-material)
- [The sync model](#the-sync-model) — versions, cursors, conflicts
- [Objects](#objects)
- [Blobs](#blobs)
- [Real-time updates (SSE)](#real-time-updates-sse)
- [End-to-end recipe](#end-to-end-recipe)

---

## Conventions

**Base URL.** All endpoints below are relative to your server's origin. Examples use `$BASE` (e.g. `http://localhost:3005` in dev).

**Requests/responses are JSON** unless noted. Blob upload/download and the single-round-trip object endpoints use raw `application/octet-stream` bodies.

**Errors** always have the shape `{ "error": string }` with an appropriate status code:

| Status | Meaning |
|--------|---------|
| `400` | Invalid JSON or failed validation |
| `401` | Missing or invalid session |
| `404` | Not found, or not owned by you (we return 404 rather than 403 to avoid leaking existence) |
| `409` | Version conflict — you tried to write a stale version (see [The sync model](#the-sync-model)) |

**Auth.** Every `/api/*` route except the login endpoints requires a session. Present it either way:

- **Cookie** — login sets an `httpOnly` `session` cookie. Browsers send it automatically.
- **Bearer token** — login also returns the raw token in the body; send `Authorization: Bearer <token>`. Use this for non-browser clients.

Sessions last 7 days and renew automatically when more than half the lifetime has elapsed (the cookie is re-sent on a renewing request).

**Value encoding — read this.** Postgres `bigint` columns are serialized as **strings** in responses. So `version`, `change_seq`, `size_bytes`, and `current_version` come back as strings (`"3"`), but you **send** numeric fields (`version`, `size_bytes`) as JSON **numbers** (`3`). The one exception: the `collectionVersion` field in mutation responses is a number, not a string. Parse defensively.

---

## Capability discovery

Call this once when a user adds a server, to learn how to drive login. One client build works against any deployment.

```
GET $BASE/
```
```json
{ "name": "futo-notes", "version": "0.4.1", "auth_mode": "password", "signup": "closed", "billing": false }
```

- `auth_mode` is `"dev"` or `"password"` (`"oidc"` is reserved for later). It tells you which login endpoint to use.
- `signup` is `"closed"` in v1.

Health check (no auth):
```
GET $BASE/health        → 200 { "status": "ok", "db": "connected" }
                        → 503 { "status": "degraded", "db": "unreachable" }
```

---

## Auth

### Password mode (`auth_mode: "password"`)

Single-user self-hosted deployments. There is one user; the admin password is configured server-side.

```
POST $BASE/api/auth/password/login
Content-Type: application/json

{ "password": "..." }
```
```json
{ "user": { "id": "...", "email": "local@futo-notes.local", "name": "FUTO Notes" }, "token": "<raw-session-token>" }
```
- `400` if `password` is missing, `401` if it's wrong.

### Dev mode (`auth_mode: "dev"`)

Passwordless login for development and tests only. Upserts a user by email.

```
POST $BASE/api/auth/dev/login
Content-Type: application/json

{ "email": "alice@example.com", "name": "Alice" }   // name optional; defaults to the email's local part
```
Same response shape as password login. `400` if `email` is missing.

### Session endpoints (both modes)

```
GET  $BASE/api/auth        → 200 { "user": { "id", "email", "name" } }   // who am I
POST $BASE/api/auth/logout → 204                                          // destroys the session, clears the cookie
```

---

## Collections

A **collection** is a container owned by one user. Every object lives in exactly one collection, and the collection carries the global sync cursor (`current_version`) plus the client's encrypted vault key material.

```
POST   $BASE/api/collections          → 201 { "collection": { id, user_id, current_version, created_at } }   // no body
GET    $BASE/api/collections          → 200 { "collections": [ ... ] }   // oldest first
GET    $BASE/api/collections/:id      → 200 { "collection": { ... } }  | 404
DELETE $BASE/api/collections/:id      → 204 | 404                       // objects cascade; blobs are GC'd later
```

A new collection starts at `current_version: "0"`.

---

## Vault key material

The server stores — but cannot read — the material your client needs to unlock a vault on a new device: a KDF salt, the KDF parameters, and the vault key encrypted under a password-derived key. All three are opaque to the server.

```
GET $BASE/api/collections/:id/key
    → 200 { "key": { key_salt, key_kdf, encrypted_vault_key, key_updated_at } }
    → 200 { "key": null }     // collection exists but no key set yet
    → 404                     // collection not found / not yours
```
```
PUT $BASE/api/collections/:id/key
Content-Type: application/json

{
  "key_salt": "<non-empty string>",
  "key_kdf": { "...": "..." },          // any JSON object — your KDF parameters
  "encrypted_vault_key": "<non-empty string>"
}
    → 200 { "key": { ... } }
    → 400 invalid body | 404 not found
```

> Heads-up: rotating the key with `PUT .../key` does **not** currently emit a real-time `change` event, so peers learn about a rotation on their next poll, not instantly. Fine for single-vault use; note it if you depend on instant key propagation.

---

## The sync model

Three numbers, two of them per-collection cursors and one per-object:

- **`collection.current_version`** — a counter bumped by **1** on every object mutation in the collection. This is your **pull cursor**.
- **`object.change_seq`** — the value of `current_version` at the moment that object was last mutated. Pull = "give me every object whose `change_seq` is greater than my cursor."
- **`object.version`** — a per-object conflict counter. A new object is `version: "1"`. Each update must set `version` to exactly `previous + 1`.

**Pull (catch up):** ask for everything since your cursor.
```
GET $BASE/api/collections/:cid/objects?sinceVersion=<cursor>
    → 200 { "objects": [ ... ] }    // change_seq > cursor, ordered ascending
```
Apply the rows in order, then advance your cursor to the highest `change_seq` you received (equivalently, the `currentVersion` from the SSE event that prompted the pull). Start from `sinceVersion=0` on first sync. `deleted: true` rows are tombstones — apply them as deletions.

**Push (write):** send `version = lastSeenVersion + 1`.
- Accepted → `200`/`201` with the new row and a `collectionVersion` (the new cursor value).
- Stale → `409` with the server's current state so you can resolve:
  ```json
  { "error": "version conflict", "currentVersion": 4, "currentBlobKey": "user-id/blob-uuid" }
  ```

**Conflict resolution is entirely yours.** The server only rejects stale writes and hands back the current version + blob key. The expected client strategy (see DESIGN.md): fetch the server's blob, three-way merge against your common ancestor, and if that fails, save a conflict copy. The server sees only opaque blobs, so it could not merge even if it wanted to.

---

## Objects

An object is a metadata row pointing at a blob. Shape:
```json
{
  "id": "...", "collection_id": "...",
  "version": "3", "change_seq": "42", "deleted": false,
  "blob_key": "user-id/blob-uuid", "size_bytes": "1024",
  "created_at": "...", "updated_at": "..."
}
```

### List / pull
```
GET $BASE/api/collections/:cid/objects?sinceVersion=N   → { "objects": [ ... ] }
GET $BASE/api/collections/:cid/objects/:oid             → { "object": { ... } } | 404
```

### Create
```
POST $BASE/api/collections/:cid/objects
{ "blob_key": "<your-user-id>/<uuid>", "size_bytes": 1024 }
    → 201 { "object": { ... }, "collectionVersion": 1 }
    → 400 invalid blob_key/size_bytes | 404 collection not found
```
`blob_key` must be a blob you own — i.e. `"<your-user-id>/<blob-uuid>"`, exactly the key returned by `POST /api/blobs`.

### Update (version-guarded)
```
PUT $BASE/api/collections/:cid/objects/:oid
{ "version": 4, "blob_key": "<your-user-id>/<uuid>", "size_bytes": 2048 }
    → 200 { "object": { ... }, "collectionVersion": 7 }
    → 409 { "error": "version conflict", "currentVersion", "currentBlobKey" }
    → 404 object not found | 400 invalid body
```
`version` must equal the object's current version + 1.

### Delete (soft, optional race guard)
```
DELETE $BASE/api/collections/:cid/objects/:oid[?version=N]
    → 200 { "object": { id, version, change_seq, deleted: true }, "collectionVersion": 8 }
    → 409 { "error": "version conflict", ... }   // only if ?version is supplied and stale
    → 404 not found
```
Soft delete sets `deleted: true` and bumps the version so peers see the tombstone. Supplying `?version=N` makes the delete lose to a newer concurrent edit (edit-vs-delete race; edit wins). Omit it to delete unconditionally.

### Single-round-trip variants

These halve the round trips on high-latency networks by combining the blob upload and the object write. The body is the raw ciphertext (`application/octet-stream`); the server mints the blob key for you. Response shapes and conflict semantics are identical to the two-call versions above.

```
POST $BASE/api/collections/:cid/blob-objects              → 201 { "object", "collectionVersion" }   // create
PUT  $BASE/api/collections/:cid/blob-objects/:oid?version=N → 200 { "object", "collectionVersion" }   // update; ?version required
    → 400 empty body | 404 collection/object | 409 conflict
```

---

## Blobs

Opaque encrypted bytes. Keys have the form `<user-id>/<blob-uuid>` and are always scoped to you.

```
POST   $BASE/api/blobs                    → 201 { "key": "<user-id>/<uuid>" }   // body: raw bytes
GET    $BASE/api/blobs/:userId/:blobId    → 200 <raw bytes>  (application/octet-stream) | 404
DELETE $BASE/api/blobs/:userId/:blobId    → 204                                  // idempotent
```
- Upload an empty body → `400`. A key you don't own → `404` on GET/DELETE (no existence leak).
- The blob lifecycle is independent of objects: uploading a blob doesn't create an object, and replacing an object's blob leaves the old blob to be reclaimed later by server-side GC (clients rely on old blobs being available for a window to fetch the common ancestor during three-way merge).

---

## Real-time updates (SSE)

`GET /api/sync/events` holds open a [Server-Sent Events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events) stream that nudges you to pull. It is a **doorbell, not a pipe**: events carry no object content — just enough routing info to tell you *which collection* changed so you can pull through the normal endpoint. This keeps the E2EE invariant intact.

```
GET $BASE/api/sync/events     (Accept: text/event-stream, authenticated)
```

Events:

| Event | Data | Meaning |
|-------|------|---------|
| `ready` | *(empty)* | Sent once per connection. **Treat as "pull now from your cursor."** |
| `change` | `{ "collectionId": "...", "currentVersion": N }` | A mutation happened. Pull that collection from your cursor. |
| `ping` | *(empty)* | Heartbeat every 25s. Ignore — it just keeps the connection warm. |

### The contract (important)

The doorbell is **lossy across disconnects**. Events fired while your stream is down — a network blip, or the server's internal listener reconnecting — are **not replayed**. There is no event log and no `Last-Event-ID` recovery. So:

1. **Pull on every (re)connect.** Whenever you receive `ready`, pull from your current cursor. Don't wait for a `change`.
2. **Keep a low-frequency safety poll** (e.g. every 30–60s) as a backstop, so a missed doorbell self-corrects within one interval.
3. The stream may close on you when the server's database listener drops — that's intentional, so you fall back to reconnect-and-pull instead of sitting on a silently-dead stream. Reconnect (browsers' `EventSource` does this automatically) and you'll get a fresh `ready`.

If you follow (1) and (2), you cannot fall permanently behind regardless of what the stream does.

### Browser caveat

Native `EventSource` cannot set an `Authorization` header — it can only send the `session` cookie. The server's CORS is currently configured for `*` origins **without** credentials, so a cross-origin browser `EventSource` won't send the cookie and will fail to authenticate. Same-origin browser apps and any non-browser client (which can set `Authorization: Bearer`, or use a streaming `fetch`) are unaffected. If you need cross-origin browser SSE, that needs a credentialed-CORS change server-side first.

---

## End-to-end recipe

A minimal sync client, start to finish:

**First run**
1. `GET /` → read `auth_mode`.
2. Log in via the matching endpoint → store the token (and/or rely on the cookie).
3. `GET /api/collections`. If empty, `POST /api/collections` to create one. Remember its `id`.
4. `PUT /api/collections/:id/key` once, with your salt + KDF params + password-wrapped vault key. On other devices, `GET .../key` to unlock.
5. Full pull: `GET /api/collections/:id/objects?sinceVersion=0`. For each row, download its blob (`GET /api/blobs/:userId/:blobId`), decrypt, apply. Set `cursor` to the highest `change_seq`.

**Saving a change**
1. Encrypt locally. Upload the blob (`POST /api/blobs`) or use `POST/PUT .../blob-objects` to do it in one call.
2. Create (`POST .../objects`) or update (`PUT .../objects/:oid` with `version = lastSeen + 1`).
3. On `200/201`: advance `cursor` to `collectionVersion`. On `409`: fetch `currentBlobKey`, merge, retry with `currentVersion + 1`.

**Staying in sync**
1. Open `GET /api/sync/events`. On `ready` and on every `change`, pull `GET .../objects?sinceVersion=<cursor>` and apply.
2. Run a safety poll on the same pull every 30–60s.
3. On disconnect, reconnect; the fresh `ready` triggers a catch-up pull.
