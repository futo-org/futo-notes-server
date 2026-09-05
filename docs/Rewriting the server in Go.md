We are rewriting the FUTO Notes server in Go.

Rewrites are generally a bad idea, especially this early. The notes server works just fine, so why rewrite? A few things.

1. TypeScript works, but it's not the ideal language for what we're trying to do. JavaScript was never designed to be a server language, and it's not particularly performant for what we want it to do. Additionally, the runtime can be a bit slow.
2. Reliability. As I was typing this, the server went down! Investigating now. The server needs to be rock solid.
3. Portability. With Bun, we were able to get a single executable for the server, but this means being tied to a single project if we want to keep this feature. Go is a compiled language.
4. Security. Go has a robust standard library. While we didn't have many npm packages on the TypeScript server, it's still nice to have fewer deps.
5. First-class SQL support. Part of the standard library, too.
6. Yucca also uses Go.

## For now, preserve existing behavior under v1
We need to start versioning the API. But for now, the idea is to create the Go server as a hot swappable option for the current TypeScript server. This means punting on the other changes we're thinking about, like introducing SQLite, CRDTs, etc.

There are some warts and oddities in how the API is currently implemented. Mostly cosmetic, things like mixing the standard for naming. This is ok! We'll fix these later.

This also means that auth stays the same as well.

## How Authentication Works
POST `{password}` to `/api/auth/password/login`.  No user yet? Create one. If there is one, check the password against what we have from the .env. It either exists in plaintext or hashed. If valid, return a JSON token and a cookie. Cookie is really just for browsers, which we don't use.

The hashed form is scrypt, stored self-describing as `scrypt:N=16384,r=8,p=1:salt_hex:hash_hex`. Every one of these has to match exactly or no existing password verifies. Go equivalent is `scrypt.Key(password, salt, 16384, 8, 1, 64)`.

**N** - 16384, work factor. costs ~16 MiB of memory per hash
**r** - 8, block size
**p** - 1, no parallelism
**key length** - 64 bytes
**salt** - 32 random bytes

The stored string carries N/r/p, but the TS verifier ignores them and uses its own constants. Match the constants, not the string.

Every subsequent request to a `/api/*` route, except the `/login` endpoint, gets a sesion and user attached to it. The SHA-256 hash of the session token is stored on the server for security. The user attribute is used to make sure you don't access someone else's data, which will be useful for the hosted version.

The token expires after 7 days. No automatic renewal - paswords are stored on the client, so getting a new token is seamless and invisible to the user.

Invalid or missing tokens results in a `401`.

Go deviation: a malformed `Authorization` header (wrong scheme, or no space after `Bearer`) is treated as *no credentials* and gets `401 {"error":"unauthorized"}`. The TS server instead hashed the whole header string, found no session, and answered `invalid_session` with a `WWW-Authenticate` header. No known client sends malformed headers. Accepted.

Compatibility details confirmed by the differential harness:

* Logout clears the `session` cookie with the same name and `/` path used at login, but the clearing `Set-Cookie` omits `HttpOnly`, `Secure`, and `SameSite`, matching the legacy server. Those attributes are not part of a cookie's identity; name, domain, and path determine which cookie is removed. Accepted.
* An authenticated request to an unmatched `/api/*` route returns `404 Not Found` as `text/plain; charset=utf-8`, with no trailing newline, matching the legacy server. Accepted.
* The capability document's `version` describes the server implementation and is expected to differ between the TypeScript and Go binaries. The comparison harness reports that field as an accepted deviation while requiring every other capability field to match. Accepted.

## Collection Keys
Each collection has its own key to encrypt its contents. The key is a random AES-256-GCM, then wrapped client-side by a salt + password. The salt per collection is stored in the database.

Collection keys != your overall password, btw.

Here's how the flow might work to enable encryption on vault content:
- device A derives the vault key and wrapping key (using the vault password + salt)
- device A PUTS the encrypted vault key along with the salt
- device B downloads the encypted vault key, derives the wrapping key using the same method as Device A, uses it to unwrap the vault key. Boom, now both devices have the same vault key without the server ever seeing it.

## Retries and Mutations
When the client tries to create an object/blob, there's always a chance that it doesn't go through properly. The problem is that the client can't know if the upload was successful or not. That's where mutations come in. When the client is creating something net new, it attaches a mutation ID, which is a way for it to check back in on it later.

When it checks in, it asks about the mutation ID. Let's say you're creating a note and the first POST to the server fails, but you keep typing. Using the mutation ID, we can check to see its status, then if it did succesfully upload, we will try to update the existing entry on the server. If it was not successful, try to create the object/blobs again.

When checking the mutation table, there are multiple responses possible.

* 200: the first create worked fine, move along! returns the original object and `replayed: true`
* 409: pending, wait!
* 404: can't find it, friend, try again with a new ID

### Mutation ID format
Go accepts one syntax only: 1-128 characters matching `^[A-Za-z0-9._~-]+$`. That's the RFC 3986 unreserved set, so UUIDs qualify.

The TS server also accepts a looser legacy syntax on the classic routes. We're dropping it. Cost: a retry carrying a legacy-shaped ID gets a `400` instead of its recorded outcome, so a create in flight at cutover could end up duplicated. Accepted.

The restriction exists because the ID has to survive three transports - an HTTP header, a URL path segment (`create-mutations/:mutationId`), and a binary frame in a batch body. A `/` in the ID makes the recovery route unmatchable.

## Claiming Blobs
When blobs are uploaded, they need to be "claimed", or owned by an object. When a blob gets replaced, the old one sticks around for a full year. After that, it is purged. Blobs are also purged when a collection is deleted.

## Database Layout

### users
**id** - uuidv7, internal
**sub** - id that comes from the identity provider
**name**
**email**

### sessions - client sessions
**id**
**user_id**
**access_token_hash**
**expires_at**

### collections
**id**
**user_id**
**current_version** - see cursors section
**key_salt** - used for deriving the wrapping key
**key_kdf** - info on key derivation
**encrypted_vault_key** - encrypted by client
**key_updated_at**
**created_at**

### objects
**id**
**collection_id**
**user_id**
**version** - per object counter
**change_seq** - per collection counter
**deleted**
**blob_key** - has nothing to do with encryption key. a filename/path.
**size_bytes**
**created_at**
**updated_at**

### blob_ledger
keeps track of blobs, is the actual source of truth!
**blob_key** - a location
**user_id**
**size_bytes**
**state**
**collection_id**
**object_id**
**object_version**
**created_at**
**state_changed_at**

### mutation_results
keeps track of retries, essentially. new rows created when a transaction contains a mutation_id. expires after some time.
**user_id**
**mutation_id**
**kind**
**collection_id**
**object_id**
**requested_version**
**result**
**created_at**

### server_config
**key**
**value**

### kysely_migration
**name**
**timestamp**

### kysely_migration_lock
**id**
**is_locked**

## Batching
Not done as a single transaction. If one of many fail, the rest still go through. JSON response comes back.

Formats for framing:

**Upload**
```
[u8 op][u16 idLen][id utf8][u32 version][u32 blobLen][bytes]

op 0 = create   id = Mutation-Id,  version must be 0
op 1 = update   id = object uuid,  version >= 1
blobLen must be non-zero; entries repeat to EOF
```

### Upload response JSON
One result per entry, in request order.
```
200 {"results": [ ... ]}

{"status":"created",  "object":{...}, "collectionVersion":N}
{"status":"replayed", "object":{...}, "collectionVersion":N}
{"status":"updated",  "object":{...}, "collectionVersion":N}
{"status":"conflict", "currentVersion":N, "currentBlobKey":"..."|null}
{"status":"not_found"}
{"status":"too_large"}
{"status":"error", "error":"..."}
```

**Download**
```
[u16 keyLen][key utf8][u8 status][u32 blobLen][bytes]

status 0 = ok        bytes present
status 1 = missing   blobLen = 0
status 2 = omitted   blobLen = 0 (cap reached)
```

## Ints vs strings on the wire
Default is a JSON number. These are the exceptions - fields read straight off an object row go out as **strings**.

**version** - string
**change_seq** - string
**size_bytes** - string, and nullable

## Server-side Events
GET `/api/sync/events/` is the only SSE we do.

Carries only `{collectionId, currentVersion}`. A cheap and easy way to see what the current cursor is at.

Use Postgres `LISTEN`/`NOTIFY` so that in the future if we have multiple servers and one SoT postgres, evreyone gets updated properly.

## Recurring Jobs
There are four recurring jobs that run across two timers.

**Timer 1** - runs 60 seconds after boot, the once every hour
**Timer 2** - runs runs 60 seconds after boot, then every 6 hours

### Session reaper - Timer 1
Runs 60 seconds after boot and then every hour. Checks for expired sessions and deletes them so we don't have a massive list of dead sessions. Expired sessions are deleted when someone presents a session token that is expired, but this doesn't always happen! Hence the cleanup.

### Storage reconciliation - Timer 2
Finds files with no blob_ledger row and adds a row with that blob with the `staged` designation. Capped at 500.

Won't do anything often, but could be useful in scenarios where blobs get uploaded, then Postgres has to get rolled back. By setting them to staged, they can get purged later by garbage collection.

### Mutation-results expiry - Timer 2
Three tiers of deletion rules.

- pending results expire after 24 hours
- successful creates are never expired
- everything else goes after 30 days

### Blob garbage collection - Timer 2
Finds blobs that weren't claimed in 24 hours, any blobs that got superseded by a new update more than a year ago, or set to purgeable - which happens when a collection is deleted.

## Enforcing Limits in Middleware
Clients can hit limits - they exist.

* MAX_BLOB_BYTES - 100 MiB.
* MAX_BATCH_BYTES - 32MiB
* MAX_BATCH_KEYS - 200
* MAX_BATCH_KEY_CHARS - 128 for each requested key - keys being filepaths
  * probably better ways to do what this is trying to accomplish, which is to avoid malformed/bad keys (filepaths)
* batch request body - 64 KiB
* max_batch_entries - 200
* MAX_PULL_LIMIT - 1000
* Mutation-Id length - 1-128
* AUTH_RATE_LIMIT - 10 per 60s
* session TTL - 7 days

With the fixed v1 defaults, a batch-upload entry cannot reach the per-entry `too_large` result: the 32 MiB whole-request cap rejects the request before an entry can exceed the 100 MiB blob cap. The response variant remains in the wire schema for a future configuration where `MAX_BATCH_BYTES` is higher, but the v1 comparison gate does not claim to exercise an unreachable branch.

## Cursors
There are two cursors - one for objects, one for collections.

The object cursor is used as a way to prevent conflicts. If I am editing and we don't send over currentCursor + 1 in the POST, someone else is editing, so cause a collision (conflict copy) instead of overwriting.

The collection cursor is useful because each change that is made to objects will have a collection cursor number associated with it, and the client knows the last cursor position it has. So on next sync, you can just ask for all changes since a certain cursor point.

## Buildout order and API routes
Intentionally leaving out details on API. [Full OpenAPI spec](./openapi.yaml) available.

* **Unauthenticated routes**.
  * GET `/`
  * GET `/health`
* **Authentication**. Done when I can protect data behind credentials. Will probably skip the `/dev` version of authentication just to keep things simpler.
  * POST `/api/auth/dev/login`
  * POST `/api/auth/password/login`
  * GET `/api/auth` - return `user` object
  * POST `/api/auth/logout`
* **Centralized/shared error logging**. Write errors out to a standardized location to make it easier to troubleshoot for other users.
* **Postgres** support. Use the `database/sql()` to implement, as this makes it easier to swap in SQLite later.
* **Collections** - the basis of encryption
  * POST `/api/collections/` - either creates a new collection or tells you one exists. can't have two collections
  * GET `/api/collections`
  * GET `/api/collections/:id`
  * DELETE `/api/collections/:id` - also deletes associated mutation results
  * GET /api/collections/:id/key
  * PUT /api/collections/:id/key
* **Objects** - canonical pointers to blobs
  * GET `/api/collections/:id/objects`
  * GET `/api/collections/:id/objects/:objectId`
  * POST `/api/collections/:id/objects` - needs blobId and userId, so you have to upload the blob first (see POST `/api/blobs`)
  * PUT `/api/collections/:id/objects/:objectId` - good for replacing, pair with the blobs API
  * DELETE `/api/collections/:id/objects/:objectId`
* **Blob objects** - blobs + objects, used for editing the blob and object at the same time
  * POST `/api/collections/:id/blob-objects`
  * PUT `/api/collections/:id/blob-objects/:objectId` - similar to the object PUT, but a little simpler. Good for smaller transactions.
  * POST `/api/collections/:id/blob-objects/batch`
* **Mutation IDs** - mutations are used for cases where something might fail. you can always just try again with the same mutation ID. this endpoint is useful as a backup if that doesn't work. Gives you back status.
  * GET `/api/collections/:id/create-mutations/:mutationId`
* **Blobs** - ciphertext
  * POST `/api/blobs`
  * GET `/api/blobs/:userId/:blobId`
  * POST `/api/blobs/batch`
  * DELETE `/api/blobs/:userId/:blobId`
* **Sync SSE**
  * GET `/api/sync/events` - useful for SSE, tells you when to pull.


## Lifecycles of Create, Update, and Delete
Making changes to a collection is not simple!

### Create
1. Insert the `mutation_results` row with the client-minted `mutation-id`
2. Server creates the blobKey, `{userId}/{uuidv7}`
3. `INSERT blob_ledger` with staged status, contains blobKey
4. `store.put(blobKey, bytes)` the thing we actually wanted to do
5. **BEGIN** SQL transaction
6. advisory-lock Mutation-Id so other clients can't mess us up
7. `SELECT mutation_results` - see if this is a replay or a mismatch. i.e., did we try this  before? if so, anything we should know?
8. `SELECT collections... FOR UPDATE` - lock the collection row, make sure the collection exists and belongs to the user
9. `SELECT blob_ledger ... FOR UPDATE` - lock the blob_ledger from earlier, amek sure it is staged and and less than 24 hours old
10. `UPDATE collections SET current_version + 1 ... RETURNING` the collection gets a version bump, which we return to use later
11. `INSERT objects change_seq = returned version number ... RETURNING version = 1` insert into the object table, give it the change_seq equal to the collection_version we got returned to us, set the object version to 1
12. Update the blob_ledger to be claimed, give it the object_id and object version we just got back from previous step
13. publish notification via SSE
14. upsert mutation_results with the outcome
15. **COMMIT**

### Update
Remember that for an update, we are creating a new blob and changing what the `objects` table points to.

1. mint blob ledger key `{userId}/{uuidv7}`, insert blob_ledger row, store.put(bytes)
2. **BEGIN** transaction
3. Check the mutation_results table in case this is a retry. If so, we need to know the status.
4. Find the collections row, lock it since we're going to be changing the collection version
5. find the relevant objects row, lock it, check the version
6. check to make sure we have the object.version (from client) - 1, meaning the server is just one step behind. if not, throw a 409.
7. lock the blob_ledger row
8. update the collections row to increment the collection version by one, return that value
9. update the objects row, setting the object version, the change_seq (collection version), blob_key, size_bytes, updated_at. return those values.
10. old blob is retained, set object_id and object_version to NULL  on blob_ledger. Old blob will be purged in 365 days.
11. for new blob, set to claimed, set object_id and object_version on blob_ledger
12. publish SSE notification
13. upsert the mutation_results entry
14. **COMMIT**

### Delete
Blobs on-disk aren't immediately deleted.

1. **BEGIN** transaction
2. lock the mutation-id
3. check mutation_results to see if this has been tried before and if so, what happened?
4. check for and lock the collections row
5. check for and lock the objects row
6. If the client requested deletion of a specific version, check that version and return 409 when it does not match; this applies to tombstones too. Otherwise, if the object has already been deleted, return the current tombstone as a successful no-op without changing the object version, collection cursor, or publishing an event.
   Go deviation: the legacy TS server increments the tombstone and collection cursor again on every re-delete. The OpenAPI contract now specifies a successful no-op, which avoids spurious sync work and makes retries stable. A client receives an older version/cursor than it would from TS only when it deletes an object that is already a tombstone. Accepted.
7. Update the collection row's version to +=1, return the value
8. Update `objects` table, set deleted to true, bump the object version number and change_seq
9. Release the blob: set blob_ledger state to `retained`, null out object_id and object_version, stamp state_changed_at. The bytes stay readable on disk so the blob can still serve as a merge ancestor, and the 365-day retention clock starts — the same treatment an update's superseded blob gets. This covers the blob the object held at delete time. A later update against the tombstone claims its replacement blob while leaving `deleted = true`, so a tombstone can still carry a `claimed` blob indefinitely; nothing releases those today.
   Go deviation: this plan originally kept the blob `claimed`. A claimed blob is never eligible for garbage collection, so a tombstone held its ciphertext forever, while an updated object's old blob was reclaimed after a year. Same merge-ancestor rationale, so both now use the same bounded window. Accepted.
10. upsert mutation_results
11. SSE publish
12. **COMMIT**

## Potential gotcha: using multiple connections
We rely on SQL transactions for writes and other operations to avoid conflicts from multiple clients operating on the same database. Since Go's sql engine has fewer guardrails than Kyseley in this regard, we have to make sure not to keep referencing `db` when we mean to be using the same existing transaction, for which we need to use the `tx` object instead.

## Migrating existing users to the new server
In the Go rewrite, include all server migrations up to `011_mutation_results`, the current one on the server, and give it the same name. Use the `kysely_migration` table. Will need a folder to store the migrations.

## Notes on building this agentically
The goal is to have this be a painless, invisble transition. We could build a harness that uses the Rust core to simulate a client, perhaps two copies, and has the client(s) do a bunch of transactions with a copy of the old and new server and compare at every step. We just want the behavior visible to the client to be the same, it doesn't have to be 100% the same from within the server. This is a gate to launch.

## Things to fix in the future
* Naming - key vs vault key is not always clear in the collections table
* might be able to consolidate between objects, blob-objects, blobs
* objects table has multiple counters in it
* "blob_key" in objects has nothing to do with an encryption key. poor naming.
  * "key" in general is just not a good name for something not having to do with auth/crypto
* wonder if we can do away with the mutation stuff
* MAX_BATCH_BYTES should be higher
* Mutation-Id length - 1-128 - probably don't need this any more, should just use UUID or something and be done with it
* Ints vs strings is a small mess
* SSE is nice, but not a requirement
* there are two paths to doing notes updates and we could probably consolidate
* delete path is somewhat odd. we bump the version number for the object? hm.
* creating a collection and claiming its key could be a single call
