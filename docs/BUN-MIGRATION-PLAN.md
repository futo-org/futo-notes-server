# Node → Bun migration plan

Status: in progress on branch `bun-migration` (worktree). `main` stays on Node as the
pristine reference for behavioral comparison.

## Goal & constraint

Move the entire toolchain to **Bun** — package manager, runtime, test runner, and the
server process. **No Node anywhere.** The hard requirement is that the running server
behaves identically to the Node version for clients (same status codes, headers, bodies,
sync/SSE/auth/conflict semantics, blob bytes). Pinned Bun version: **1.3.14**.

## What goes away, what replaces it

| Removed | Replaced by |
|---|---|
| pnpm + `pnpm-lock.yaml` + `pnpm-workspace.yaml` | `bun install` + `bun.lock` |
| `tsx` (dev + test loader) | Bun runs `.ts` natively (`bun --watch`) |
| `@hono/node-server` | `Bun.serve` |
| `dotenv` + `import 'dotenv/config'` | Bun's built-in `.env` loading |
| `node --test` | `bun test` |
| `node:24-slim` image, corepack | `oven/bun:1.3.14-slim` image |

**Kept on purpose** (swapping these would change behavior):
- `pg` + Kysely — `Bun.sql` has no Kysely dialect.
- `node:crypto` scrypt — `Bun.password` can't read the existing hash format; existing
  `FUTO_NOTES_PASSWORD_HASH` values must keep verifying.
- **esbuild for the production bundle**, run via `bun build.mjs` — esbuild's tree-shaking is
  what guarantees the OSS image contains zero hosted code. One-word change (`node`→`bun`),
  needs no Node.

## File changes

### `package.json`
- Drop deps: `@hono/node-server`, `dotenv`, `tsx`.
- Add `"trustedDependencies": ["esbuild"]` (so esbuild's binary postinstall runs under Bun).
- Add devDep `bun-types`.
- Scripts:
  - `dev`: `bun --watch src/index.ts`
  - `start`: `bun src/index.ts`
  - `build`: `bun build.mjs`
  - `build:hosted`: `bun build.mjs --hosted`
  - `migrate`: `bun src/db/migrate.ts`
  - `typecheck`: `tsc --noEmit`
  - `test`: `bun run test:dev && bun run test:limit && bun run test:password`
  - `test:dev`: `AUTH_MODE=dev bun test tests/e2ee-routes.test.ts tests/capability.test.ts tests/sync-routes.test.ts tests/isolation.test.ts`
  - `test:limit`: `AUTH_MODE=dev bun test tests/blob-limit.test.ts`
  - `test:password`: `AUTH_MODE=password bun test tests/auth-password.test.ts`

Three test invocations (not two): `bun test` runs listed files in one process, but
`blob-limit` and `auth-password` set env vars before importing `env.ts`, which snapshots
env at module load. Isolating them per-invocation preserves today's per-file env behavior.

### `tsconfig.json`
- `"types": ["node", "bun"]` (still using `node:` builtins; add Bun globals for `Bun.serve`).

### `src/server.ts`
```ts
// remove: import { serve } from '@hono/node-server'
const server = Bun.serve({ fetch: app.fetch, port: env.PORT, idleTimeout: 0 })
log.info(`${label} listening on http://localhost:${server.port}`)
```
`idleTimeout: 0` is mandatory — Bun's default idle timeout would kill the SSE stream whose
heartbeat is only every 25s. In `shutdown()`, `server.close(cb)` → `await server.stop()`
(drains in-flight streams), kept after `notifier.stop()`.

### `src/index.ts` & `src/hosted/index.ts`
- Replace `if (import.meta.url === \`file://${process.argv[1]}\`)` with `if (import.meta.main)`.

### `src/env.ts`
- Delete `import 'dotenv/config'` (line 1). Bun auto-loads `.env`.
- Guardrails to match dotenv exactly: do not add `.env.local` / `.env.production` /
  `.env.development` files (Bun would auto-load them); escape literal `$` in `.env` values
  (Bun expands `$VAR`).

### `build.mjs`
- Drop `'dotenv'` from `external` (no longer a dep). Everything else unchanged.

### `tests/*.ts`
- `import { test, before, after } from 'node:test'` → `import { test, beforeAll, afterAll } from 'bun:test'`; rename `before`→`beforeAll`, `after`→`afterAll`.
- **Keep all `node:assert/strict` assertions** (they work inside `bun:test`).
- `auth-password.test.ts`: the `spawnSync('node', ['--import','tsx','-e', ...])` subprocess
  becomes `spawnSync('bun', ['--no-env-file','-e', ...])` (the `--no-env-file` keeps the
  "rejects without a hash" assertion deterministic regardless of any `.env`).

### `Dockerfile`
- `oven/bun:1.3.14-slim` for both stages; `bun install --frozen-lockfile` /
  `bun install --production --frozen-lockfile`; `USER bun` + `chown bun:bun` (image ships a
  `bun` user at uid 1000); HEALTHCHECK `bun -e "..."`; `CMD ["bun", "dist/index.js"]`.

### `.gitlab-ci.yml`
- Base image `oven/bun:1.3.14-slim`, empty `before_script`, cache `$CI_PROJECT_DIR/.bun-cache`
  (via `BUN_INSTALL_CACHE_DIR`) keyed on `bun.lock`.
- `bun install --frozen-lockfile`, `bunx tsc --noEmit`, `bun run test`.
- `test:build`: OSS-imports-hosted grep (unchanged), `bun scripts/check-migration-registry.mjs`,
  `bun run build`, `bun run build:hosted`.
- `test:docker` / `build:image` dind jobs unchanged.

### Docs
- `AGENTS.md` / `README.md`: bun commands; flip the testing note from "node:test, no
  Jest/Vitest" to "bun:test (built into Bun)".

Migrations need no change: dev/`bun run src/db/migrate.ts` loads from disk;
the bundle still falls back to the static registry; the registry check runs under Bun.

## Verification (must hold before merge)

1. **scrypt** — an existing `FUTO_NOTES_PASSWORD_HASH` still verifies under Bun (golden vector).
2. **pg LISTEN/NOTIFY + SSE** — `bun run test` passes (sync-routes exercises the real
   `pg.Client` listener + `streamSSE`); plus a live smoke test: hold a `curl` on
   `/api/sync/events`, POST a mutation, confirm the `change` event arrives and the stream
   survives past the 25s heartbeat.
3. **Boot/CLI** — `bun dist/index.js` starts; `bun dist/index.js hash x` prints a hash and exits.
4. **Client equivalence** — drive identical request sequences against the Node bundle
   (`node dist/index.js`, from `main`) and the Bun bundle (`bun dist/index.js`, this branch);
   diff normalized responses (ignore UUIDs/tokens/timestamps, compare status/headers/bodies/
   version numbers/blob bytes). Must match.
