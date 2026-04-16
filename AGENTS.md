# Stonefruit Server

Generic E2EE sync server. Stores opaque encrypted blobs and metadata — the server never sees plaintext. First client is a notes app, but the server is client-agnostic.

## Commands

```bash
pnpm dev              # Dev server with hot reload (tsx watch)
pnpm build            # esbuild bundle → dist/index.js
pnpm test             # Integration tests (needs running Postgres)
pnpm migrate          # Run DB migrations
docker compose up -d  # Local Postgres on port 5433
tsc --noEmit          # Type-check only
```

## Environment

Copy `.env.example` → `.env`. Only `DATABASE_URL` is required. Use `AUTH_MODE=dev` for development (enables passwordless login at `/api/auth/dev/login`).

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
- Tests call `app.fetch()` directly (no HTTP server started).
- Requires `AUTH_MODE=dev` and a valid `DATABASE_URL`.
- Prefer running individual tests over the full suite for speed.

## Migrations

Sequential files in `src/db/migrations/` (001_, 002_, ...). Each exports `up()` and `down()`.

IMPORTANT: When adding a migration, you MUST also register it in `src/db/migration-registry.ts`. The production bundle cannot discover migrations from the filesystem.

## Monorepo

pnpm workspace. The self-hosting CLI is a separate package at `packages/cli/` (`@futo-notes/cli`).

## CI/CD

GitLab CI. Pipeline: type-check → test (against Postgres service) → build bundle → Docker image → push. Git tags trigger CLI releases to GitLab package registry.

## Design document

See @DESIGN.md for the full architecture, threat model, sync protocol, and scaling plan.
