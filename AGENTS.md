# Stonefruit Server

Generic E2EE sync server. Stores opaque encrypted blobs and metadata — the server never sees plaintext. First client is a notes app, but the server is client-agnostic.

## Commands

```bash
pnpm dev              # Dev server with hot reload (tsx watch)
pnpm build            # esbuild bundle → dist/index.js (OSS entrypoint)
pnpm build:hosted     # esbuild bundle → dist/hosted.js (hosted entrypoint)
pnpm test             # Integration tests (needs running Postgres)
pnpm migrate          # Run DB migrations
docker compose up -d  # Local Postgres on port 5433
tsc --noEmit          # Type-check only

node dist/index.js hash <password>   # print a scrypt hash for STONEFRUIT_PASSWORD_HASH
```

## Environment

Copy `.env.example` → `.env`. `DATABASE_URL` is always required. Set `AUTH_MODE=dev` for development and tests (enables passwordless login at `/api/auth/dev/login`). Set `AUTH_MODE=password` + `STONEFRUIT_PASSWORD_HASH` for single-user self-hosted mode. Validation happens at boot via `validateEnv()` in `src/env.ts`.

## App structure: OSS vs hosted

- `src/app.ts` exports `buildApp()` — the shared Hono app factory (all sync routes).
- `src/index.ts` is the OSS entrypoint; ships in the public `stonefruit/server` image.
- `src/hosted/index.ts` is the hosted entrypoint; wraps `buildApp()` with hosted-only middleware (billing, etc.). Ships in a separate hosted image from the same commit.
- `src/server.ts` holds the shared lifecycle (`runServer`, `runCliSubcommand`).

**Invariant:** OSS code never imports from `src/hosted/*`. CI enforces this with a grep check in `test:build`. Hosted code can import freely from the rest of `src/`; the reverse must not happen.

## Code style

- ES modules only, never CommonJS
- `.ts` extensions in all import paths (`import { db } from './db/connection.ts'`)
- UUIDs via `uuidv7()` — sortable, not `crypto.randomUUID()`
- Kysely for all queries — no raw SQL except inside `sql` template tags
- Bigints: stored as `string` in Postgres types, parsed to `Number` at the application boundary
- Error responses: `{ error: string }` with appropriate HTTP status

## Architecture invariants

- **User-partitioned data.** Every query MUST include `user_id` from auth context. Never query across users.
- **E2EE-agnostic.** Never inspect, parse, or validate blob contents.
- **Conflict detection, client resolution.** Reject stale writes with 409 + current state. Never merge server-side.
- **Soft deletes.** Objects get `deleted=true` + version bump. Never hard-delete object rows.
- **Version-guarded writes.** Updates require `version == currentVersion + 1`. Deletes accept optional `?version=N` for edit-vs-delete race protection.

## Testing

- Framework: `node:test` (native). No Jest, no Vitest.
- Tests hit a real Postgres — no mocks for the database.
- Tests call `buildApp().fetch()` directly (no HTTP server started).
- Requires a valid `DATABASE_URL`.
- Two invocations because mode affects module-loaded env: `pnpm test:dev` (AUTH_MODE=dev) and `pnpm test:password` (AUTH_MODE=password). `pnpm test` runs both in sequence.
- Password-mode tests set `STONEFRUIT_PASSWORD_HASH` at the top of the file via top-level await, before dynamic-importing modules that snapshot env.

## Migrations

Sequential files in `src/db/migrations/` (001_, 002_, ...). Each exports `up()` and `down()`.

IMPORTANT: When adding a migration, you MUST also register it in `src/db/migration-registry.ts`. The production bundle cannot discover migrations from the filesystem.

## Self-hosting installer

The `stonefruit` CLI that users install via `curl -sSL … install.sh | sh` is a separate Go module at `installer/` (Cobra + Bubble Tea TUI). It is not part of the pnpm workspace. Build it with `cd installer && go build ./`; run tests with `go test ./...`.

The installer collects an admin password during setup, hashes it by shelling out to `docker run --rm <image> node dist/index.js hash <pw>` (single source of truth for scrypt format), and writes `STONEFRUIT_PASSWORD_HASH=...` to a sibling `.env` file. Docker compose substitutes it via `${STONEFRUIT_PASSWORD_HASH}` in `docker-compose.yml`. The `.env` is `0600`.

## CI/CD

GitLab CI. Pipeline: type-check → test (against Postgres service) → build bundle → Docker image → push. Git tags trigger installer releases to GitLab package registry.

**Image tags:** `build:image` pushes `server:${CI_COMMIT_TAG}` + `server:stable` on tagged releases, and `server:${CI_COMMIT_SHORT_SHA}` + `server:latest` on main branch pushes. Fresh installs default to `:stable` via the installer; `stonefruit release latest` switches to rolling.

**Monitoring pipelines:** `$GITLAB_TOKEN` env var holds a PAT for `gitlab.futo.org`. Before using it (with `glab` or `curl`), verify it's not revoked — tokens get rotated:

```bash
curl -sS --header "PRIVATE-TOKEN: $GITLAB_TOKEN" "https://gitlab.futo.org/api/v4/user" | head -c 200
```

If it returns a user JSON, you can use `glab ci list`, `glab ci view <id>`, `curl .../pipelines`, etc. to monitor runs, tail job logs, and verify that a tag has picked up `release:installer` and published binaries. If the curl returns `invalid_token`/`Token was revoked`, ask the user to rotate the PAT.

## Design document

See @DESIGN.md for the full architecture, threat model, sync protocol, and scaling plan.
