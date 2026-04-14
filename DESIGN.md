# Stonefruit Server — Design Document

A generic E2EE sync server. Paid service, licensed under [FUTO Source First License](https://sourcefirst.com/). Designed for future migration to [Yucca](https://github.com/immich-app/yucca) for auth and storage.

The first client is a notes app, but the server knows nothing about notes — see §Objects and collections.

## Stack

| Layer | Choice | Notes |
|-------|--------|-------|
| Runtime | Node.js | |
| Language | TypeScript | |
| HTTP framework | Hono | |
| Package manager | pnpm | |
| Database | PostgreSQL | Metadata only — kept intentionally small (see §Database footprint) |
| Query builder | Kysely | Matches Yucca, type-safe, no ORM magic |
| Blob storage | Cloudflare R2 (prod) | Stores encrypted blobs. Local dev uses an S3-compatible service (TBD — candidates: LocalStack, SeaweedFS, Garage) |
| Auth | OIDC + PKCE | Opaque session tokens in Postgres. Zitadel is the current reference provider |
| Real-time | WebSocket | Native WS, not Socket.IO |
| Deployment | Hetzner + Kubernetes | Docker Compose is local-dev only |

## Objects and collections

The server is a **generic E2EE sync backend**. It is not notes-specific. Yucca's author cautioned against one-sync-service-per-app sprawl, and this design takes that seriously: the server does not know what a "note" is.

- A **collection** is a container owned by a single user.
- An **object** is a versioned, opaque encrypted blob inside a collection.

Clients compose higher-level structures on top:
- A "note with attachments" = several objects in the same collection (one for the Markdown, one per attachment), plus a client-encrypted metadata object that describes the relationships.
- The server never sees this structure. It sees collections of versioned blobs.

This keeps the server reusable for any E2EE-sync client.

## Sync model scope

Stonefruit v1 syncs **single-user data** across that user's devices. Every collection has exactly one owner. There is no server-mediated sharing, ACLs, or cross-user access in v1.

Clients that want to share encrypted data between users handle it client-side — out-of-band key exchange, copy-on-share, or a higher-level protocol layered on top. In an E2EE setting this is the only sharing that actually preserves the E2EE property, since server-mediated sharing would require the server to know about relationships between users.

This scope is a deliberate product boundary, not a hack. It keeps the service a clean primitive — reusable across notes, passwords, files, photos, and anything else with single-user-across-devices semantics — and it makes the natural user-partitioning that enables horizontal scaling (see §Statelessness & scaling) a property of the product, not a workaround.

### Future direction: shared collections

Shared collections are an eventual goal, not v1. The expected shape when they land:

- Collections retain a single **owner**; storage and shard routing follow the owner.
- A `collection_members` table records grants (`collection_id`, `user_id`, role, wrapped-key metadata for re-wrapping the content key under each member's public key).
- Authorization scope widens from "you own it" to "you own it OR you're a member." Membership-aware queries route to the collection's owner shard.
- The threat model gains a new cross-user relationship (membership rows) — plaintext metadata about *who can access what*, which is currently absent.

v1 is designed so this is a forward-compatible extension, not a rewrite. The invariants in §Statelessness & scaling flag which assumptions will need to be revisited.

## Auth

OIDC Authorization Code flow with PKCE (S256). Zitadel is the current reference provider; Yucca's production IdP is TBD.

Flow:
1. Client redirects to the IdP login
2. IdP redirects back with authorization `code`
3. Server exchanges code for tokens, extracts `sub`, `name`, `email`
4. Server creates/updates user in Postgres, creates session
5. Server generates opaque session token (random 32 bytes, hex-encoded), stores its **SHA-256 hash** in the `sessions` table, and returns the raw token as an `httpOnly` cookie
6. Subsequent requests: server hashes the incoming cookie value and looks up the row by hash

Storing only the hash means a `sessions` table leak does not yield usable bearer credentials.

Required OIDC claims: `sub`, `name`, `email`.

Session expiry: 7 days (matches Yucca).

### Yucca migration path
Point `OIDC_ISSUER` at whatever provider Yucca uses. Session model is identical — no code changes needed.

## Storage architecture

```
Client → [encrypted blobs] → Hono server → [opaque blobs] → S3-compatible store (R2 / Ceph)
                                    ↕
                              PostgreSQL (metadata)
```

**Postgres stores:** users, sessions, collections, objects (metadata), sync state.

**Blob store holds:** encrypted blobs. Current key format: `{user_id}/{blob_id}` — bucket-agnostic, works with either layout below.

### BlobStore interface

The server talks to storage through an abstraction:

```ts
interface BlobStore {
  put(key: string, data: Uint8Array): Promise<void>
  get(key: string): Promise<Uint8Array | null>
  delete(key: string): Promise<void>
  list(prefix: string): Promise<string[]>
}
```

Implementations:
- `S3BlobStore` — talks to any S3-compatible endpoint (R2 in prod, local dev service in development)
- `FsBlobStore` — local filesystem (testing)

### Yucca migration path
Yucca uses a Ceph cluster in Hetzner, exposed via the S3 API. Migration is a config change — point the existing `S3BlobStore` at Yucca's endpoint. No new implementation needed.

### Bucket layout (open question)

Two candidates:
- **Bucket per user** — matches Immich's direction. Simpler per-user IAM and deletion; many buckets to operate.
- **Bucket per collection** — finer-grained isolation. Even more buckets; complicates per-user operations (e.g., account deletion).

The key format is bucket-agnostic, so the decision can be deferred until access patterns are clearer.

### Operational considerations (open)

- **Access patterns** — read-heavy during sync catch-up, write-heavy at capture time. Not yet characterized.
- **Caching** — blobs are user-scoped, so CDN caching isn't free. TBD.
- **Hot tier** — object storage is the default; frequently-read blobs may want an SSD-backed tier later.

## Encryption

Encryption is entirely a client-side concern. The server never sees plaintext.

The server's responsibilities:
- Store and serve opaque encrypted blobs
- Store encrypted vault key material (a random vault key encrypted by the client with a password-derived key) so users can unlock a vault from a new device after login
- TLS in transit

The client chooses its own encryption scheme. The server is agnostic.

Current client key model:
- Notes are encrypted with a random AES-256-GCM vault key.
- The vault key is wrapped client-side with a PBKDF2-SHA-256 password key.
- The server stores `key_salt`, `key_kdf`, and `encrypted_vault_key` on the collection.
- The server never sees the vault password, password-derived key, vault key, filenames, or note content.

## Threat model

Explicit about what the server is trusted with and what it is not, so future schema or feature changes don't accidentally smuggle sensitive plaintext into Postgres.

### What the server sees (and therefore what a DB leak exposes)

- **Account metadata:** `sub`, `name`, `email` from the IdP. Standard SaaS PII.
- **Existence and ownership:** which users exist, which collections they own, which objects are in each collection.
- **Activity shape:** object counts, blob sizes, version numbers, `created_at` / `updated_at` timestamps. An attacker with DB read access can infer "user X was active on Tuesday" and "user X has ~400 objects, total ~2 GB."
- **Blob routing:** `blob_key` values (opaque, but reveal user→blob ownership).

### What the server must never see

- Object titles, filenames, tags, folder paths, MIME types, thumbnails.
- Object content (the blob bytes).
- Client-side structure between objects (e.g., "these objects form a note," "this object is an attachment of that one"). That relationship lives inside a client-encrypted metadata object, which is itself just another opaque blob to the server.

**Rule of thumb for schema changes:** if a proposed column would be human-readable and tell an operator what an object *is*, it belongs in a client-encrypted blob, not in Postgres.

### Accepted tradeoffs

True zero-knowledge — hiding even collection/object IDs and activity shape — would require encrypting sync-coordination identifiers, which breaks the server's ability to coordinate sync efficiently. Stonefruit accepts the same tradeoff Yucca, Immich, and other hosted E2EE services accept: the server learns *that* you have data and *when* you touched it, but never *what* it is.

### Multi-tenant isolation

Postgres is a single shared instance. All users' rows live in the same tables, separated by `user_id` / `collection_id` foreign keys. Every query must be scoped by the authenticated user's ID. A missing scope is a cross-tenant leak — testing and review should treat this as a top-priority invariant.

## Data model

### Collections

```
collections
  id          uuid PK
  user_id     uuid FK → users
  current_version bigint -- collection-global sync cursor
  key_salt    text       -- client KDF salt for encrypted_vault_key
  key_kdf     jsonb      -- client KDF parameters
  encrypted_vault_key text -- password-wrapped vault key bytes
  key_updated_at timestamptz
  created_at  timestamptz
```

### Objects

An object is a metadata record in Postgres pointing at a blob in object storage.

```
objects
  id             uuid PK
  collection_id  uuid FK → collections
  user_id        uuid FK → users   -- denormalized for direct scoping; equal to collections.user_id
  version        bigint            -- per-object conflict counter
  change_seq     bigint            -- collection-global ordering cursor
  deleted        boolean
  blob_key       text              -- S3 key for the current version's blob
  size_bytes     bigint
  created_at     timestamptz
  updated_at     timestamptz
```

Earlier-version blobs can be retained or garbage-collected depending on policy — a configuration concern, not a structural one.

### Users and sessions

```
users
  id          uuid PK
  sub         text UNIQUE   -- OIDC subject identifier
  name        text
  email       text UNIQUE

sessions
  id                uuid PK
  user_id           uuid FK → users
  access_token_hash bytea UNIQUE  -- SHA-256 of the raw token; raw token only ever lives in the client's cookie
  expires_at        timestamptz
```

### Database footprint

Postgres is for sync coordination only — collections, objects (metadata), users, sessions. Everything else lives in the blob store or in client-encrypted metadata objects.

Keeping the DB small protects the scaling path: Immich is nervous about DB-at-scale (considering PlanetScale for their service-wide store), and this project should avoid painting itself into the same corner.

## Sync protocol

Versioned sync with per-object conflict checks and a collection-global pull cursor. No CRDTs — last-write-wins with conflict surfacing.

### Push (client → server)
1. Client encrypts object content, uploads blob to server
2. Client sends sync request: `{ objectId, version, blobKey }`
3. Server checks: is `version` exactly `server_object_version + 1`?
   - Yes → accept, update metadata
   - No → reject with `409 Conflict`, return current server version and blob key so client can resolve
4. On accepted create/update/delete, server increments `collections.current_version` and writes that value to `objects.change_seq`

### Pull (server → client)
1. Client sends its last seen collection cursor
2. Server sends deltas ordered by `change_seq`: all objects where `change_seq > client_cursor`
3. Ongoing: server pushes updates over WS as they arrive

### Conflict resolution

Client-side. The server surfaces conflicts (version mismatch) but does not resolve them.

The client implements a two-tier strategy:
1. **Three-way merge** — client keeps a common ancestor version. On conflict, it fetches the server's version and performs a three-way merge (ancestor, local, remote). This handles most non-overlapping edits automatically.
2. **Conflict copy fallback** — if the three-way merge fails (overlapping edits), the client saves the remote version as a "conflict copy" object alongside the local version. The user resolves manually.

The server's only role is rejecting stale pushes with `409` and returning the current version. All merge logic lives in the client — and since the server sees only opaque blobs, it could not resolve them even if it wanted to.

## Statelessness & scaling

The server holds no in-process state. Any instance can serve any request. This is a hard requirement for running under Kubernetes with horizontal pod autoscaling.

- **Sessions, sync cursors, any transient coordination** — all in Postgres (sessions likely move to a KV store at Stage 2 below).
- **WebSocket fan-out** — a naive in-memory subscriber map breaks horizontal scaling. Pub/sub is required: Postgres `LISTEN`/`NOTIFY` at small scale, a dedicated broker (Redis, NATS) later.

### Natural partitioning

Data is naturally partitioned by user in v1: no query crosses users, no cross-user foreign keys exist. This makes sharding a routing problem rather than a data-model problem, and it's the single most important fact enabling the scaling path below.

When shared collections land (§Sync model scope → Future direction), the invariant weakens — queries gain a membership axis — but routing can still follow the collection's owner, preserving most of the benefit at the cost of occasional cross-shard reads for non-owners.

### Invariants to preserve

Cheap now, expensive to retrofit. Every one of these is a prerequisite for the sharding story, and all of them survive the shared-collections extension:

1. **Every query scoped by `user_id`** (by owner, post-sharing). No exceptions.
2. **No cross-user foreign keys in v1.** When shared collections land, the lone exception will be `collection_members`, keyed by collection → owner.
3. **`user_id` present on every leaf row**, not just reachable via join. Makes shard routing trivial.
4. **Stable, opaque, shard-neutral IDs** (UUIDv7 or similar). No auto-increment that assumes one DB.

### Scaling progression

Each stage earned by measurement, not adopted speculatively.

| Stage | State | Move |
|---|---|---|
| 0 | POC (current) | Single Postgres, no tuning. Learn the workload. |
| 1 | Single node, tuned | PgBouncer, read replica for non-critical reads, native `objects` partitioning by `hash(user_id)`. |
| 2 | Sessions out | Move `sessions` to Redis/Valkey — highest-QPS table, replaceable on loss, automatic TTL. Removes the hottest read path from Postgres. |
| 3 | Sharded | N independent Postgres instances. `shard_id` claim in the session JWT routes at a stateless gateway. Fallback: Citus (Postgres-compatible) if managing N instances isn't wanted. |
| 4 | Within-shard | Normal Postgres work — replicas, partitioning, query tuning. |

**Deliberately not on this path:**
- **PlanetScale / Vitess** — MySQL commitment, diverges from Yucca's Postgres stack.
- **CockroachDB / Yugabyte** — distributed SQL. Solves problems we don't have (cross-shard transactions) at an operational cost we'd pay every day.

### Future sharding (mechanics)

Clients receive a signed JWT carrying a `shard_id` claim. The stateless gateway reads the claim and opens a connection to the right shard's pool. A small central map (shard registry) is the source of truth for rebalancing; JWTs are reissued on shard moves. Not implemented in v1, but v1 must not preclude it — which is what the invariants above are for.

## API surface

All endpoints under `/api`.

### Auth
```
GET  /api/auth                                    → current user
GET  /api/auth/oidc/login                         → redirect to IdP
GET  /api/auth/oidc/callback                      → complete OIDC flow
POST /api/auth/logout                             → destroy session
```

### Collections
```
POST   /api/collections                           → create collection
GET    /api/collections                           → list user's collections
GET    /api/collections/:id                       → get collection
GET    /api/collections/:id/key                   → get encrypted vault key material
PUT    /api/collections/:id/key                   → create/update encrypted vault key material
DELETE /api/collections/:id                       → delete collection
```

### Objects
```
GET    /api/collections/:id/objects               → list objects (metadata only)
POST   /api/collections/:id/objects               → create object
GET    /api/collections/:id/objects/:objectId     → get object metadata
PUT    /api/collections/:id/objects/:objectId     → update object (push new version)
DELETE /api/collections/:id/objects/:objectId     → soft delete
```

### Blobs
```
POST   /api/blobs                                 → upload encrypted blob, returns blob key
GET    /api/blobs/:key                            → download encrypted blob
DELETE /api/blobs/:key                            → delete blob
```

### Sync
```
WS     /api/sync                                  → WebSocket for real-time sync
```

## Deployment

**Production:** Hetzner, Kubernetes. Containers designed for horizontal scaling (see §Statelessness & scaling). This matches Immich's direction, which makes shared operational knowledge cheaper.

Not targets:
- **Cloudflare Workers** — native WebSockets there require Durable Objects, which reintroduce stateful components under a different name. Not worth the complexity.
- **DigitalOcean droplets.**

**Local development:** Docker Compose, with services for Postgres, an S3-compatible blob store (choice TBD), and the OIDC provider.

## Billing

Out of scope for this POC. Production billing will use [Polar](https://polar.sh/), handled as a separate service. Sync and storage do not integrate with billing directly in v1.

Yucca uses Polar as well, but this project's billing is not coupled to Yucca's.

## Yucca migration summary

| Layer | Migration effort |
|-------|-----------------|
| Auth (OIDC) | Config change — point `OIDC_ISSUER` at Yucca's IdP |
| Storage (S3) | Config change — point `S3BlobStore` at Yucca's Ceph endpoint. No new implementation |
| Sync protocol | None — Yucca has no sync, this stays |
| Encryption | None — client-side, server-agnostic |
| Database schema | Users/sessions structure already matches Yucca |
| Billing | Separate — this project uses Polar; Yucca's billing choice is not coupled |
