# Hosted service plan

FUTO Notes as a managed, multi-tenant service: an **API service** in one region and a
**storage service** in each of three regions. The self-hosted single binary (`cmd/server`) stays as
it is, and its client contract is unchanged.

Engineering conventions follow `~/src/backups`: chi, zerolog, env config, OTLP metrics, alpine
images, a library chart plus app charts, and mise. Where this plan is silent, do what backups does.

Operators: see [Hosted deployment](Hosted%20deployment.md). Local development:
[DEVELOPMENT.md](DEVELOPMENT.md#hosted-development).

## Decisions

| Topic | Decision |
|---|---|
| Topology | API: 3 replicas in one region on Kubernetes, backed by CloudNativePG. Storage: one deployment per region (3 regions), one S3 bucket per region, one prefix per user. |
| Blob bytes | Clients send bytes **directly** to their region's storage service. The API never touches blob bytes. |
| API surface | Stays under `/api` with the same sync contract. The endpoints that carry blob bytes are removed on hosted. New endpoints are listed below. |
| Region | Each user picks a region once, at onboarding. It is fixed in v1. |
| Login | OIDC (Authorization Code + PKCE) against Zitadel. Native apps sign in through the API, which hands them an opaque session token. |
| Storage auth | At login, each device gets a session token and a **storage refresh token** bound to that session. The device exchanges the refresh token at the API for a **24h access JWT** and uses it for any number of storage requests. Revoking the session revokes both. |
| Blob proof | Storage returns a **receipt signed by its region** with each upload. The device passes the receipt to the API when an object claims the blob. The API checks it offline. |
| JWT keys | Ed25519. Keys are listed by `kid` in static config (Secret/ConfigMap), so rotation needs no downtime. Storage never calls the API. |
| Account deletion | A 30-day grace period during which the user can cancel. After that, everything is purged. |
| Quotas, billing | Out of scope for v1. |
| Logging / HTTP | zerolog and chi across the **whole repo**, the self-hosted server included. |
| Metrics | OTLP push (OTel SDK, `otlpmetrichttp`) to VictoriaMetrics, as in backups. |
| Tooling | mise tasks, compose for local dev, and `k8s:validate` in CI. No k3d/Tilt/Flux tree. |
| Charts | Exactly backups' set: a library chart, `api` and `storage` app charts (Deployment, Service, PDB, Secret, plus a migration hook Job for the API), and a `cnpg-cluster` platform chart. Ingress, HPA and NetworkPolicy live in the infra repo. |
| Mock OIDC | A small Go issuer in this repo (`cmd/mock-oidc`), used both in compose and in-process by tests. |
| API docs | An OpenAPI spec for the hosted API and one for the storage service. Tests check responses against the specs. |

## Architecture

```
                         ┌──────────── API region ────────────┐
 app / browser ──HTTPS──▶│ api ×3 (chi, htmx dashboard, SSE)  │──▶ CNPG Postgres
       │                 │  OIDC ⇄ Zitadel                     │
       │                 │  jobs (advisory lock, once/interval)│──admin JWT──┐
       │                 └─────────────────────────────────────┘             │
       │  access JWT (aud = region)                                          ▼
       └────────────────────────────▶ storage (us-east) ──▶ S3 bucket us-east/<user>/<blob>
                                      storage (eu-...)  ──▶ S3 bucket ...
                                      storage (ap-...)  ──▶ S3 bucket ...
```

Each storage service is stateless. It verifies API-signed JWTs with static public keys, signs
receipts with its own key, and talks only to S3.

## API service (`cmd/api`)

### Endpoints

The sync contract carries over unchanged: capability, auth, collections, keys, objects, mutation
IDs, create-mutation recovery and SSE. Responses keep the same bytes.

Removed on hosted (bytes go to storage instead): `POST /api/blobs`, `GET|DELETE
/api/blobs/{userId}/{blobId}`, `POST /api/blobs/batch`, `POST
/api/collections/{id}/blob-objects`, `PUT .../blob-objects/{objectId}` and `POST
.../blob-objects/batch`.

Changed: object create and update take `"receipt": "<jwt>"` in place of a staged `blob_key`. The
receipt names the key and the size. `size_bytes` in responses comes from the receipt.

Changed: vault key material (`GET|PUT /api/collections/{id}/key`) also carries an optional
**recovery envelope**, the vault key wrapped under a recovery key: `recovery_key_salt`,
`recovery_key_kdf` and `recovery_encrypted_vault_key`. They come all three or none (a partial set
is a 400), and a `PUT` replaces both envelopes, so **a `PUT` without them deletes the stored
recovery envelope**. Both envelopes share one `key_updated_at`. Self-hosted neither stores nor
returns them.

Capability (`GET /`, still the JSON document): `auth_mode: "oidc"`, `signup: "open"`, `billing: false`.

New endpoints:

| Method + path | Auth | Purpose |
|---|---|---|
| `GET /api/regions` | public | `[{id, name, description, endpoint}]` |
| `GET /api/auth/oidc/start` | public | Begins login. The dashboard gets a cookie. An app passes `redirect_uri` (a custom scheme or loopback, checked against an allowlist) plus its own PKCE `code_challenge`. |
| `GET /api/auth/oidc/callback` | public | Zitadel redirects here. The API exchanges the code, upserts the user by `sub`, then either sets the dashboard cookie or redirects the app with a one-time code. |
| `POST /api/auth/oidc/token` | public | The app swaps its one-time code and PKCE verifier for `{token, storage_refresh_token, user}`. The token never travels in a URL. |
| `GET /api/account` | session | Profile, region, deletion status, collection and blob counts, stored bytes |
| `PUT /api/account/region` | session | Choose a region. This works once; a second call returns 409. |
| `DELETE /api/account` | session | Schedules deletion in 30 days and revokes every other session |
| `POST /api/account/restore` | session | Cancels a scheduled deletion |
| `GET /api/sessions` | session | Lists devices: created, last used, user agent, which one is current |
| `DELETE /api/sessions/{id}` | session | Revokes a device and its storage refresh token |
| `POST /api/storage/token` | refresh token | Returns `{access_token, expires_at, region: {id, endpoint}}` |
| `POST /api/pairings` | session | A new device opens a pairing with a one-time public key: `201 {id, expires_at}` |
| `POST /api/pairings/{id}/key` | session | An unlocked device posts the vault key sealed to it: `204`, or `409 pairing_already_keyed` on a second post |
| `GET /api/pairings/{id}` | session | The new device polls: `202` until the key is posted, then `200 {ciphertext}` once, deleting the pairing |

The pairing relay is scoped to the caller's account. A pairing lives 5 minutes from creation.
Unknown, expired, collected and other accounts' pairings all get the same `404 not_found`. Bodies
are capped at 8 KiB (`413 payload_too_large`) and each field at 1024 characters (`400`). The
ciphertext is never logged. Pairing is blocked while a deletion is pending, but will not be gated
on a subscription.

While a deletion is pending, sync and storage-token endpoints return `403 {"error":"account
pending deletion"}`, and so does the pairing relay. Only account, session, restore and logout keep
working.

### Dashboard (htmx)

Server-rendered `html/template` pages under `/dashboard`, with a pinned, embedded htmx. Every
action calls the same service code as the API endpoint it matches, so the two can't drift:

- sign in and sign out
- choose a region (on first visit)
- account overview
- devices: list and revoke
- delete account, and cancel a pending deletion

The dashboard uses the session cookie and CSRF tokens on state-changing requests.

### Data

- Postgres only. The existing Postgres migrations run, then a **hosted migration track** that
  self-hosted databases never see:
  - `users.region_id`, `users.deletion_requested_at` and `users.purge_at`
  - the `users.email` unique constraint is dropped: hosted users are keyed by `sub`, and the
    identity provider owns email
  - session device fields (created, last used, user agent)
  - `storage_refresh_tokens` (hash, `session_id` with cascade delete, created and rotated times)
  - OIDC logins in flight and one-time app codes
  - `collection_recovery_envelopes`: each collection's recovery envelope, written in the key's
    transaction
  - `pairings`: open pairings and the sealed keys posted to them
  - `hosted_job_runs` (below)
- Migrations run in an `api migrate` subcommand, as a Helm **pre**-install/pre-upgrade hook Job.
  Backups uses post-*, but here that would deadlock: the API refuses to start while migrations are
  pending, and post-* hooks wait for the new pods to be Ready. Old pods serve during the upgrade,
  so migrations must stay backward compatible for one release.
- Blob ledger on hosted:
  - When an object claims a blob, the API verifies the receipt: region signature, `sub` is the
    caller, the region is the user's, the receipt hasn't expired, and the key isn't already in the
    ledger.
  - The API then inserts the ledger row directly as **claimed**. There is no staged state, because
    the API never sees uploads.
  - retained and purgeable work as they do today. GC keeps a purgeable row until it was claimed
    more than a receipt lifetime (24h) ago: the row is what makes a receipt single-use.

### Background jobs

Every replica runs the scheduler. Each run first takes a transaction-scoped advisory lock for the
job, then skips if the job started on any replica within about its interval (`hosted_job_runs`),
so a job runs at most once per interval across the deployment.

| Job | What it does |
|---|---|
| session reaping | as today |
| pairing sweep | Hourly: deletes pairings past their 5-minute window, so an uncollected sealed key is gone within the hour |
| mutation result expiry | as today |
| blob GC | Deletes expired and purgeable ledger rows, then deletes their keys through the region's internal storage endpoint |
| orphan reconciliation | Lists each region's keys older than 48h (receipts expire after 24h, which leaves a margin) and deletes keys the ledger doesn't know |
| account purge | After the 30-day grace: purges the user's storage prefix, then deletes their rows. Both are idempotent, and the account stays due until both succeed. |

SSE works across replicas through the existing Postgres LISTEN/NOTIFY.

## Storage service (`cmd/storage`)

One deployment per region. Configuration: `REGION_ID`, `S3_*`, the API's public keys (with kid),
and the region's receipt signing key.

| Method + path | Auth | Behavior |
|---|---|---|
| `POST /blobs` | access JWT | Body: ciphertext, up to 100 MiB, `Content-Length` required. The server mints the key `<sub>/<uuidv7>` and writes it with `If-None-Match: *`. Returns `201 {key, size, receipt}`. |
| `GET /blobs/{userId}/{blobId}` | access JWT | `userId` must equal `sub`. Streams the bytes and supports Range. |
| `POST /blobs/batch` | access JWT | Today's frame format and limits |
| `DELETE /internal/blobs` | admin JWT | Deletes a list of keys. Already-gone keys are fine. |
| `GET /internal/blobs?prefix=&older_than=` | admin JWT | Paged key listing, for reconciliation |
| `DELETE /internal/users/{userId}` | admin JWT | Deletes a user's whole prefix |
| `GET /readyz` | none | Readiness and drain, as in backups |

JWTs:

| Token | Signed by | Claims | Lifetime |
|---|---|---|---|
| access | API | `iss` = API, `aud` = region id, `sub` = user id, `scope` = `blobs` | 24h |
| admin | API | `aud` = region id, `scope` = `admin` | 5 min |
| receipt | storage region | `iss` = region id, `aud` = `notes-api`, `sub`, `key`, `size` | 24h |

Clients are expected to refresh the access token about once a day. A revoked session's access
token keeps working until it expires (at most 24h). That's the accepted trade-off for keeping
storage stateless.

## Observability

- zerolog JSON to stderr, with hlog request logging and request IDs.
- Errors use the `{statusCode, message, requestId}` shape on new hosted-only endpoints. The sync
  contract keeps its existing error bodies.
- OTLP metrics:
  - HTTP duration, count and errors by route
  - DB pool
  - job runs and outcomes
  - SSE streams
  - token issuance and verification failures
  - storage bytes in and out
  - S3 operation latency and errors
  - receipt verification failures

## Repo and tooling

- Packages:
  - `cmd/api`, `cmd/storage`, `cmd/mock-oidc` (dev only)
  - hosted logic under `internal/hosted/...` and `internal/storagesvc/...`
  - shared sync logic stays in `internal/{objects,collections,events,jobs,...}`
- Whole repo moves to zerolog and chi first, with the self-hosted contract gate (188/188 steps)
  green.
- mise tasks: `dev`, `test`, `test:integration`, `lint`, `build`, `k8s:validate`,
  `charts:package`.
- compose stack:
  - Postgres, MinIO (one bucket per region) and victoria-metrics
  - the mock OIDC issuer
  - the api and three storage instances (`us-east`, `eu-central`, `ap-southeast`)
- Charts: `charts/lib/notes-common`, `charts/apps/{api,storage}` and
  `charts/platform/cnpg-cluster`, each with dev defaults in `values.yaml` and why-comments.
- GitLab CI:
  - lint, unit, integration (Postgres and MinIO services)
  - `k8s:validate`
  - image build
  - image and OCI chart publish to the GitLab registry
- Specs: `docs/openapi-hosted.yaml` and `docs/openapi-storage.yaml`, with conformance tests.

## Out of scope (v1)

Quotas, billing, region migration, batch uploads, and the Notes client changes. Hosted needs these
client changes, tracked separately:

- OIDC login through the API
- storage token refresh
- direct upload and download
- passing the receipt on create/update
- unlocking the E2EE vault key under SSO: passphrase, recovery key and QR pairing (decided; the
  server half, the recovery envelope and the pairing relay, is built: `docs/Hosted launch plan.md`
  B1 and B2)

## Confirmed defaults

- Hosted sessions, and the storage refresh tokens bound to them, last **90 days, sliding**: every
  use extends them. Self-hosted stays at a fixed 7 days.
- The storage refresh token is opaque and **rotates on every exchange**, with reuse detection.
  Its successor is derived from it (`HMAC-SHA256(STORAGE_REFRESH_ROTATION_KEY, token)`), so
  presenting a token again returns the same successor until that successor is exchanged. Presenting
  it after that revokes the whole session.
- While a deletion is pending, only account, restore, session and logout endpoints work.

## Build order

Each step lands on its own with tests, lint and the self-hosted contract gate green.

1. zerolog + chi across the repo
2. Tooling: mise, compose, mock OIDC, chart skeletons, GitLab CI
3. Storage service, with its chart and MinIO integration tests
4. API core: OIDC (dashboard and app), sessions, regions, storage tokens, receipts in object
   mutations, hosted migrations
5. Jobs: GC, reconciliation and account purge through storage, with advisory-lock leadership
6. Dashboard and the parity endpoints
7. OpenAPI specs and conformance tests, then an end-to-end scenario (`e2e/hosted`, real
   `api`, `storage` ×3 and `mock-oidc` processes against Postgres and MinIO): log in, pick a
   region, get a token, upload, claim, sync to a second device, delete, GC
