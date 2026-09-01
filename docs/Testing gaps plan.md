# Closing the testing gaps

Plan for the four gaps found when auditing the test setup (2026-08-25).
Nothing here is client-visible; the compare harness in `test:adoption` is the
wire-contract judge for the one refactor this plan makes. The four gaps, in
order of importance:

1. The Postgres-backed integration tests never run in CI — `test:go` sets none
   of the `*_TEST_DATABASE_URL` variables, so all four files skip silently.
2. There is no in-process end-to-end test: the mux is assembled inline in
   `main()`, and `requireAuth` — the middleware guarding every `/api/*` route —
   plus the `rateLimited` wrapper have zero direct tests. Every handler test
   injects a session into the context, bypassing them. The compare harness
   covers this today, but it needs the TS repo, bun, and the Rust client, and
   it retires with the TS server after cutover.
3. CI never runs the race detector, on a codebase with an SSE hub, a
   LISTEN goroutine, and a jobs runner.
4. The integration env-var convention is undocumented — it appears only in
   test source and one aside in `docs/Recurring jobs plan.md`.

Verified before writing this plan: the full suite passes locally with
`go test -race -count=1 ./...`, and all four integration packages pass in
about 10 seconds against a scratch database on the compose Postgres.

## Step 1 — extract `routes()` and test the middleware composition (gap 2)

### The refactor

Move the mux assembly out of `main()` into a new function in
`cmd/server/main.go` (or a new `routes.go` if main gets cramped):

```go
func routes(cfg config.Config, database *sql.DB, blobStore *blobs.Store, eventHub *events.Hub) http.Handler
```

It builds exactly what `main()` builds today — the `api` mux with the
auth-mode-conditional login routes, the outer mux with capability/health/dev
UI, `requireAuth` around `/api/`, and returns `recoverPanic(mux)`. `main()`
keeps config loading, `db.Open`, `Migrate`, the SSE listener, and the jobs
runner, then serves `routes(...)`. The `auth.NewRateLimiter()` moves inside
`routes()` with the handler that uses it.

This is a pure move. No route, wrapper, or ordering changes — the compare
harness in CI is the check that the wire behavior is untouched.

### The test

New `cmd/server/routes_test.go`, gated the same way as the other integration
tests: skip unless `SERVER_TEST_DATABASE_URL` is set (requireAuth calls
`auth.ValidateSession` against a real database; mocking it would mean
inventing an interface this codebase deliberately doesn't have). The test
opens the database, runs `db.Migrate`, builds a `config.Config` literal with
`AuthMode: "dev"` and `DevUI: true`, and starts
`httptest.NewServer(routes(...))`.

What it checks is the composition the unit tests bypass — not handler logic,
which stays where it is:

- `GET /` and `GET /health` answer without credentials (mounted outside
  `requireAuth`).
- `GET /api/auth` with no credentials → 401 `{"error":"unauthorized"}`.
- `GET /api/auth` with a garbage bearer token → 401 with the
  `invalid_session` body and the `WWW-Authenticate` header (the shape clients
  key re-login on).
- `POST /api/auth/dev/login` works unauthenticated (the exemption in
  `requireAuth`), returns a session cookie and token; the cookie and the
  bearer token each then pass `GET /api/auth`.
- `POST /api/auth/logout` → 204, and the same token afterwards → 401
  `invalid_session` (session actually deleted, not just cookie cleared).
- `POST /dev/panic` → 500 `{"error":"internal server error"}` with
  `Connection: close` — proves `recoverPanic` wraps the outer mux, not just
  the handler it's unit-tested with.

A second, separate server instance with `AuthMode: "password"` covers the
`rateLimited` wiring: repeated wrong-password logins from one connection until
a 429 with a `Retry-After` header arrives (the limiter's window and limit are
already unit-tested in `internal/auth/ratelimit_test.go`; this only proves the
wrapper is actually mounted).

Details that matter:

- Dev login is the fixture: it upserts its own user, so the test seeds no
  rows and needs no cleanup beyond what UUID-fresh emails give the other
  integration tests.
- The test client must not follow redirects or share cookies implicitly —
  pass tokens/cookies explicitly per request so each assertion states its own
  credentials.
- Dev-mode exemption means the password-login exemption arm of `requireAuth`
  is only reachable in the second (password-mode) instance — both arms end up
  covered.

## Step 2 — run the integration tests, with `-race`, in CI (gaps 1 and 3)

One edit to `test:go` in `.gitlab-ci.yml`:

```yaml
test:go:
  extends: .skip-soak-schedule
  stage: test
  image: golang:1.27-bookworm
  services:
    - name: postgres:16
      alias: postgres
  variables:
    POSTGRES_USER: futo_notes
    POSTGRES_PASSWORD: ci
    POSTGRES_DB: futo_notes
    OBJECTS_TEST_DATABASE_URL: postgres://futo_notes:ci@postgres:5432/objects_test
    EVENTS_TEST_DATABASE_URL: postgres://futo_notes:ci@postgres:5432/events_test
    JOBS_TEST_DATABASE_URL: postgres://futo_notes:ci@postgres:5432/jobs_test
    BLOBS_TEST_DATABASE_URL: postgres://futo_notes:ci@postgres:5432/blobs_test
    SERVER_TEST_DATABASE_URL: postgres://futo_notes:ci@postgres:5432/server_test
  before_script:
    - apt-get update && apt-get install -y --no-install-recommends postgresql-client
    - for db in objects_test events_test jobs_test blobs_test server_test; do
        PGPASSWORD=ci createdb -h postgres -U futo_notes "$db";
      done
  script:
    - go test -race ./...
    - go vet ./...
```

Details that matter:

- **One database per package**, matching the per-package env vars. Concurrent
  `Migrate` calls would be safe on a shared database (the advisory lock in
  `internal/db/migrate.go` exists for exactly that), but the tests themselves
  weren't written for cross-package data sharing — the jobs GC and reaper
  tests operate on global table state. Separate databases keep Go's default
  package parallelism without auditing every test for interference. The local
  verification run used one shared database with `-p 1`; per-package
  databases are the same isolation without the serialization.
- `-race` folds gap 3 into the same line. The bookworm image ships gcc, which
  the race detector needs. Measured locally: about 2 seconds per package —
  noise next to the adoption stage.
- `go vet` stays.
- If a `*_TEST_DATABASE_URL` variable is ever misspelled the tests silently
  skip again, so after the CI change, confirm the job log shows the
  integration test names running (`TestObjectMutationLifecycle`,
  `TestRecurringJobs`, `TestBlobDeleteLifecycle`,
  `TestListenerTransactionalDeliveryAndReconnect`, and the new routes test) —
  once, by eye, when the pipeline first goes green.

## Step 3 — document how to run the tests (gap 4)

A short "Testing" section in `README.md`, written after steps 1–2 so it
describes the final state:

- `go test ./...` — unit tests, no dependencies, DB-backed tests skip.
- The five env vars (`OBJECTS_`, `EVENTS_`, `JOBS_`, `BLOBS_`,
  `SERVER_TEST_DATABASE_URL`), each pointing at a scratch database the tests
  may freely write to — never the dev database. Include the copy-paste local
  recipe against the compose Postgres on port 5433:

  ```sh
  docker exec futo-notes-postgres createdb -U futo_notes notes_test
  URL='postgres://futo_notes:futo_notes@localhost:5433/notes_test'
  OBJECTS_TEST_DATABASE_URL=$URL EVENTS_TEST_DATABASE_URL=$URL \
  JOBS_TEST_DATABASE_URL=$URL BLOBS_TEST_DATABASE_URL=$URL \
  SERVER_TEST_DATABASE_URL=$URL go test -race -count=1 -p 1 ./...
  ```

  (`-p 1` because a single shared scratch database is the convenient local
  shape; CI uses one database per package instead.)
- One line each: `-race` is what CI runs; fuzz targets exist
  (`go test -fuzz=FuzzStreamBlobBatch ./cmd/server`); the compare harness and
  adoption rehearsal are CI-only and documented in the migration plan.

## Sequencing and verification

1. **Step 1** lands first, alone: refactor + routes test. Green when
   `go test ./...` still passes with no env vars (new test skips), the routes
   test passes locally against a scratch database, and the CI adoption stage
   passes (wire behavior unchanged).
2. **Step 2** lands second: the CI edit. Green when the `test:go` log shows
   the five integration tests actually ran.
3. **Step 3** lands last: README, describing what now exists.

Out of scope, deliberately: a coverage gate (13.9-style profiling stays a
local tool — the numbers are misleading without the env vars set), an
assertion-helper package (plain `if got != want` is the house style), and any
interface layer over `*sql.DB` for mocking (integration tests against real
Postgres are the established convention).
