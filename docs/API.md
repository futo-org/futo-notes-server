# Client Integration Guide

How to build a client against the FUTO Notes sync server. This is the integrator's reference — for *why* the server is shaped this way (threat model, scaling), see [DESIGN.md](../DESIGN.md), which predates the Go rewrite but whose threat model and sync model still apply.

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

**Errors** always include `{ "error": string }` with an appropriate status code.
Some errors also carry a stable machine-readable `code`:

| Status | Meaning |
|--------|---------|
| `400` | Invalid JSON or failed validation |
| `401` | Missing or invalid session |
| `404` | Not found, or not owned by you (we return 404 rather than 403 to avoid leaking existence) |
| `409` | Version conflict, Mutation ID intent mismatch, direct deletion of an in-use blob, or a `blob_key` that isn't currently staged |
| `413` | Blob upload body exceeds the server's size limit (default 100 MiB) |

**Auth.** Every `/api/*` route except the login endpoints requires a session. Present it either way:

- **Cookie** — login sets an `httpOnly` `session` cookie (also `Secure` by default in password mode; set `COOKIE_SECURE=false` for plain-HTTP deployments). Browsers send it automatically.
- **Bearer token** — login also returns the raw token in the body; send `Authorization: Bearer <token>`. Use this for non-browser clients.

Sessions expire 7 days after issuance; authenticated activity does not extend that fixed lifetime. If a supplied cookie or bearer token has expired or is otherwise invalid, the server returns `401`, a `WWW-Authenticate: Bearer … error="invalid_token"` challenge, and `{ "error": "session expired or invalid", "code": "invalid_session" }`. A client that securely retained its login material should log in again, receive a new session token, retry the request once, and preserve its local sync cursor/object map; session expiry is not a vault reset.

**Value encoding — read this.** Postgres `bigint` columns come back as **strings** in responses: `version`, `change_seq`, `size_bytes`, `current_version`. Scalars the server computes come back as **numbers**: `collectionVersion`, `currentVersion`, `nextCursor`. Requests run the other way round — you *send* `version` and `size_bytes` as JSON numbers (`3`). Parse defensively, and see [the object shape](#objects) for the field-by-field breakdown and the string-vs-number comparison trap this sets up.

---

## Capability discovery

Call this once when a user adds a server, to learn how to drive login. One client build works against any deployment.

```
GET $BASE/
```
```json
{
  "name": "futo-notes",
  "version": "0.6.0",
  "auth_mode": "password",
  "signup": "closed",
  "billing": false,
  "mutation_ids": {
    "supported": true,
    "required": false,
    "retention_days": 30,
    "successful_create_outcomes": "durable"
  }
}
```

- `auth_mode` is `"dev"`, `"password"`, or `"oidc"`. It tells you which login endpoint to use.
- `signup` is `"closed"`, except `"open"` in `oidc` mode, since any FUTO account can log in.
- `billing` is `true` on the hosted service and `false` on a self-hosted server. When it is
  `true` alongside `auth_mode: "oidc"`, the server sells subscriptions: show the sign-up path,
  and expect a write to be refused with `402 subscription_required` until the account is
  entitled. Reads are never refused for billing reasons.
- `mutation_ids` advertises retry-safe object mutations. They are supported but
  optional for backward compatibility. Successful create outcomes remain
  available for the collection lifetime; other recorded outcomes are retained for
  30 days.

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

### OIDC mode (`auth_mode: "oidc"`)

"Log in with FUTO": Authorization Code + PKCE against the configured issuer. Browser-driven only.

```
GET $BASE/api/auth/oidc/start     → 302 to the identity provider's authorize endpoint
GET $BASE/api/auth/oidc/callback  → completes the flow
```
- `start` sets a short-lived `oidc_flow` cookie carrying the OAuth `state` and PKCE verifier, and redirects.
- `callback` requires that cookie and a matching `state` query parameter (`400` otherwise), exchanges the `code`, reads `sub`/`email`/`name` from the identity provider's userinfo endpoint, and upserts the user by `sub`. `502` if the provider omits `sub` or `email`.
- On success: if the server runs with `DEV_UI=true`, redirects to `/dev`; otherwise returns the same `{ user, token }` body as password/dev login.
- Rate limited like password login.
- `403 { "error": "sign-up is not open yet", "code": "signup_not_open" }` when `HOSTED_ALLOWED_EMAIL_DOMAINS` is set and the identity's email domain is not on it. Checked before any user row exists, compared case-insensitively. Unset the variable to admit everyone.

#### Login Hand-off (apps)

An app can't drive a browser redirect, so it hands the login off to one and redeems the result. It never sees identity-provider tokens.

```
POST $BASE/api/auth/handoff           → 200 { "ticket": "...", "url": "..." }   // no body, no credentials
GET  $BASE/api/auth/handoff/{ticket}  → 202 (empty)  while the person is still in the browser
                                      → 200 { "user": {...}, "token": "..." }   // exactly once
                                      → 404 afterwards, and for unknown or expired tickets
```
- `url` is the `oidc/start` route carrying the ticket, on the origin from `OIDC_REDIRECT_URI` — whose path has to end in `/api/auth/oidc/callback`, since the start route is its sibling.  Open it in a system browser, auth sheet, or webview — the server does not care.
- The browser lands on a small "go back to the app" page instead of the login JSON, and keeps its own session cookie. The token you redeem is a separate session, opened at redemption, so logging out in one place does not end the other.
- A ticket records which account authenticated, never a session token.
- Tickets stay claimable for 10 minutes; a fulfilled one stays redeemable for 5 minutes from the callback. A ticket whose identity is turned away by `HOSTED_ALLOWED_EMAIL_DOMAINS` is spent, so the poll answers `404` immediately rather than leaving you waiting.
- Poll on whatever interval suits you — this route is not rate limited. Minting is.

### Session endpoints (all modes)

```
GET  $BASE/api/auth        → 200 { "user": { "id", "email", "name" } }   // who am I
POST $BASE/api/auth/logout → 204                                          // destroys the session, clears the cookie
```

---

## Collections

A **collection** is a container owned by one user. Every object lives in exactly one collection, and the collection carries the global sync cursor (`current_version`) plus the client's encrypted vault key material.

```
POST   $BASE/api/collections          → 201 { "collection": { id, user_id, current_version, created_at } }   // no body; created
                                      → 200 { "collection": { ... } }    // you already have a vault; this is it
GET    $BASE/api/collections          → 200 { "collections": [ ... ] }   // oldest first
GET    $BASE/api/collections/:id      → 200 { "collection": { ... } }  | 404
DELETE $BASE/api/collections/:id      → 204 | 404                       // objects cascade; blobs are GC'd later
```

A new collection starts at `current_version: "0"`.

**One vault per account.** `POST` is idempotent: it creates your vault the first
time (`201`) and hands back that same vault on every later call (`200`). Two devices
setting up at once therefore converge instead of forking into vaults that never see
each other's notes — the same convergence the key endpoint gives you. Read the id from
the response rather than assuming a `201`. Accounts that already hold several
collections from before this cap keep all of them, and `GET /api/collections` still
lists them; `POST` returns the oldest, which is the one to sync against.

---

## Vault key material

The server stores — but cannot read — the material your client needs to unlock a vault on a new device. The vault key can be wrapped twice, and each wrapping is an **envelope** of a salt, KDF parameters, and the wrapped key:

- The **password envelope** (`key_salt`, `key_kdf`, `encrypted_vault_key`) wraps the vault key under the vault password. Always present.
- The **recovery envelope** (`recovery_key_salt`, `recovery_key_kdf`, `recovery_encrypted_vault_key`) wraps the same vault key under a generated recovery key instead. **Optional**, and the three fields are `null` together. Their absence means that vault has no recovery key: the vault password, or a device that is already unlocked, is the only way in.

> **On a PUT, "optional" does not mean "leave unchanged".** Omitting the three `recovery_*` fields **deletes** any recovery envelope already stored. See [A PUT replaces the whole of the key material](#vault-key-material) below before you write one.

Every field is opaque to the server, which never sees a vault password, a recovery key, either derived key, or the vault key itself.

```
GET $BASE/api/collections/:id/key
    → 200 { "key": { key_salt, key_kdf, encrypted_vault_key, key_updated_at,
                      recovery_key_salt, recovery_key_kdf, recovery_encrypted_vault_key } }
    → 200 { "key": null }     // collection exists but no key set yet
    → 404                     // collection not found / not yours
```

The three `recovery_*` fields are always in the response, `null` and all, so you can tell "no recovery envelope" from "old server".

```
PUT $BASE/api/collections/:id/key
Content-Type: application/json

{
  "key_salt": "<non-empty string>",
  "key_kdf": { "...": "..." },          // any JSON object — your KDF parameters
  "encrypted_vault_key": "<non-empty string>",
  "previous_key_updated_at": "<opaque revision from GET/PUT>", // re-wrap only

  // The recovery envelope: all three, or none at all.
  "recovery_key_salt": "<non-empty string>",
  "recovery_key_kdf": { "...": "..." },
  "recovery_encrypted_vault_key": "<non-empty string>"
}
    → 200 { "key": { ... } }
    → 409 { "error": "key conflict", "currentKey": { ... } }   // stale revision token only
    → 400 invalid body | 404 not found
```

The first client may claim an unset key without `previous_key_updated_at`. Retrying
the exact same material is idempotent and returns `200` with the original
`key_updated_at`. A **re-wrap** — replacing key material that already exists — must
include the current `key_updated_at` from a prior `GET` or `PUT`; the server treats it
as an opaque revision token and replaces the material only if it still matches. A stale
token returns `409` with the authoritative material in `currentKey`, allowing the
client to resolve the conflict safely. Do not parse or synthesize the token.

**Holding the current token, you may re-wrap as often as you like.** A second write is
not a conflict; only a stale token is. That is what lets an already-unlocked device
change the vault password or issue a new recovery key without racing a first claim. On
a `409`, re-read, re-wrap from the token that came back, and retry.

**A PUT replaces the whole of the key material, both envelopes together.** There is no
partial update and no way to address one envelope on its own, so **omitting the three
`recovery_*` fields deletes any recovery envelope already stored for this vault**, in
the same write that stores the password envelope. `null` and omitted mean the same
thing here; neither means "leave it alone".

Take that seriously if you are changing only one envelope. Changing the vault password
re-wraps the *same* vault key, so the recovery envelope you did not send would still
have opened the vault — dropping it destroys a working recovery door and the person
loses their way back in. **Send the other envelope back unchanged in the same request**,
taking it from the `GET` (or the previous `PUT`) you read the revision token from. Same
the other way round: issuing a new recovery key means sending the untouched password
envelope alongside it.

Deleting is the right outcome in exactly one case: a client that knows nothing about
recovery keys. A v1 client re-wrapping generates a fresh vault key, so the recovery
envelope it omits wrapped a key that no longer exists; keeping it would leave a recovery
door that opens nothing.

A client that knows only the vault password — a v1 client, or a self-hosted server in
password mode — simply omits the three `recovery_*` fields and is otherwise unaffected.
Sending some but not all three is a `400`: half an envelope is a recovery door that
cannot be opened, so the server refuses to store one.

A claim that omits the token has not asked to replace anything, so losing a race to
mint the first key is not an error: the server returns `200` with the **authoritative**
key material, which may differ from what you sent. Adopt what you get back, recovery
envelope included — one vault has exactly one key. The winner's key is never overwritten.

> Heads-up: a successful re-wrap does **not** currently emit a real-time `change` event, so peers learn about it on their next poll, not instantly. Note it if you depend on instant key propagation.

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

### Retry safety with Mutation IDs

Send a client-generated `Mutation-Id` header on every create, update,
or delete. Reuse that value only when retrying the same intended mutation.

After syntax and field validation succeeds, the first mutation outcome is
recorded. Successful create outcomes remain for the collection lifetime; other
outcomes expire after 30 days. A retry inside its retention window returns that exact outcome
without advancing an object version or collection cursor again, or publishing
another change notification—the retry body is ignored, and the object keeps
the blob key from the original attempt. Completed Mutation IDs are checked
before a one-call create stages ciphertext, so ordinary replays do not create
an extra staged blob. Concurrent requests that both observe the in-progress
claim may stage independently, but only one can finalize it; any unclaimed blob
expires on the normal 24-hour staging window. Reusing the ID for a different mutation kind,
collection, object, or requested version returns:

```json
{ "error": "Mutation-Id reused for different intent" }
```

with status `409`. Classic routes retain the published contract of any non-empty
1–128-character header value; percent-encode that value when using the retained
outcome lookup path. Framed batch creates require 1–128 URL/header-safe ASCII
characters: letters, digits, `.`, `_`, `~`, and `-`. New clients should use
that batch-safe syntax on every route so fallback preserves the ID unchanged.
IDs are currently
optional so older clients continue to work, but clients should use them for
every mutation—especially single-round-trip creates, whose success response may
be lost after the server commits. A request without `Mutation-Id` remains an
independent mutation: each successful create keeps its submitted ciphertext,
but retrying after a lost create response can produce a duplicate
object. Definitive outcomes produced by the mutation
coordinator, including `404` and `409`, are recorded just like success. Requests
rejected before that point—invalid JSON or fields, empty or oversized bodies,
or an invalid Mutation ID—are not recorded because they do not provide a valid
mutation intent.

The server records an in-progress claim for a create Mutation ID before staging
its blob. This closes the window where recovery could report a missing outcome
while the original request was still committing. An abandoned claim becomes
missing after the same 24-hour window used for staged blobs.

If a client has lost the original create body—for example, the local file was
deleted after an ambiguous response—it can recover the successful create
outcome without replaying ciphertext:

```http
GET /api/collections/:cid/create-mutations/:mutationId
```

The response has the normal create shape with `"replayed": true`. A create that
is still staging returns `409 { "error": "mutation still in progress" }`; the
client must retain its pending state and retry. For an existing collection, a
`404` proves that the scoped Mutation ID never committed a create. Clients that
use that result to discard pending state must be released only after a server
advertising `"successful_create_outcomes": "durable"` is deployed.

---

## Objects

An object is a metadata row pointing at a blob. This is its canonical shape for
reads, creates, and updates. Soft-delete responses retain their older compact
projection `{ id, version, change_seq, deleted }` for wire compatibility.

```json
{
  "id": "018f4b2a-6e9a-7c1a-9c3e-1a2b3c4d5e6f",
  "collection_id": "018f4b29-4f5a-7b2c-8d1e-2f3a4b5c6d7e",
  "version": "3",
  "change_seq": "42",
  "deleted": false,
  "blob_key": "018f4b28-11aa-7d3e-9f0a-3c4d5e6f7a8b/018f4b2b-22bb-7e4f-8a1b-4d5e6f7a8b9c",
  "size_bytes": "1024",
  "created_at": "2026-01-15T09:30:00.000Z",
  "updated_at": "2026-01-15T09:31:12.000Z"
}
```

- `id`, `collection_id` — UUID strings.
- `version`, `change_seq` — JSON **strings**, not numbers, even though they're numeric. They're Postgres `bigint` columns serialized straight onto the wire as strings — that's the stable, intentional contract, not an accident awaiting a fix. Rely on it.
- `deleted` — boolean.
- `blob_key` — string, or `null`.
- `size_bytes` — JSON string (same `bigint`-as-string rule), or `null`. The column is nullable, so treat `null` as unknown size rather than assuming a string is always there. Every write path the server offers today populates it.
- `created_at`, `updated_at` — ISO 8601 timestamp strings.

**The trap:** `collectionVersion` (mutation responses), `currentVersion` (the 409 conflict body), and `nextCursor` (paged list responses) are server-computed and are JSON **numbers** — the opposite representation from the row fields above. So comparing `object.change_seq` straight against `nextCursor` or `collectionVersion` compares a string to a number and will never match; coerce one side (e.g. `Number(object.change_seq)`) before comparing. This is the same coercion the cursor rule in the [end-to-end recipe](#end-to-end-recipe)'s "Saving a change" step depends on, where `cursor + 1` is a numeric comparison against `collectionVersion`.

### List / pull
```
GET $BASE/api/collections/:cid/objects?sinceVersion=N   → { "objects": [ ... ] }
GET $BASE/api/collections/:cid/objects/:oid             → { "object": { ... } } | 404
```

**Optional paging.** Add `?limit=N` (positive integer, clamped to a max of `1000`) to cap how many objects a single pull returns — useful for a first sync of a large collection. When `limit` is present, the response gains two additive fields:
```
GET $BASE/api/collections/:cid/objects?sinceVersion=N&limit=500
    → { "objects": [ ... ], "hasMore": true, "nextCursor": 542 }
    → 400 invalid limit
```
- `hasMore` — `true` if more objects remain past this page.
- `nextCursor` — the `change_seq` of the last returned object (a number), or your `sinceVersion` if the page is empty. Pass it as the next request's `sinceVersion` to fetch the remainder; repeat until `hasMore` is `false`.

Omitting `limit` is fully backward compatible: the response is exactly `{ "objects": [ ... ] }` with no `hasMore`/`nextCursor` and no row cap, as before.

### Create
```
POST $BASE/api/collections/:cid/objects
{ "blob_key": "<your-user-id>/<uuid>", "size_bytes": 1024 }
    → 201 { "object": { ... }, "collectionVersion": 1 }
    → 400 invalid blob_key/size_bytes | 404 collection not found
    → 409 { "error": "blob is not staged" }
```
`blob_key` must be your still-staged blob—i.e.
`"<your-user-id>/<blob-uuid>"`, exactly the key returned by `POST /api/blobs`.
A successful mutation claims it; reusing a key that's already claimed (by an
earlier mutation) or whose 24h staging window expired returns the `409` above.
Re-upload the ciphertext to get a fresh staged key and retry under a **new**
Mutation ID. `size_bytes` is still required for validation, but the value you
send is no longer stored—the response's `object.size_bytes` reflects the
actual byte length the server measured when the blob was staged.

### Update (version-guarded)
```
PUT $BASE/api/collections/:cid/objects/:oid
{ "version": 4, "blob_key": "<your-user-id>/<uuid>", "size_bytes": 2048 }
    → 200 { "object": { ... }, "collectionVersion": 7 }
    → 409 { "error": "version conflict", "currentVersion", "currentBlobKey" }
    → 409 { "error": "blob is not staged" }
    → 404 object not found | 400 invalid body
```
`version` must equal the object's current version + 1. `blob_key` must still be
staged—an already-claimed key (by an earlier mutation) or one whose 24h
staging window expired returns the `blob is not staged` `409` above. Re-upload
the ciphertext to get a fresh staged key and retry under a **new** Mutation ID.
`size_bytes` is still required for validation, but the value you send is no
longer stored—the response's `object.size_bytes` reflects the actual byte
length the server measured when the blob was staged.

### Delete (soft, optional race guard)
```
DELETE $BASE/api/collections/:cid/objects/:oid[?version=N]
    → 200 { "object": { id, version, change_seq, deleted: true }, "collectionVersion": 8 }
    → 409 { "error": "version conflict", ... }   // only if ?version is supplied and stale
    → 404 not found
```
Soft delete sets `deleted: true` and bumps the version so peers see the tombstone. Supplying `?version=N` makes the delete lose to a newer concurrent edit (edit-vs-delete race; edit wins). Omit it to delete unconditionally.
Deleting an already-deleted object is a successful no-op: it returns the current
tombstone without advancing the object version or collection cursor. A stale
`?version` still returns `409` against a tombstone.

### Single-round-trip variants

These halve the round trips on high-latency networks by combining the blob upload and the object write. The body is the raw ciphertext (`application/octet-stream`); the server mints the blob key for you. Response shapes and conflict semantics are identical to the two-call versions above, **except** `409 { "error": "blob is not staged" }` cannot happen here—the server mints and stages the blob itself in the same call, so there's no caller-supplied key to go stale.

```
POST $BASE/api/collections/:cid/blob-objects              → 201 { "object", "collectionVersion" }   // create
PUT  $BASE/api/collections/:cid/blob-objects/:oid?version=N → 200 { "object", "collectionVersion" }   // update; ?version required
    → 400 empty body | 404 collection/object | 409 conflict
```

Create responses also include the additive boolean `replayed`. It is `false`
when this request commits the Mutation ID and `true` when the server returns
that Mutation ID's retained outcome. Retry ciphertext never replaces the
original blob.

### Batch single-round-trip uploads

```
POST $BASE/api/collections/:cid/blob-objects/batch
Content-Type: application/octet-stream
```

The request body concatenates up to 200 create or update frames:

```
[u8 operation][u16 identifierLen][identifier utf8][u32 version][u32 blobLen][blob bytes]
```

Integers are big-endian. Operation `0` creates an object: `identifier` is the
client-generated Mutation ID and `version` must be zero. Retrying that frame
reuses the same Mutation ID and returns the original object even when the retry
carries different ciphertext. Operation `1` updates an object: `identifier` is
the object ID and `version` must be the next positive object version. The
current protocol does not attach a Mutation ID to batch updates; their existing
version guard still prevents a retry from advancing the version twice.

The response preserves request order:

```json
{
  "results": [
    { "status": "created", "object": { "...": "..." }, "collectionVersion": 1 },
    { "status": "updated", "object": { "...": "..." }, "collectionVersion": 2 }
  ]
}
```

| Status | Meaning |
|--------|---------|
| `created` | The create Mutation ID committed and returned `object`, `collectionVersion` |
| `replayed` | The create Mutation ID already had a retained outcome; retry ciphertext was ignored |
| `updated` | The versioned update committed and returned `object`, `collectionVersion` |
| `conflict` | Stale update; returns `currentVersion`, `currentBlobKey` |
| `not_found` | The update target is absent or not owned by the caller |
| `too_large` | This entry exceeds `MAX_BLOB_BYTES` |
| `error` | This entry failed operationally; later entries are still attempted |

Entries are coordinated independently through the same Collection Contents
module as classic writes. Each successful non-replay mutation publishes its
own transactional SSE change event; conflicts, failures, and replays publish
none. Malformed framing, an empty batch, or more than 200 entries rejects the
complete request with `400`. A request body over `MAX_BATCH_BYTES` (default
32 MiB) returns `413`.

---

## Blobs

Opaque encrypted bytes. Keys have the form `<user-id>/<blob-uuid>` and are always scoped to you.

```
POST   $BASE/api/blobs                    → 201 { "key": "<user-id>/<uuid>" }   // body: raw bytes
GET    $BASE/api/blobs/:userId/:blobId    → 200 <raw bytes>  (application/octet-stream) | 404
DELETE $BASE/api/blobs/:userId/:blobId    → 204 | 409 { "error": "blob is in use" }
```
- Upload an empty body → `400`. A key you don't own → `404` on GET/DELETE (no existence leak).
- Uploading stages a blob for a fixed 24 hours. Creating or updating an object
  atomically claims it. DELETE is idempotent for staged or absent blobs,
  including legacy files not yet adopted into the ledger. Any non-staged ledger
  state—claimed, retained, purgeable, or legacy-shared—is controlled by
  object/collection lifetime and returns `409`.
- Replacing an object's blob retains the prior blob for 365 days so clients can
  fetch a merge ancestor. Deleting a collection makes its claimed and retained
  blobs immediately eligible for asynchronous removal; quarantined
  legacy-shared blobs remain non-deletable. These lifetimes are fixed protocol
  policy, not server configuration.

### Batch fetch

```
POST $BASE/api/blobs/batch    → 200 <binary frames>   // body: { "keys": ["<user-id>/<uuid>", ...] }
```

Fetches up to **200** blobs in one request — removes the per-request overhead that makes large pulls (first sync especially) latency-bound. The response is a stream of binary frames, one per requested key **in request order**, integers big-endian:

```
[u16 keyLen][key utf8][u8 status][u32 blobLen][blob bytes]
```

| Status | Meaning |
|--------|---------|
| `0` ok | `blobLen` bytes of blob follow |
| `1` missing | Absent, malformed, or not owned by you (no existence leak — mirrors the single GET's `404`); `blobLen = 0` |
| `2` omitted | The complete encoded response would exceed the server cap (`MAX_BATCH_BYTES`, default 32 MiB) if this blob were included; re-request omitted keys in a fresh batch. `blobLen = 0` |

The cap counts every frame's 7 fixed bytes, UTF-8 key bytes, and blob bytes. The
first available blob is always sent even if its frame alone exceeds the cap, so a
client always makes progress. Mandatory missing/omitted frame metadata may also
exceed an unusually small operator-configured cap. `> 200` keys, a key over 128
chars, an empty list, or a non-JSON body → `400`; a request body over 64 KiB → `413`.

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
4. `PUT /api/collections/:id/key` once, with your salt + KDF params + password-wrapped vault key, plus the three `recovery_*` fields if you also wrap it under a recovery key. On other devices, `GET .../key` to unlock.
5. Full pull: `GET /api/collections/:id/objects?sinceVersion=0`. For each row, download its blob (`GET /api/blobs/:userId/:blobId`), decrypt, apply. Set `cursor` to the highest `change_seq`.

**Saving a change**
1. Generate one Mutation ID for the intended change and encrypt locally.
2. Upload the blob (`POST /api/blobs`) or use `POST/PUT .../blob-objects` to do it in one call.
3. Create (`POST .../objects`) or update (`PUT .../objects/:oid` with `version = lastSeen + 1`), sending `Mutation-Id`.
4. If the response is lost, retry the same request with the same Mutation ID.
   On `200/201`: advance `cursor` to `collectionVersion` **only if** it equals
   `cursor + 1`—that's the only value proving no other device's change landed
   in between, so a gap means leave `cursor` alone and pull instead. On a
   version-conflict `409`:
   fetch `currentBlobKey`, merge, and use a new Mutation ID for the new intent
   at `currentVersion + 1`.

**Staying in sync**
1. Open `GET /api/sync/events`. On `ready` and on every `change`, pull `GET .../objects?sinceVersion=<cursor>` and apply.
2. Run a safety poll on the same pull every 30–60s.
3. On disconnect, reconnect; the fresh `ready` triggers a catch-up pull.
