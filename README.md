# FUTO Notes server (Go)

Self-hosted encrypted sync for FUTO Notes. This implementation is wire- and
storage-compatible with the previous TypeScript server.

## Self-host with Docker

Copy `docker-compose.production.yml`, create `.env`, and start the server:

```dotenv
FUTO_NOTES_PASSWORD=replace-with-your-sync-password
FUTO_NOTES_DATA_DIR=/absolute/path/to/futo-notes-data
```

```bash
docker compose -f docker-compose.production.yml up -d
curl --fail http://localhost:3005/health
```

SQLite metadata and encrypted blobs both live under `FUTO_NOTES_DATA_DIR`.
Back up the complete server by stopping it, copying that directory, and
starting it again:

```bash
docker compose -f docker-compose.production.yml stop server
cp -a /absolute/path/to/futo-notes-data /absolute/path/to/futo-notes-data.backup
docker compose -f docker-compose.production.yml start server
```

The image is `futotech/notes-server:stable` for both `linux/amd64` and
`linux/arm64`. While the Go rewrite is under test, every `go-rewrite` build also
publishes `futotech/notes-server:go-candidate` (moving) alongside
`futotech/notes-server:candidate-<short-sha>` (immutable).

Existing TypeScript installations must first follow
[the TypeScript-to-Go upgrade guide](docs/UPGRADING_FROM_TYPESCRIPT.md). They
continue using Postgres. Afterward, they can optionally follow
[Switching from Postgres to SQLite](docs/Switching%20from%20Postgres%20to%20SQLite.md).

## Run a release binary

New installs do not need a database server or `DATABASE_URL`. Set one password
variable and an absolute `BLOB_DIR`; SQLite defaults to `./data/notes.db`:

```bash
FUTO_NOTES_PASSWORD='sync password' \
BLOB_DIR='/srv/futo-notes/blobs' \
PORT=3005 \
./futo-notes-server
```

Run the process from a stable working directory, or explicitly set an absolute
SQLite URL such as `DATABASE_URL=sqlite:/srv/futo-notes/notes.db`.

As a safety check, the server refuses to create a new SQLite file when
`BLOB_DIR` already contains blob files; this usually means an existing install
lost its `DATABASE_URL`. Recover the intended configuration instead of syncing
against an empty vault. `ALLOW_FRESH_DATABASE=true` is available only for an
intentional fresh database beside pre-existing blob files.

Release tags produce Linux amd64/arm64, macOS amd64/arm64, and Windows amd64
binaries plus SHA-256 checksums. `scripts/build-release.sh <version>` produces
the same files locally.

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
```

The adoption rehearsal requires local TypeScript-server and FUTO Notes client
checkouts; override their locations with `FUTO_TS_SERVER_REPO` and
`FUTO_NOTES_CLIENT_REPO`.

During a staging soak, audit the immutable candidate and its containers with:

```bash
CANDIDATE_TAG=candidate-abcdef12
SOAK_START=2026-08-21T20:15:00Z
FUTO_SOAK_ENGINES='postgres sqlite' \
./scripts/staging-soak-check.sh "$CANDIDATE_TAG" "$SOAK_START"
```
