# FUTO Notes Server — Design Document

A generic E2EE sync server. Paid service, licensed under [FUTO Source First License](https://sourcefirst.com/). Designed for future migration to [Yucca](https://github.com/immich-app/yucca) for auth and storage.

The first client is a notes app, but the server knows nothing about notes — see §Objects and collections.

## Stack

| Layer | Choice | Notes |
|-------|--------|-------|
| Runtime | Bun | Also the package manager, bundler driver, and test runner |
| Language | TypeScript | |
| HTTP framework | Hono | Served via `Bun.serve` |
| Package manager | Bun | |
| Database | PostgreSQL | Metadata only — kept intentionally small (see §Database footprint) |
| Query builder | Kysely | Matches Yucca, type-safe, no ORM magic |
| Blob storage | Cloudflare R2 (prod) | Stores encrypted blobs. Local dev uses an S3-compatible service (TBD — candidates: LocalStack, SeaweedFS, Garage) |
| Auth | Password (v1) or OIDC (deferred) | Opaque session tokens in Postgres. Mode is per-deployment (`AUTH_MODE` env var) |
| Real-time | Server-Sent Events | One-way notifications are sufficient — all pushes already go over HTTP. Falls back to polling on disconnect |
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

FUTO Notes v1 syncs **single-user data** across that user's devices. Every collection has exactly one owner. There is no server-mediated sharing, ACLs, or cross-user access in v1.

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

Two modes, chosen per-deployment via `AUTH_MODE`. The server renders no auth UI in either mode — clients read the capability endpoint at `/` and drive the login flow themselves.

### `AUTH_MODE=password` (v1 self-hosted)

Single-user mode. Configure exactly one credential:

- `FUTO_NOTES_PASSWORD` stores the password directly for the simplest self-hosted setup. Protect the mode-0600 `.env` like any other credential.
- `FUTO_NOTES_PASSWORD_HASH` stores a scrypt hash instead (produced by `bun dist/index.js hash <pw>`) when hash-at-rest is preferred.

One singleton user row (`sub='local'`) is lazy-upserted on first successful login. There is no per-user password table or reset endpoint. To change the password, update the configured credential and restart the server.

Flow:
1. Client POSTs `{ password }` to `/api/auth/password/login`
2. Server verifies against the configured plaintext credential with a timing-safe fixed-size digest comparison, or scrypt-verifies against `FUTO_NOTES_PASSWORD_HASH`
3. On success: upsert singleton user, open session, return `{ user, token }`

### `AUTH_MODE=oidc` (deferred)

Not implemented in v1. Will live in `src/hosted/` as middleware; `AUTH_MODE` gains `'oidc'` as a third value when it lands. Flow will be OIDC Authorization Code with PKCE (S256); required claims `sub`, `name`, `email`. The capability endpoint's `signup` field flips to `'open'` when this is enabled.

Yucca now ships a concrete reference for this flow (see §Yucca migration path below): `packages/yucca-api` implements OIDC Authorization Code + PKCE with `/auth/oidc/login` and `/auth/oidc/callback`, opaque session tokens, and a 7-day httpOnly cookie — the same shape planned here. It also adds an SSE-based **device flow** (`/auth/oidc/device`) for headless clients, which is worth borrowing if FUTO Notes needs CLI/headless login.

### Session model (both modes)

Server generates an opaque session token (32 random bytes, hex-encoded), stores its **SHA-256 hash** in the `sessions` table, and returns the raw token as an `httpOnly` cookie. Subsequent requests hash the cookie and look up the row. A `sessions` table leak therefore does not yield usable bearer credentials.

Session expiry: 7 days from issuance, with no sliding extension. An
expired/invalid supplied session returns the standard Bearer `invalid_token`
challenge plus the stable JSON code `invalid_session`, so clients can
reauthenticate with securely retained login material, receive a new token, and
retry without clearing their local vault cursor/object map.

### Capability discovery

`GET /` returns `{ name, version, auth_mode, signup, billing, mutation_ids }`.
Clients fetch it once at server-add time to render the right login UI and
discover retry-safety support. `mutation_ids` is
`{ supported: true, required: false, retention_days: 30 }`.

### Yucca migration path
Yucca's auth is built — `packages/yucca-api` has a working OIDC Authorization Code + PKCE implementation (via the public `openid-client` library, generic issuer discovery) with opaque session tokens and a 7-day httpOnly cookie (a `mock-oidc-provider` package backs local dev). The session model is already identical to this design, so migration stays a config change: point `OIDC_ISSUER` at the shared IdP when OIDC lands here. No session-layer code changes needed.

**The shared IdP is decided: Zitadel Cloud.** It is the FUTO org identity provider for both customer auth and internal FUTO auth — one "Log in with FUTO" account across every service (Notes, Immich, futopay). The cross-service identity key is the OIDC `sub` claim, which maps directly to this design's `users.sub UNIQUE` column. See [docs/MANAGED-LAUNCH.md](./docs/MANAGED-LAUNCH.md) for the integration plan, open questions (confirm public vs pairwise subjects), and the unresolved E2EE-vault-key-under-SSO decision.

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
- `FsBlobStore` — local filesystem. **The only implementation that exists today.** Used in tests and currently wired into both the OSS and hosted entrypoints (`src/app.ts`, `src/server.ts`).
- `S3BlobStore` — **not yet implemented.** Planned: talks to any S3-compatible endpoint (R2 in prod, local dev service in development). Needed for the horizontal-scaling story; until it exists, a managed launch runs on a host-volume `FsBlobStore`. See [docs/MANAGED-LAUNCH.md](./docs/MANAGED-LAUNCH.md).

### Yucca migration path
Yucca uses a Ceph cluster in Hetzner, exposed via the S3 API. Once `S3BlobStore` exists (it does not yet — see above), migration is a config change: point it at Yucca's endpoint, no further implementation needed.

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

Key material is **not** on the 409-conflict path that object mutations use. A first
claim of an unset key converges: the loser of the race is handed the authoritative
material with `200` and adopts it, because one vault has exactly one key and the
client has nothing to merge. Only a *rotation* — a write naming the exact revision
it read — can conflict, and only when that revision is stale. The winner's key is
never overwritten either way.

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

True zero-knowledge — hiding even collection/object IDs and activity shape — would require encrypting sync-coordination identifiers, which breaks the server's ability to coordinate sync efficiently. FUTO Notes accepts the same tradeoff Yucca, Immich, and other hosted E2EE services accept: the server learns *that* you have data and *when* you touched it, but never *what* it is.

### Multi-tenant isolation

Postgres is a single shared instance. All users' rows live in the same tables, separated by `user_id` / `collection_id` foreign keys. Every query serving an authenticated request must be scoped by the authenticated user's ID. A missing scope is a cross-tenant leak — testing and review should treat this as a top-priority invariant.

**Exception: scheduled background maintenance.** The invariant above exists to stop cross-tenant leaks from reaching a *request* and to keep shard routing trivial; it applies without exception to anything serving an authenticated request. Background maintenance jobs have no auth context and are deliberately global instead: the session reaper (`src/maintenance/sessionReaper.ts`) deletes expired sessions by `expires_at` across all users, and blob ledger/storage reconciliation and the mutation-result sweep (`src/collection-contents/index.ts`) reclaim rows by `state`/`state_changed_at`/`created_at` across all users. These sweeps never return data to any user and select purely on timestamp and lifecycle state, never on tenant. This is a real cost, not a free pass — see §Statelessness & scaling for what it means at Stage 3.

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

### Blob ledger and Mutation IDs

`blob_ledger` is the authoritative lifetime record for every known blob. A blob
is `staged`, `claimed`, `retained`, or `purgeable`; migration-only
`legacy_shared` rows quarantine historic shared references from deletion.
Claimed rows name exactly one object version. The storage bytes are deleted
before the ledger row, so an interrupted cleanup remains retryable.
The former `orphaned_blobs` table is retained only for safe downgrade and
migration auditing; runtime code neither reads nor writes it.

`mutation_results` records the original outcome of an optional client-generated
Mutation ID for 30 days. Its key is `(user_id, mutation_id)`, and its stored
intent includes mutation kind, collection, object, and requested version.
Collection and object IDs intentionally are not foreign keys: a retry must still
receive its original outcome after those rows have been deleted.

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
1. Client chooses a Mutation ID and encrypts object content
2. Client uploads a staged blob and sends `{ objectId, version, blobKey }`, or
   sends the ciphertext through a single-round-trip blob-object route
3. Server checks: is `version` exactly `server_object_version + 1`?
   - Yes → accept, update metadata
   - No → reject with `409 Conflict`, return current server version and blob key so client can resolve
4. On accepted create/update/delete, server increments `collections.current_version` and writes that value to `objects.change_seq`

The server serializes mutations collection-first. Version validation, claiming
the staged blob, changing the object, advancing the collection cursor, recording
the Mutation ID outcome, and publishing the transactional notification are one
database transaction. A conflict does not claim the staged blob or advance the
cursor. Retrying the same Mutation ID returns the recorded outcome and ignores
retried ciphertext; reusing it for a different intent is rejected.

Writing blob bytes is deliberately **outside** that transaction, including on
the single-round-trip routes: storing ciphertext while holding the collection's
row lock would serialize every other mutation in the collection behind that
I/O, and would hold a pooled connection across a network round trip once blobs
live in object storage. So a one-call mutation stages its blob first, then opens
the transaction to claim it. A mutation that is then declined — conflict, replay,
missing object — leaves its blob unclaimed, and the 24-hour staging window
reclaims it. Bounded, self-correcting waste is the accepted price of keeping
storage I/O off the lock.

### Pull (server → client)
1. Client sends its last seen collection cursor
2. Server sends deltas ordered by `change_seq`: all objects where `change_seq > client_cursor`
3. Ongoing: client holds an SSE stream open at `/api/sync/events`; the server emits a lightweight `change` event (`{collectionId, currentVersion}`) on every successful mutation, prompting the client to repeat step 2 with its current cursor. The event carries no object content — clients pull through the existing endpoint so the E2EE invariant is preserved.

The doorbell is **lossy across disconnects**: events fired while the stream is down (network blip, server-side listener reconnect) are not replayed — there is no event log and no `Last-Event-ID` recovery. Clients therefore recover by re-pulling, not by replay. The contract is: treat the `ready` event (sent on every connect) as a prompt to pull from the current cursor, and keep a low-frequency safety poll so a missed doorbell self-corrects within one poll interval.

### Why SSE, not WebSockets

The connection is a **doorbell, not a pipe**: the server has nothing useful to push besides "something changed" — all real data lives in opaque blobs that the client still has to pull. That eliminates WebSockets' main advantage (bidirectional framing) and trades it for SSE's wins: it runs through the same Hono middleware as every other HTTP route (auth, CORS, request logging), the browser/EventSource ecosystem auto-reconnects for free, and long-lived plain-HTTP streams play nicely with K8s ingress, load balancers, and graceful drain. The transport is hidden behind a `Notifier` interface, so a future move to WS is a swap, not a rewrite.

### Conflict resolution

Client-side. The server surfaces conflicts (version mismatch) but does not resolve them.

The client implements a two-tier strategy:
1. **Three-way merge** — client keeps a common ancestor version. On conflict, it fetches the server's version and performs a three-way merge (ancestor, local, remote). This handles most non-overlapping edits automatically.
2. **Conflict copy fallback** — if the three-way merge fails (overlapping edits), the client saves the remote version as a "conflict copy" object alongside the local version. The user resolves manually.

The server's only role is rejecting stale pushes with `409` and returning the current version. All merge logic lives in the client — and since the server sees only opaque blobs, it could not resolve them even if it wanted to.

### Blob lifetime

A separate upload is staged for a fixed 24 hours. An accepted create or update
atomically claims it for exactly one object version; direct deletion is allowed
only while staged. Any ledger state other than staged — claimed, retained,
purgeable, or legacy-shared — returns `409 blob is in use`.

A soft delete sets `deleted=true` and bumps the version, but its claimed blob
stays fetchable because the tombstone may be a merge ancestor. Updating an
object moves the prior claimed blob to retained for a fixed 365 days. Deleting
the collection makes all of its claimed and retained blobs immediately
purgeable; asynchronous maintenance removes storage bytes first and then their
ledger rows. Failed storage deletions retain their rows and retry later.

Storage reconciliation treats a valid user-owned blob file missing from the
ledger as freshly staged, giving it the full 24-hour claim window. Historic
duplicate references are marked `legacy_shared` and never automatically
deleted. These lifetimes are protocol policy, not self-host configuration.

## Statelessness & scaling

The server holds no in-process state. Any instance can serve any request. This is a hard requirement for running under Kubernetes with horizontal pod autoscaling.

- **Sessions, sync cursors, any transient coordination** — all in Postgres (sessions likely move to a KV store at Stage 2 below).
- **Real-time fan-out** — a naive in-memory subscriber map breaks horizontal scaling. Pub/sub is required: Postgres `LISTEN`/`NOTIFY` at small scale (current implementation), a dedicated broker (Redis, NATS) later. The `Notifier` abstraction in `src/sync/` is what gets swapped at that point — the SSE route is unaware of the fan-out mechanism.

### Natural partitioning

Data is naturally partitioned by user in v1: no query crosses users, no cross-user foreign keys exist. This makes sharding a routing problem rather than a data-model problem, and it's the single most important fact enabling the scaling path below.

When shared collections land (§Sync model scope → Future direction), the invariant weakens — queries gain a membership axis — but routing can still follow the collection's owner, preserving most of the benefit at the cost of occasional cross-shard reads for non-owners.

### Invariants to preserve

Cheap now, expensive to retrofit. Every one of these is a prerequisite for the sharding story, and all of them survive the shared-collections extension:

1. **Every query serving an authenticated request scoped by `user_id`** (by owner, post-sharing). No exceptions. The sole carve-out is scheduled background maintenance (session reaping, blob ledger/storage reconciliation, mutation-result expiry — see §Multi-tenant isolation), which has no auth context and is deliberately global. At Stage 3 (sharded), that carve-out has a real cost: these sweepers can no longer assume one database and must iterate per shard instead.
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

All endpoints under `/api`. This section is the design-level overview; for the full request/response shapes, auth flows, and the client sync contract, see the [client integration guide](./docs/API.md).

### Capability
```
GET  /                                            → { name, version, auth_mode, signup, billing, mutation_ids }
GET  /health                                      → { status, db }
```

### Auth
```
GET  /api/auth                                    → current user (authenticated)
POST /api/auth/logout                             → destroy session
POST /api/auth/password/login                     → password-mode login (mode=password)
GET  /api/auth/oidc/start                         → OIDC redirect to IdP (deferred)
GET  /api/auth/oidc/callback                      → OIDC callback (deferred)
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
DELETE /api/blobs/:key                            → delete a staged blob; 409 once claimed
```

### Sync
```
GET    /api/sync/events                           → SSE stream of change events for the authenticated user.
                                                    Events:
                                                      event: ready  (sent once on connect)
                                                      event: change data: {"collectionId":"...","currentVersion":N}
                                                      event: ping   (heartbeat every 25s)
```

## Deployment

**Production (aspirational):** Hetzner, Kubernetes. Containers designed for horizontal scaling (see §Statelessness & scaling). This matches Immich's direction, which makes shared operational knowledge cheaper.

**Production (actual, near-term launch):** FUTO deploys via **Manifest** (`gitlab.futo.org/devops/manifest`, driven by the `manifest-inventory` repo) — Docker containers on named bare-metal servers (e.g. `hv-lax2`) with **host volume mounts** for persistence and HAProxy load balancing, not Kubernetes and not object storage. Immich itself is deployed this way in FUTO infra. The near-term managed launch follows this model (host-volume `FsBlobStore`); K8s + object storage is the scaling path, not the launch path. See [docs/MANAGED-LAUNCH.md](./docs/MANAGED-LAUNCH.md).

Not targets:
- **Cloudflare Workers** — the real-time fan-out path needs a long-lived Postgres `LISTEN` connection per process, which doesn't fit Workers' request-scoped execution model. Pushing that state into Durable Objects works but reintroduces the stateful components we're trying to avoid.
- **DigitalOcean droplets.**

**Local development:** Docker Compose, with services for Postgres, an S3-compatible blob store (choice TBD), and the OIDC provider.

## Billing

Out of scope for this POC. Production billing will use [Polar](https://polar.sh/), handled as a separate service. Sync and storage do not integrate with billing directly in v1.

Yucca uses Polar as well, but this project's billing is not coupled to Yucca's.

## Yucca migration summary

| Layer | Migration effort |
|-------|-----------------|
| Auth (OIDC) | Config change — point `OIDC_ISSUER` at Yucca's IdP. Yucca's OIDC + opaque-session model is built and matches this design's session layer |
| Storage (S3) | Config change — point `S3BlobStore` at Yucca's Ceph endpoint. No new implementation |
| Sync protocol | None — Yucca has no sync, this stays |
| Encryption | None — client-side, server-agnostic |
| Database schema | Users/sessions structure already matches Yucca |
| Billing | Separate — this project uses Polar; Yucca's billing choice is not coupled |
