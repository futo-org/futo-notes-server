# S3 blob storage plan

Status: live on hosted staging since 2026-10-22. Reviewed by Codex; its
findings are folded in.

## Current state (2026-10-22)

Both staging hosts store blobs in one Garage bucket, and each serves every
blob. The two-host check (write through one host, read through the other,
both directions) passed at 21:56 UTC after the cutover, and again at 23:06 UTC
after a later redeploy put both hosts on `candidate-be066c95`.

| Piece | Where |
|---|---|
| Server S3 backend, tests, CI Garage | futo-notes-server `32fadab` on `hosted-server` (pipeline #38530 green) |
| Garage node on manifest-staging | manifest-inventory `acee419`, `inventory/futo-notes-s3/garage-staging.yml` |
| Cutover of `hosted-staging.yml` to S3 | manifest-inventory `b11c809` |
| Garage RPC secret and S3 key pair | new fields on 1Password `Manifest/futo-notes-hosted-staging` |

Left to do:

- **Deploy step 6:** remove `/futo-notes-hosted-staging-data/blobs` on both
  hosts by hand. Nothing reads it now. It holds the 5 staged test blobs from
  the 2026-10-21 measurement (131 bytes, no objects point at them); their
  ledger rows go to blob GC, whose `DeleteObject` on the missing S3 keys
  succeeds.
- **Backups for the bucket** before paying users (see the end of Deploy).
- **A third host for production Garage** (see Open questions).

Found along the way, outside this plan:

- **The Postgres replica on manifest-staging2 lacks `futo_notes_hosted_staging`.**
  `pg_is_in_recovery()` is true there, but it lists only `postgres` and
  `futo_notes`, so it seems to have stopped following the primary before that
  database was created on 2026-10-21. The app is unaffected, since both hosts
  reach the primary through `pghaproxy` at `$PODNAME:15510`. A failover to
  staging2 would not have the database. Raise with devops.
- **Every push to `hosted-server` starts a full Manifest run**, because its
  pipeline triggers `deploy:hosted-staging`, and a new Manifest run cancels the
  unfinished jobs of the one in progress. A doc-only push on 2026-10-22 cut off
  the cutover run's `update-hosts` and `backup-check-partial` (the next run
  redid both). Check `devops/manifest` pipelines before pushing here or to
  manifest-inventory.

## Goal

Hosted staging runs on two hosts behind one load balancer, and every host
can serve every blob. Before this plan it couldn't. This is the "S3 blob store" that ADR 0004
and `Hosted service plan.md` §7 call the first task after the MVP; running two
hosts moved it ahead of launch.

The client sees no change. Every route, status code and response body stays as
it is.

## Why now

`hosted-staging.yml` deploys the server to manifest-staging and
manifest-staging2. Postgres is shared, but `BLOB_DIR=/data/blobs` is each
host's own disk. Measured on 2026-10-21 against staging: a blob written through
one host is `404` from the other, in both directions. The load balancer sends
everything to manifest-staging until it stops answering, which happens on every
deploy. Any note saved during that window has its bytes only on
manifest-staging2, and once traffic moves back, every other device gets `404`
for it.

Everything else is already safe across hosts. Sessions, Login Hand-offs and
pairings are in Postgres. The SSE doorbell crosses hosts through Postgres
`LISTEN/NOTIFY` (measured: 25 ms from a write on one host to the event on the
other). Blob GC already locks rows with `FOR UPDATE SKIP LOCKED`, so two hosts
can run it at once. The one per-host thing left is the login rate limiter,
which lives in memory, so two hosts allow twice the attempts. That's
acceptable and out of scope.

## Shape

Follow Yucca's `packages/michael` (github.com/insertish/yucca): the server
talks to S3 with `aws-sdk-go-v2`, using a configured endpoint and path-style
addressing, and streams bytes through itself. No presigned URLs, so auth and
the blob-key namespace check stay where they are.

### 1. A storage backend behind `blobs.Store`

`internal/blobs` keeps the ledger logic (`Stage`, `Delete`) and gets a small
backend with two implementations, filesystem (today's code) and S3. The server
uses three operations:

| Operation | Filesystem | S3 |
|---|---|---|
| `Put(ctx, key, data)` | `MkdirAll` + `WriteFile`, as now | `PutObject` with a `bytes.Reader` over the upload already in memory: no second copy, no multipart |
| `Open(ctx, key)` → reader, size | `os.Open` + `Stat` | `GetObject`, size from `ContentLength` |
| `Remove(ctx, key)` | `os.Remove`, missing is success | `DeleteObject`, already idempotent |

GC's `blobFileExists` check goes away rather than becoming a fourth operation
(an extra `HeadObject` per purge only fed a counter). `FilesRemoved` then
counts successful removals, not files confirmed present beforehand.

Errors:

- Only a confirmed missing object is `blobs.ErrNotFound` (S3 `NoSuchKey`, or
  HTTP 404 from `GetObject`). Handlers map it to `404` and stop checking
  `os.ErrNotExist`.
- Everything else (auth failure, timeout, S3 down) stays an error: `500` on
  single routes, and an aborted stream on the batch route. "S3 is down" must
  never read as "your note doesn't exist".
- `GetObject` with no `ContentLength`, or a negative one, is an error, never
  size 0. The batch frame header carries the length before the bytes.

Context and timeouts:

- Every backend call takes a context. Handlers pass the request context, and
  jobs pass theirs. Today `Stage`, `Open` and `Remove` take none.
- The S3 HTTP client gets a dial timeout and a response-header timeout, so a
  stalled S3 server fails a request instead of hanging it. A body being
  streamed to a client is bounded by the request context, which ends when the
  client goes away.

Keys stay `{userId}/{uuid}`, used as the object key unchanged in one bucket.

No write-once (`If-None-Match`): keys are server-minted UUIDv7s, so a collision
is not a case to handle, and leaving it out keeps the backend to calls every S3
server supports.

### 2. Handlers

- `GET /api/blobs/{userId}/{blobId}`: the filesystem path keeps
  `http.ServeContent` (it hands back an `*os.File`, which can seek), so
  self-hosters keep `Last-Modified`, `Range` and conditional requests exactly as
  now. The S3 path sets `Content-Type: application/octet-stream` and
  `Content-Length` and copies the body; `HEAD` gets the same headers and no
  body, which `net/http` already does. The S3 path drops `Range`,
  `Last-Modified` and conditional requests. That's deliberate: the client never
  sends those headers (checked in `crates/futo-notes-sync` on the client's
  `origin/main`), and the OpenAPI spec promises only 200/401/404 and
  octet-stream. A copy that fails partway aborts the response.
- `POST /api/blobs/batch` (`streamBlobBatch`): it needs the size before writing
  a frame header. It takes that from `Open` instead of `Stat`, checking it
  before the header is written. The existing "short read fails the response"
  check stays, and it covers an S3 body that errors partway; the spec requires
  that abort (`docs/openapi.yaml:1890`).
- Upload routes don't change; they already go through `blobs.Stage`.

### 3. Jobs

- **Blob GC**: calls `Remove` on the backend. Two hosts running it at once is
  already safe: `purgeBlobRow` locks with `FOR UPDATE SKIP LOCKED`.
- **Storage reconciliation** (`jobs.ReconcileStorage`) walks `BLOB_DIR` and
  adopts files the ledger doesn't know about. It stays for the filesystem
  backend and doesn't run on S3, because the ledger is authoritative (ADR 0001)
  and there are no legacy files in a new bucket.
- **Known gap: orphaned objects.** On the filesystem, reconciliation re-adopts
  a file whose row was deleted but whose bytes weren't. On S3 nothing does.
  Both places commit the row before removing the bytes: GC (`purgeBlobRow`,
  then `Remove`) and a client's `DELETE /api/blobs/...` (`blobs.Delete`). Both
  already log the failed key. Cleanup, if the logs ever show it, is by hand:
  delete those keys from the bucket. Listing the bucket to adopt orphans is a
  later task, not this one.

### 4. Config

S3 mode is on when `S3_ENDPOINT` is set. In that mode all of these are
required, and boot fails naming any that are missing:

| Variable | Notes |
|---|---|
| `S3_ENDPOINT` | e.g. `http://100.76.177.70:3900` |
| `S3_BUCKET` | |
| `S3_ACCESS_KEY_ID` | |
| `S3_SECRET_ACCESS_KEY` | |
| `S3_REGION` | optional, default `us-east-1`; signing needs some value |

Path-style addressing is always on. Garage, SeaweedFS, Ceph RGW and R2 all
accept it, so it isn't a setting.

At startup the server runs `HeadBucket` with a 10 s timeout and exits if the
bucket is unreachable or missing, the same way it treats an unreachable OIDC
issuer. It doesn't create the bucket.

`BLOB_DIR` is ignored in S3 mode. The default stays filesystem, so self-hosters
and the SQLite default are untouched. Places that read `BLOB_DIR` today:

- The SQLite fresh-database guard (`prepareSQLiteFile`, `internal/db/db.go:240`)
  refuses to create a new database next to existing blob files. It only runs
  with the filesystem backend.
- The Dockerfile entrypoint creates and chowns `BLOB_DIR`. That's harmless in
  S3 mode, so it stays.
- `cmd/hostedrehearsal` builds its server's environment from `os.Environ()`, so
  stray `S3_*` variables would switch it to S3. It drops them, staying on the
  filesystem backend it sets up.
- `cmd/compare` and the docker-compose files stay on the filesystem.

### 5. Dev UI

`/dev` shows which backend is active (directory, or endpoint + bucket). The
"storage reconciliation" job button reports that it doesn't apply on S3,
instead of walking a directory that isn't used.

### 6. Tests

- One backend contract test (put, open with size, open missing →
  `ErrNotFound`, remove, remove missing) run against both backends. The
  S3 run needs `BLOBS_TEST_S3_ENDPOINT` (plus `_BUCKET`, `_ACCESS_KEY_ID` and
  `_SECRET_ACCESS_KEY`) and skips without it, the same way the Postgres tests
  skip without `*_TEST_DATABASE_URL`. `internal/blobs/blobstest` hands both
  packages the list of backends.
- The server route tests for download, batch fetch, `POST /api/blobs` and the
  blob-object create and update routes also run once on S3 when that variable
  is set.
- Failure tests against a fake S3 (an `httptest` server): a missing
  `ContentLength`, a body shorter than its length, and a body that errors
  partway through all abort the batch stream. A `500` from S3 on download is a
  `500`, not a `404`. A stalled S3 fails within the timeout instead of hanging.
- CI: `test:go` runs Garage (v2.4.1, the same as staging) with
  `garage server --single-node --default-bucket`. That creates the layout, an
  access key and the bucket from `GARAGE_DEFAULT_ACCESS_KEY`,
  `GARAGE_DEFAULT_SECRET_KEY` and `GARAGE_DEFAULT_BUCKET`. The flags don't
  make the config file optional (checked 2026-10-21: without `/etc/garage.toml`
  it exits with `Failed to read config file`), and a GitLab service container
  can't be given a file, while the image is `FROM scratch` with no shell to
  write one. So the job writes the config and starts Garage itself, then waits
  for the S3 port before the tests start.
- A two-host check, run by hand after deploy: write through
  `100.76.177.70:3011`, read through `100.127.246.105:3011`, and the reverse.
  Both must be `200`. That's the test that failed on 2026-10-21.

## Deploy (manifest-inventory)

S3 server: **Garage** (v2.4.1, AGPL-3.0, single binary), chosen 2026-10-21.

- Not MinIO: its open-source repo was archived in April 2026, and its images
  stopped in October 2025.
- Garage over SeaweedFS and Versity Gateway: simple to run, the most
  vouched-for in the Hacker News threads on replacing MinIO, and it can later
  replicate across hosts.
- The server only uses put, get and delete, so moving to Yucca's Ceph RGW or
  anything else later is a config change.

**Replication needs three hosts.** Garage's docs (configuration reference,
`replication_factor`):

| Setup | What happens when one Garage node is down |
|---|---|
| 1 node, `replication_factor = 1` | Nothing to fall back on. The docs call it test-only. |
| 2 nodes, factor 2 | Reads work, **writes fail**. Worse than one node: either host failing stops writes. |
| 3 nodes, factor 3 | Reads and writes continue. About 3× the stored size in disk. |

Staging has two hosts, so staging runs **one node** (`--single-node`). That is
the same single point of failure as any one-box option, and fine for an
internal alpha. Production runs three nodes on three separate hosts with factor
3, which needs a third host.

Steps 1 to 5 are done; step 6 is left.

1. **Done** (`acee419`, deployed by Manifest #38536). New
   `inventory/futo-notes-s3/garage-staging.yml`: one Garage node on manifest-staging,
   with a host volume and the S3 port 3900. Manifest maps a container port to
   the same host port when no other project holds it (`sm_utils.py`
   `assign_ports_to_pod`); no inventory file declares 3900. It starts with
   `garage server --single-node --default-bucket`, taking the RPC secret,
   access key and secret from new fields on the 1Password item
   `Manifest/futo-notes-hosted-staging`, and the bucket name from a plain
   variable. That creates the bucket itself, so no hand step is needed.
   - Its own project folder, not `inventory/futo-notes`: adding a port to a
     pod makes Manifest recreate the pod, which would restart Patroni and both
     sync servers on manifest-staging.
   - `garage.toml` goes in through the manifest's `files:`. It pins
     `s3_region = "us-east-1"` to match the server's `S3_REGION` default;
     Garage rejects a request signed for any other region (`400`, checked).
   - Health checks use the exec form `["CMD", "/garage", "status"]`, because
     the image has no shell and Manifest's plain-string checks run through
     `/bin/sh -c`. Checked locally under podman 5.8.7: the startup check
     passes and the container reports `healthy`.
   - Garage landed on host port 3900. A signed `HeadBucket` with the
     1Password keys returns `200`.
2. Both hosts use the direct tailnet address,
   `S3_ENDPOINT=http://100.76.177.70:3900` (decided 2026-10-21). That's fine
   for one node. Production's three nodes go behind a per-host HAProxy route,
   the way Postgres is reached through `pghaproxy` at `$PODNAME:15510`. Port
   3900 was not probed from manifest-staging2 itself (no SSH access). The
   server's boot-time `HeadBucket` covered it: staging2 came up healthy in S3
   mode, and the two-host check passed through it.
3. **Done** (`b11c809`, deployed by Manifest #38559). `hosted-staging.yml`:
   add the `S3_*` variables and secrets, and drop `BLOB_DIR` and the `/data`
   volume. It went out after `candidate-32fadabb`, the first image that
   understands `S3_*`, was on both hosts; without that image, removing
   `BLOB_DIR` and the volume would have put blobs on the container's own disk.
4. Push only when no full Manifest run is in progress: a manifest-inventory
   push, or a push to `hosted-server`, cancels it. On 2026-10-22 one push
   cancelled a teammate's run (#38553); the next run redeployed their change.
5. **Done.** Cut over before anyone had a vault on staging: `collections` and
   `objects` were both empty right before the deploy (checked 2026-10-22),
   and the two-host check then passed in both directions. Empty mattered
   because a rolling deploy briefly runs one host on the filesystem and one on
   S3, which brings back the 404 for blobs written in that window. Rollback
   would now be putting `BLOB_DIR` back, which strands every blob written to
   the bucket since the cutover, so it's no longer a free undo.
6. **Left.** Remove the old `/futo-notes-hosted-staging-data/blobs` folders on
   both hosts by hand.

The S3 node is a single point of failure, and its volume has no backup, unlike
Patroni. That is acceptable for an internal alpha, not for paying users. Before
launch, blobs need backups independent of Postgres's. Garage has no S3 bucket
versioning; we don't rely on it, since keys are never overwritten and the
ledger keeps superseded blobs for 365 days. Don't put live blobs on `bigminio`
(`manifest-inventory/config-inv.yml:471`): it's the Postgres backup target, and
it runs MinIO.

## Open questions

- ~~Can manifest-staging2 reach port 3900 on `100.76.177.70` over
  Tailscale?~~ Yes: Manifest's nftables rules accept all input on
  `tailscale0` (`sm_utils.py` `firewall_change`), and staging2 has served
  blobs from the bucket since the cutover.
- **The third host for production Garage.** Does it come from the
  dedicated-host item already on the pre-launch list?

## Not in scope

Presigned URLs, bucket listing and orphan adoption, migrating existing
filesystem blobs into S3, per-host rate limiting, and the move to Yucca's
Kubernetes.
