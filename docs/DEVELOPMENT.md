# Development

Maintainer notes for working on the server itself: tests, the local dev server,
the pre-release compatibility gates, and the candidate image tags. If you only
want to run the server, see the [README](../README.md).

## Testing

`go.mod` requires Go 1.27. On an older local toolchain every `go` command in
this file needs `GOTOOLCHAIN=auto` in front of it, or it stops with
`go.mod requires go >= 1.27.0` and does nothing. CI sets it globally.

`go test ./...` runs the unit tests and the SQLite application lifecycle test
without external dependencies. Postgres-backed tests also run when
`OBJECTS_TEST_DATABASE_URL`, `EVENTS_TEST_DATABASE_URL`,
`JOBS_TEST_DATABASE_URL`, `BLOBS_TEST_DATABASE_URL`,
`USAGE_TEST_DATABASE_URL`, `SUBSCRIPTIONS_TEST_DATABASE_URL`, and
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
USAGE_TEST_DATABASE_URL=$URL SUBSCRIPTIONS_TEST_DATABASE_URL=$URL \
SERVER_TEST_DATABASE_URL=$URL MIGRATION_TEST_DATABASE_URL=$URL \
GOTOOLCHAIN=auto go test -race -count=1 -p 1 ./...
```

CI runs the Go suite with `-race`. Fuzz targets are also available, for example
`go test -fuzz=FuzzStreamBlobBatch ./cmd/server`.

## Development and launch gates

The default developer server uses SQLite and needs no database setup:

```bash
AUTH_MODE=dev GOTOOLCHAIN=auto go run ./cmd/server
```

Hosted mode needs the four `OIDC_*` variables, the four required `POLAR_*`
ones, and, during the internal MVP, the sign-up gate and the discount that
makes the checkout free:

```bash
AUTH_MODE=oidc DEV_UI=true COOKIE_SECURE=false \
  OIDC_ISSUER=https://<instance>.zitadel.cloud \
  OIDC_CLIENT_ID=... OIDC_CLIENT_SECRET=... \
  OIDC_REDIRECT_URI=http://localhost:3005/api/auth/oidc/callback \
  POLAR_BASE_URL=https://sandbox-api.polar.sh \
  POLAR_ACCESS_TOKEN=polar_oat_... POLAR_PRODUCT_ID=... \
  POLAR_WEBHOOK_SECRET=whsec_... \
  POLAR_DISCOUNT_ID=... \
  HOSTED_ALLOWED_EMAIL_DOMAINS=futo.org \
  GOTOOLCHAIN=auto go run ./cmd/server
```

`OIDC_REDIRECT_URI`'s path has to end in `/api/auth/oidc/callback`; the server
refuses to boot otherwise, because the Login Hand-off login URL is built from
that route's `start` sibling. Any missing `OIDC_*` or `POLAR_*` value fails the
boot with one error naming every one of them, so an operator fixes them in a
single pass. `HOSTED_ALLOWED_EMAIL_DOMAINS` is a comma-separated list compared
case-insensitively — unset it to admit every identity the provider
authenticates.

`POLAR_DISCOUNT_ID` is the one optional Polar setting: while it is set, every
checkout is created with that discount applied. Point it at a **100%
percentage discount whose duration is Forever** — anything shorter starts
billing when it lapses. Dropping the variable is the launch flip.

The `/dev` page's "Start hand-off" card drives the whole login end to end. Its
subscription card signs a Polar-shaped delivery and puts it through the real
webhook route, so entitlement can be exercised without a sandbox subscription;
its checkout card calls the real `POLAR_BASE_URL`, so point that at the sandbox
before pressing anything there.

To walk the whole hosted path without any credentials at all, run the hosted
rehearsal. It builds the server from the working tree and boots it from the
deployed environment contract against a scratch Postgres, with local stand-ins
for Zitadel and Polar, and checks the sequence a person actually goes through
— hand-off login, the email-domain allowlist, a 402 write, a checkout carrying
the discount, a signed webhook delivery, an accepted write, usage, the quota,
and the lapse:

```bash
docker compose up -d postgres
docker exec futo-notes-postgres createdb -U futo_notes hosted_rehearsal
HOSTED_REHEARSAL_DATABASE_URL='postgres://futo_notes:futo_notes@localhost:5433/hosted_rehearsal' \
  GOTOOLCHAIN=auto go run ./cmd/hostedrehearsal
```

It drops and recreates the scratch database's public schema on every run, so
never point it at anything worth keeping. Deploying the hosted service, and
the full list of settings it reads, are in [Hosted deployment](./Hosted%20deployment.md).

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

The hosted path has its own rehearsal, `go run ./cmd/hostedrehearsal`, which
wants a scratch Postgres rather than the checkouts these want. It is described
under hosted mode above.

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

## Image tags

The released image is `futotech/notes-server:stable` for both `linux/amd64` and
`linux/arm64`. While the Go rewrite is under test, every `go-rewrite` build also
publishes `futotech/notes-server:go-candidate` (moving) alongside
`futotech/notes-server:candidate-<short-sha>` (immutable).

## Release binaries

Release tags produce Linux amd64/arm64, macOS amd64/arm64, and Windows amd64
binaries plus SHA-256 checksums. `scripts/build-release.sh <version>` produces
the same files locally.
