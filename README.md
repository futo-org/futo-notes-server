# FUTO Notes server (Go)

Self-hosted encrypted sync for FUTO Notes. This implementation is wire- and
storage-compatible with the previous TypeScript server.

## Self-host with Docker

Copy `docker-compose.production.yml`, create `.env`, and start the stack:

```dotenv
POSTGRES_PASSWORD=replace-with-a-long-random-value
FUTO_NOTES_PASSWORD=replace-with-your-sync-password
FUTO_NOTES_DATA_DIR=/absolute/path/to/futo-notes-data
```

```bash
docker compose -f docker-compose.production.yml up -d
curl --fail http://localhost:3005/health
```

The image is `futotech/notes-server:stable` for both `linux/amd64` and
`linux/arm64`. While the Go rewrite is under test, every `go-rewrite` build also
publishes `futotech/notes-server:go-candidate` (moving) alongside
`futotech/notes-server:candidate-<short-sha>` (immutable) — pull the pinned tag
for anything you need to reproduce. PostgreSQL metadata lives under
`$FUTO_NOTES_DATA_DIR/postgres`; encrypted blobs live under
`$FUTO_NOTES_DATA_DIR/blobs`.

Existing TypeScript installations must follow
[the upgrade and rollback guide](docs/UPGRADING_FROM_TYPESCRIPT.md) before
changing images.

## Run a release binary

Set at least `DATABASE_URL`, one password variable, and an absolute `BLOB_DIR`:

```bash
DATABASE_URL='postgres://futo_notes:password@127.0.0.1/futo_notes' \
FUTO_NOTES_PASSWORD='sync password' \
BLOB_DIR='/srv/futo-notes/blobs' \
PORT=3005 \
./futo-notes-server
```

Release tags produce Linux amd64/arm64, macOS amd64/arm64, and Windows amd64
binaries plus SHA-256 checksums. `scripts/build-release.sh <version>` produces
the same files locally.

## Development and launch gates

Start only PostgreSQL for local development (port 5433):

```bash
docker compose up -d postgres
GOTOOLCHAIN=auto go test ./...
GOTOOLCHAIN=auto go run ./cmd/server
```

Run the compatibility gates before release:

```bash
GOTOOLCHAIN=auto go run ./cmd/compare -mode all
./scripts/rust-acceptance.sh ts
./scripts/rust-acceptance.sh go
./scripts/adoption-rehearsal.sh all
./scripts/compose-rehearsal.sh
```

The adoption rehearsal requires local TypeScript-server and FUTO Notes client
checkouts; override their locations with `FUTO_TS_SERVER_REPO` and
`FUTO_NOTES_CLIENT_REPO`.

`compose-rehearsal.sh` repeats the swap and rollback at the container level: it
builds both images, runs `docker-compose.production.yml` in a throwaway project on
host port 3205, and swaps by re-tagging the one image tag the Compose file names.
It needs Docker on this machine — a remote or docker-in-docker daemon cannot share
the bind mount it inspects — so it stays out of CI and runs before a release.

During a staging soak, audit the immutable candidate and its containers with:

```bash
CANDIDATE_TAG=candidate-abcdef12
SOAK_START=2026-08-21T20:15:00Z
./scripts/staging-soak-check.sh "$CANDIDATE_TAG" "$SOAK_START"
```

For the release decision, also set `FUTO_SOAK_MIN_DAYS=7` (or 14 for the full
target) and `FUTO_SOAK_REQUIRE_JOBS=true`.
