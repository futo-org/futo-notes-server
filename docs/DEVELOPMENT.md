# Development

Maintainer notes for working on the server itself: tests, the local dev server,
the pre-release compatibility gates, and the candidate image tags. If you only
want to run the server, see the [README](../README.md).

## Testing

`go test ./...` runs the unit tests and the SQLite application lifecycle test
without external dependencies. Postgres-backed tests also run when
`OBJECTS_TEST_DATABASE_URL`, `EVENTS_TEST_DATABASE_URL`,
`JOBS_TEST_DATABASE_URL`, `BLOBS_TEST_DATABASE_URL`, and
`SERVER_TEST_DATABASE_URL` are set to scratch databases. The Postgres-to-SQLite
copy test also runs when `MIGRATION_TEST_DATABASE_URL` is set. Every URL must
identify a scratch database that tests may freely write to; never use a
development or production database.

For a convenient local Postgres run on port 5433:

```bash
docker compose up -d postgres
docker exec futo-notes-postgres createdb -U futo_notes notes_test
URL='postgres://futo_notes:futo_notes@localhost:5433/notes_test'
OBJECTS_TEST_DATABASE_URL=$URL EVENTS_TEST_DATABASE_URL=$URL \
JOBS_TEST_DATABASE_URL=$URL BLOBS_TEST_DATABASE_URL=$URL \
SERVER_TEST_DATABASE_URL=$URL MIGRATION_TEST_DATABASE_URL=$URL \
go test -race -count=1 -p 1 ./...
```

CI runs the Go suite with `-race`. Fuzz targets are also available, for example
`go test -fuzz=FuzzStreamBlobBatch ./cmd/server`.

## Development and launch gates

The default developer server uses SQLite and needs no database setup:

```bash
AUTH_MODE=dev GOTOOLCHAIN=auto go run ./cmd/server
```

Postgres remains available for exercising existing-install and hosted paths:

```bash
docker compose up -d postgres
```

Run the compatibility gates before release:

```bash
GOTOOLCHAIN=auto go run ./cmd/compare -mode all
GOTOOLCHAIN=auto go run ./cmd/compare -engine-parity -mode dev
./scripts/rust-acceptance.sh ts
./scripts/rust-acceptance.sh go
./scripts/rust-acceptance.sh go sqlite
./scripts/sqlite-migration-rehearsal.sh
./scripts/adoption-rehearsal.sh all
./scripts/compose-rehearsal.sh
./scripts/sqlite-switch-rehearsal.sh
```

The adoption rehearsal requires local TypeScript-server and FUTO Notes client
checkouts; override their locations with `FUTO_TS_SERVER_REPO` and
`FUTO_NOTES_CLIENT_REPO`.

`sqlite-switch-rehearsal.sh` runs `migrate-to-sqlite.sh` — the one command a
self-hoster uses to move off Postgres — against a Postgres install that
deliberately does not match the shipped compose file, then rolls it back.

During a staging soak, audit the immutable candidate and its containers with:

```bash
CANDIDATE_TAG=candidate-abcdef12
SOAK_START=2026-08-21T20:15:00Z
FUTO_SOAK_ENGINES='postgres sqlite' \
./scripts/staging-soak-check.sh "$CANDIDATE_TAG" "$SOAK_START"
```

## Hosted development

The hosted service (see [Hosted service plan](Hosted%20service%20plan.md);
operators: [Hosted deployment](Hosted%20deployment.md)) uses
[mise](https://mise.jdx.dev) for tools and tasks; `mise tasks` lists them. None
of this touches the self-hosted flows above.

```bash
mise install              # pinned go, golangci-lint, helm, kubeconform
mise run docker:start     # compose.hosted.yml: Postgres, MinIO, victoria-metrics, mock-oidc
mise run test:integration # every suite, against that stack
mise run lint
mise run k8s:validate     # helm template | kubeconform for charts/
mise run docker:stop      # add -v to drop the Postgres volume
```

| Service | Host port | Notes |
|---|---|---|
| Postgres 18 | 15432 | `postgres`/`postgres`, database `notes` |
| MinIO | 9000 (S3), 9001 (console) | `minio`/`miniominio`; buckets `notes-us-east`, `notes-eu-central`, `notes-ap-southeast` |
| victoria-metrics | 8428 | OTLP at `/opentelemetry/v1/metrics` |
| mock-oidc | 8092 | client `notes-dev` / `notes-dev-secret` |
| storage (us-east, eu-central, ap-southeast) | 3015, 3016, 3017 | profile `services` or `mise run storage:dev`; see below |

The mock OIDC issuer (`cmd/mock-oidc`) logs in any subject. In a browser,
`/authorize` shows a sign-in form; scripts can call `/test/authorize` with the
same query plus `sub=` (optionally `email=`, `name=`) to get a code with no UI.
PKCE (S256) is mandatory. Go tests can run it in-process with
`mockoidc.StartTestServer`.

Token keys are Ed25519 PEM blocks with a `kid` header; `mise run keygen -kid
<kid>` prints a new pair. `dev/hosted-keys/` holds **dev-only** pairs for the
API and the three regions; never use them outside local development and tests.
`charts/apps/storage/values.yaml` carries copies of them for `useDevKeys`;
change both together.

### Storage service

`cmd/storage` is one region's blob store (API: `docs/openapi-storage.yaml`).
Run a region on the host against the compose MinIO, with the dev keys:

```bash
mise run storage:dev              # us-east on :3015 (or REGION_ID=...)
mise run storage:dev eu-central   # :3016
mise run storage:dev ap-southeast # :3017
```

Or run all three in containers (same host ports, so not both at once):

```bash
docker compose -f compose.hosted.yml --profile services up -d --build
```

Requests need tokens minted with `dev/hosted-keys/api.key.pem`: get one from
`mise run api:dev` (below) or mint it in Go with `hostedauth.NewMinter`. The storage integration
tests run against MinIO when `S3_TEST_ENDPOINT` is set, which
`mise run test:integration` does; they use bucket `notes-us-east` and a fresh
user prefix per test.

The Helm chart `charts/apps/storage` is installed once per region; a region's
release sets only `region` (names, `REGION_ID` and the `notes-<region>` bucket
derive from it). Its Secret `notes-storage-<region>` must hold
`S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, `API_PUBLIC_KEYS` and
`RECEIPT_SIGNING_KEY`.

### API service

`cmd/api` is the hosted API: the sync contract without the blob-byte routes,
OIDC sign-in, storage tokens, and account, region and device management (API:
`docs/openapi-hosted.yaml`). It runs on the host, not in compose, because the mock issuer's URL is
`http://localhost:8092` and the API must reach it under the same name browsers
do:

```bash
mise run docker:start   # Postgres :15432, mock-oidc :8092
mise run storage:dev    # optional: a region to upload to (:3015)
mise run api:dev        # applies migrations, then serves :3005
```

`api:dev` uses the dev keys, `dev/hosted-regions.json` (the three storage dev
ports) and `DEV_UI=true`: <http://localhost:3005/dev> links the dashboard and drives app
sign-in, regions, devices, deletion, storage tokens and receipt-backed object
writes. App sign-in there needs the page opened as `http://127.0.0.1:3005/dev`,
because only loopback IP literals are allowed app redirect targets.

`api migrate` applies the shared migrations and then the hosted track
(`internal/hosted/hosteddb`, bookkeeping table `hosted_migrations`); `api`
refuses to start while either has migrations pending. In Kubernetes the chart's
pre-install/pre-upgrade hook Job runs it.

Every API replica runs the background jobs (`internal/hosted/hostedjobs`); a
job takes a Postgres advisory lock and is skipped if it started on any replica
within its interval (`hosted_job_runs`). Hourly: session reaping, expired OIDC
logins. Every 6 hours: mutation result expiry, blob GC, orphan reconciliation
(each region's keys older than 48h that the ledger does not know) and account
purge (accounts past the 30-day grace: storage prefix first, then the rows).
The storage jobs call each region's `/internal/*` endpoints at the region's
`endpoint` from the regions file, so the API must be able to reach it.

The end-to-end client flow against a dev stack:

1. `GET /api/auth/oidc/start?redirect_uri=http://127.0.0.1:<port>/cb&code_challenge=<S256>&state=<s>`
   in a browser; after sign-in the API redirects to `redirect_uri` with
   `code` and `state`.
2. `POST /api/auth/oidc/token {"code", "code_verifier"}` returns
   `{token, storage_refresh_token, user}`; `token` is the bearer session.
3. `PUT /api/account/region {"region_id": "us-east"}` (once).
4. `POST /api/storage/token {"refresh_token"}` returns an access token for the
   region, its endpoint, and the next refresh token (the old one is spent).
5. `POST <endpoint>/blobs` with the access token stores ciphertext and returns
   `{key, size, receipt}`.
6. `POST /api/collections/{id}/objects {"receipt"}` (or `PUT .../objects/{objectId}
   {"version", "receipt"}`) claims it.

The API's integration tests use `HOSTED_TEST_DATABASE_URL` (one database shared
by every hosted package; `mise run test:integration` sets it) and run the OIDC
flows against an in-process `mockoidc.StartTestServer`.

With `DEV_UI=true` the API also serves job triggers, which run a storage job at
once on that replica, outside its schedule and lock (the dev page has buttons
for them):

| Trigger | Runs |
|---|---|
| `POST /dev/jobs/blob-gc` | blob GC |
| `POST /dev/jobs/account-purge` | account purge |
| `POST /dev/jobs/reconciliation[?region=&user=&now=]` | orphan reconciliation, optionally in one region, under one user's prefix, and as of a later RFC 3339 `now` (S3 cannot backdate an upload) |

### End-to-end scenario

`e2e/hosted` builds `cmd/mock-oidc`, `cmd/storage` and `cmd/api`, runs them as
subprocesses (three regions on loopback ports, the dev keys, a generated
regions file, `api migrate` first), and drives the product over HTTP as a
native app would. It covers app sign-in with PKCE, regions, storage tokens,
upload and claim, a second device's SSE change, pull and downloads (single and
batch), update, a 409 conflict, Mutation-Id replay, cross-region token
rejection, device revocation, refresh-token reuse, account deletion, restore
and purge, GC after the receipt window, and orphan reconciliation. Timestamps
are shifted with SQL, and jobs run through the dev triggers. It runs when
`HOSTED_E2E_DATABASE_URL` (a scratch database) and `S3_TEST_*` are set, as
`mise run test:integration` and CI do:

```bash
HOSTED_E2E_DATABASE_URL=postgres://postgres:postgres@localhost:15432/hosted_e2e \
S3_TEST_ENDPOINT=http://localhost:9000 S3_TEST_ACCESS_KEY_ID=minio \
S3_TEST_SECRET_ACCESS_KEY=miniominio S3_TEST_REGION=us-east-1 \
go test -count=1 -v ./e2e/hosted
```

On failure it prints the tail of every service's log. It removes its user's
rows and S3 prefixes, and stops every process, when it ends.

### Dashboard

The account dashboard is served by the API at `/dashboard` (`GET /` stays the
JSON capability document). With `mise run docker:start` and `mise run api:dev`
running, open <http://localhost:3005/dashboard>, press **Sign in**, and sign in
on the mock issuer's form with any subject; the callback sets the session
cookie and lands back on `/dashboard`. The dev page links there too.

- First visit with no region: choose one (offered once; it cannot change).
- Overview: name, email, region, usage, and a banner with the purge date and
  **Cancel deletion** while a deletion is pending.
- Devices: every session with user agent and UTC times; **Sign out** on any
  device but this one (this browser signs out from the header instead).
- Delete account: explains the 30-day grace and needs `DELETE` typed to
  confirm; every other device is signed out at once.

Pages are `html/template` under `internal/hosted/api/dashboard/templates`;
htmx 2.0.4 and the stylesheet are embedded from `dashboard/static` (no CDN).
htmx requests (`HX-Request: true`) get the `#page` fragment, other requests the
whole document, and every form also works as a plain post without JavaScript.
Each action calls the same `account.Store` (or `auth.DeleteSession` for sign
out) as its JSON route:

| Dashboard | JSON API |
|---|---|
| `POST /dashboard/region` | `PUT /api/account/region` |
| `POST /dashboard/devices/{id}/revoke` | `DELETE /api/sessions/{id}` |
| `POST /dashboard/delete` | `DELETE /api/account` |
| `POST /dashboard/restore` | `POST /api/account/restore` |
| `POST /dashboard/sign-out` | `POST /api/auth/logout` |

State-changing requests need the session's CSRF token, an HMAC of the session
token, rendered into a `csrf-token` meta tag, `hx-headers` (`X-CSRF-Token`) and
a hidden `csrf_token` field; without it they get 403. Responses carry a strict
CSP (`'self'` only, no inline script or style; htmx is configured through its
`htmx-config` meta tag), `nosniff`, `Referrer-Policy: no-referrer` and
`Cache-Control: no-store`. The session cookie is `HttpOnly`, `SameSite=Lax`,
`Secure` per `COOKIE_SECURE`, and is re-issued with a fresh 90-day `Max-Age`
whenever a cookie-authenticated request slides the session.

## Image tags

The released image is `futotech/notes-server:stable` for both `linux/amd64` and
`linux/arm64`. While the Go rewrite is under test, every `go-rewrite` build also
publishes `futotech/notes-server:go-candidate` (moving) alongside
`futotech/notes-server:candidate-<short-sha>` (immutable).

## Release binaries

Release tags produce Linux amd64/arm64/armv6, macOS amd64/arm64, and Windows
amd64 binaries plus SHA-256 checksums. `scripts/build-release.sh <version>`
produces the same files locally.
