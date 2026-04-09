# Stonefruit Server — Design Document

E2EE sync server for a notes app. Paid service, licensed under [FUTO Source First License](https://sourcefirst.com/). Designed for future migration to [Yucca](https://github.com/immich-app/yucca) for auth and storage.

## Stack

| Layer | Choice | Notes |
|-------|--------|-------|
| Runtime | Node.js | |
| Language | TypeScript | |
| HTTP framework | Hono | |
| Package manager | pnpm | |
| Database | PostgreSQL | Metadata only — no note content |
| Query builder | Kysely | Matches Yucca, type-safe, no ORM magic |
| Blob storage | MinIO (S3-compatible) | Stores encrypted blobs. Runs as separate Docker service |
| Auth | OIDC + PKCE via Authentik | Opaque session tokens in Postgres |
| Real-time | WebSocket | Native WS, not Socket.IO |
| Deployment | Docker Compose | |

## What a "note" is

A note is a Markdown document that can reference attachments (images, PDFs, potentially other file types in the future). From the server's perspective, everything is an opaque encrypted blob — it cannot distinguish Markdown from a PNG.

A note maps to multiple blobs:
- One blob for the Markdown content
- One blob per attachment

The client encrypts all blobs before upload. The server stores them and manages sync metadata.

## Auth

OIDC Authorization Code flow with PKCE (S256), backed by Authentik.

Flow:
1. Client redirects to Authentik login
2. Authentik redirects back with authorization `code`
3. Server exchanges code for tokens, extracts `sub`, `name`, `email`
4. Server creates/updates user in Postgres, creates session
5. Server returns opaque session token (random 32 bytes, hex-encoded) as `httpOnly` cookie
6. Subsequent requests authenticate via cookie → session table lookup

Required OIDC claims: `sub`, `name`, `email`.

Session expiry: 7 days (matches Yucca).

### Yucca migration path
Point `OIDC_ISSUER` at whatever provider Yucca uses. Session model is identical — no code changes needed.

## Storage architecture

```
Client → [encrypted blobs] → Hono server → [opaque blobs] → MinIO (S3)
                                    ↕
                              PostgreSQL (metadata)
```

**Postgres stores:** users, sessions, vaults, notes (metadata), sync state.

**MinIO stores:** encrypted blobs (note content, attachments). Addressed by key, organized as `{user_id}/{blob_id}`.

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
- `S3BlobStore` — talks to MinIO (production)
- `FsBlobStore` — local filesystem (development/testing)

### Yucca migration path
Implement `YuccaBlobStore` that speaks Yucca's restic REST backend protocol. Everything above the interface stays the same.

## Encryption

Encryption is entirely a client-side concern. The server never sees plaintext.

The server's responsibilities:
- Store and serve opaque encrypted blobs
- Optionally store an encrypted master key blob (encrypted by the client with a password-derived key) so users can recover their vault from a new device
- TLS in transit

The client chooses its own encryption scheme. The server is agnostic.

## Data model

### Vaults

A vault is a collection of notes owned by a single user.

```
vaults
  id          uuid PK
  user_id     uuid FK → users
  created_at  timestamptz
```

### Notes

A note is a metadata record in Postgres. Its content lives in MinIO as encrypted blobs.

```
notes
  id          uuid PK
  vault_id    uuid FK → vaults
  version     bigint
  deleted     boolean
  created_at  timestamptz
  updated_at  timestamptz
```

### Blobs

All encrypted data — note content, attachments, anything — is stored as blobs in MinIO. The client tracks which blobs belong to which note in its own encrypted metadata. The server doesn't distinguish content types.

```
blobs
  id          uuid PK
  note_id     uuid FK → notes
  blob_key    text          -- S3 key ({user_id}/{blob_id})
  size_bytes  bigint
  created_at  timestamptz
```

### Users and sessions

```
users
  id          uuid PK
  sub         text UNIQUE   -- OIDC subject identifier
  name        text
  email       text UNIQUE

sessions
  id          uuid PK
  user_id     uuid FK → users
  access_token text UNIQUE  -- random 32 bytes, hex
  expires_at  timestamptz
```

## Sync protocol

Version-vector sync with per-note granularity. No CRDTs — last-write-wins with conflict surfacing.

### Push (client → server)
1. Client encrypts note content, uploads blob to server
2. Client sends sync request: `{ noteId, version, blobKey }`
3. Server checks: is `version` exactly `server_version + 1`?
   - Yes → accept, update metadata
   - No → reject with `409 Conflict`, return current server version and blob key so client can resolve

### Pull (server → client)
1. Client connects via WebSocket, sends last-known version per note
2. Server sends deltas: all notes where `server_version > client_version`
3. Ongoing: server pushes updates over WS as they arrive

### Conflict resolution

Client-side. The server surfaces conflicts (version mismatch) but does not resolve them.

The client implements a two-tier strategy:
1. **Three-way merge** — client keeps a common ancestor version. On conflict, it fetches the server's version and performs a three-way merge (ancestor, local, remote). This handles most non-overlapping edits automatically.
2. **Conflict copy fallback** — if the three-way merge fails (overlapping edits), the client saves the remote version as a "conflict copy" note alongside the local version. The user resolves manually.

The server's only role is rejecting stale pushes with `409` and returning the current version. All merge logic lives in the client.

## API surface

All endpoints under `/api`.

### Auth
```
GET  /api/auth                          → current user
GET  /api/auth/oidc/login               → redirect to Authentik
GET  /api/auth/oidc/callback            → complete OIDC flow
POST /api/auth/logout                   → destroy session
```

### Vaults
```
POST /api/vaults                        → create vault
GET  /api/vaults                        → list user's vaults
GET  /api/vaults/:id                    → get vault
DELETE /api/vaults/:id                  → delete vault
```

### Notes
```
GET  /api/vaults/:id/notes              → list notes (metadata only)
POST /api/vaults/:id/notes              → create note
GET  /api/vaults/:id/notes/:noteId      → get note metadata
PUT  /api/vaults/:id/notes/:noteId      → update note (push new version)
DELETE /api/vaults/:id/notes/:noteId    → soft delete
```

### Blobs
```
POST /api/blobs                         → upload encrypted blob, returns blob key
GET  /api/blobs/:key                    → download encrypted blob
DELETE /api/blobs/:key                  → delete blob
```

### Sync
```
WS  /api/sync                          → WebSocket for real-time sync
```

## Docker Compose topology

```
services:
  server:       # Hono app (this project)
  postgres:     # Metadata
  minio:        # Encrypted blob storage
  authentik:    # OIDC provider
```

## Yucca migration summary

| Layer | Migration effort |
|-------|-----------------|
| Auth (OIDC) | Config change — point to Yucca's IdP |
| Storage (S3) | Implement `YuccaBlobStore` behind existing interface |
| Sync protocol | None — Yucca has no sync, this stays |
| Encryption | None — client-side, server-agnostic |
| Database schema | Users/sessions table structure already matches Yucca |
