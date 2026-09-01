# Comparison Harness: TS server vs Go rewrite

## Context

The Go rewrite is feature-complete (all spec routes, migrations 001–011, jobs, SSE) and `go test ./...` is green. The migration plan (`docs/Rewriting the server in Go.md:341`) names one remaining gate to launch: a harness that drives identical client traffic at the old TypeScript server and the new Go server and confirms the client-visible behavior matches. Only client-visible behavior must match; internals may differ.

Decisions made with Justin:
- **Two layers**: (1) a differential driver that mirrors identical requests to both servers and diffs responses at every step; (2) the existing Rust client acceptance suite (`crates/futo-notes-sync/tests/server_integration.rs` + `sse_live.rs` in `/home/justin/Developer/futo-notes`) pointed at the Go server.
- **Build `POST /api/auth/dev/login` in Go** (reversing the plan's "probably skip"): the Rust suite, the client repo's CI, and the staging box all run `AUTH_MODE=dev`. It's fully specified in `docs/openapi.yaml:150-181`.


## Key facts from exploration

- **TS server** (`/home/justin/Developer/futo-notes-server`, currently on `feat/dockerhub-multiarch-images`): `src/` and `tests/` are byte-identical to `main` (`git diff main HEAD -- src tests` is empty), so it can run in place. Bun runtime: `bun src/index.ts`. Env-driven, shell exports beat its `.env`. Migrations auto-run at boot. Readiness: `GET /health` → `{"status":"ok","db":"connected"}`. Relevant env: `DATABASE_URL`, `PORT`, `BLOB_DIR` (cwd-relative default!), `AUTH_MODE`, `FUTO_NOTES_PASSWORD`, `COOKIE_SECURE=false`, `BLOB_GC_ENABLED=false`, `LOG_LEVEL`.
- **Go server**: env-only config (`internal/config/config.go`), migrations auto-run at boot with Kysely-compatible bookkeeping. Background jobs start unconditionally, first fire at T+60s — all no-ops on fresh state (nothing expired, nothing untracked, nothing >24h old), so short harness runs are safe.
- **Postgres**: `futo-notes-postgres` container already running on host port 5433 (user/pass/db `futo_notes`). Scratch-DB-per-run is the established pattern there (`futo_notes_xplat_s2`, …).
- **Rust suite**: run from `/home/justin/Developer/futo-notes` with `FUTO_TEST_SERVER=http://127.0.0.1:3055 cargo test -p futo-notes-sync --test server_integration --test sse_live -- --ignored --test-threads=1`. Requires `AUTH_MODE=dev` (raw helpers call `/api/auth/dev/login`). Its `AGENTS.md` demands an isolated server, never the `:3005` demo server. Client CI boots the TS server the same way (`.gitlab-ci.yml:836`).
- **Reference helpers to copy** (unexported, `package main`, can't import): request batch framing `batchFrame()` in `cmd/server/objects_test.go:8`; response frame decoder `decodeFrames()` in `cmd/server/blobs_test.go:57`; SSE event reader `readSSEEvent()` in `cmd/server/sync_test.go:106`.

## Part 0 — Dev login in Go (prerequisite)

Contract from `docs/openapi.yaml:150-181`: mounted only when `AUTH_MODE=dev`; body `{email, name?}`; email trimmed + lowercased; name defaults to the email's local part; upsert user by email; returns the standard LoginSuccess `{user:{id,email,name}, token}` + session cookie; `400 {"error": ...}` on invalid JSON or missing email. No rate limiter (TS only rate-limits password mode).

Changes:
- `internal/auth/user.go`: add `UpsertUserByEmail(ctx, db, email, name)` alongside `UpsertLocalUser`. `sub` is not client-visible and unspecified — use `dev:<email>`.
- `cmd/server/auth.go`: add `handleDevLogin(cfg, database)` mirroring `handlePasswordLogin`'s shape (decode → validate → upsert → `auth.CreateSession` → cookie + JSON). Extend the `requireAuth` passthrough at `auth.go:40`: when `cfg.AuthMode == "dev"`, let `/api/auth/dev/login` through.
- `cmd/server/main.go`: register `POST /api/auth/dev/login` when `cfg.AuthMode == "dev"` (next to the password-mode registration at `main.go:126`).
- `cmd/server/devui.html`: add a dev-login card to the `endpoints` array (line ~125), desc noting it's dev-mode only.
- Tests in `cmd/server` following the existing handler-test patterns: email normalization, name defaulting, 400 cases, route absent in password mode, upsert-not-duplicate.

## Part 1 — Differential driver (`cmd/compare`)

A standalone Go program in this repo: `go run ./cmd/compare`. Not imported by the server; may share nothing with `cmd/server` (copy the frame encoders).

### Orchestration
1. Connect to `postgres://futo_notes:futo_notes@localhost:5433/futo_notes`, `CREATE DATABASE futo_notes_cmp_{ts,go}_<runid>`. Drop on success; keep with `-keep` or on divergence.
2. Two temp blob dirs via `os.MkdirTemp` (absolute — both servers default `BLOB_DIR` cwd-relative).
3. Start TS: `bun src/index.ts`, cwd `/home/justin/Developer/futo-notes-server`, `PORT=3105`, plus env above. Startup guard: warn if `git -C <ts-repo> diff --quiet main -- src` fails (working tree drifted from main).
4. Build and start Go server: `go build -o <tmp>/server ./cmd/server`, `PORT=3005`. The harness refuses to start when either port is occupied instead of killing an unrelated process; the error names the port and the override flags remain available for deliberately isolated runs.
5. Poll both `/health` until 200; run scenarios; report; teardown.

### Mirror + diff engine
- Scenarios are written against **canonical placeholders**; per-server identity maps translate. When a response contains a server-minted value at a known path (user id, collection id, object id, blob key, token), each server's actual value is bound to the placeholder; later requests substitute per-target, later responses normalize back before diffing.
- JSON compare is **type-strict** (decode with `json.Number` so `"3"` ≠ `3` — the string-vs-number rules in the spec header are load-bearing) and key-set-exact (missing/extra fields are divergences).
- Timestamps: assert both parse as RFC3339 within the run window, then normalize to a placeholder.
- Headers compared: status code, `Content-Type`, `Retry-After` presence, `WWW-Authenticate` presence, `Set-Cookie` shape (name + flags, not value).
- Binary bodies (blob GET, `POST /api/blobs/batch` response): decode frames, map keys, compare payload bytes exactly.
- SSE: open `/api/sync/events` on both; at checkpoints compare coalesced final state — the set of (collection placeholder → max `currentVersion` seen) — never event counts (the stream is lossy and coalesced by design).
- On divergence: record structured report (step, path, both raw values), continue, exit nonzero at the end. Steps that later steps depend on (login) abort the scenario.

### Known-divergences allowlist
Seeded from the migration plan's accepted deviations, keyed by step + JSON path, each entry carrying a doc reference:
- Malformed `Authorization` header: Go `401 {"error":"unauthorized"}` vs TS `invalid_session` + `WWW-Authenticate` (plan §How Authentication Works).
- Legacy-syntax Mutation-Id: Go `400` vs TS recorded outcome (plan §Mutation ID format).
- Capability doc `version` only: the implementation versions legitimately differ (plan §How Authentication Works). Any other capability metadata difference is a divergence.

Anything not on the allowlist that diffs = triage: Go bug (fix it) or newly discovered accepted deviation (document in the migration plan, then allowlist).

### Scenario set (deterministic, named, filterable via `-scenario`)
Run once in dev mode, once in password mode (`-mode dev|password|all`):
1. **Capability/health**: `GET /`, `GET /health`.
2. **Auth (dev)**: login new user, re-login upsert, email normalization, missing email 400, bad JSON 400, whoami, logout → 204, post-logout 401, garbage bearer → `invalid_session` shape, cookie-based auth.
3. **Auth (password)**: correct/wrong password, plaintext and scrypt-hash configs, rate limit: 11th attempt in 60s → 429 + `Retry-After`.
4. **Collections**: claim, second claim (one-per-account behavior), list, get, get unknown → 404, key PUT/GET, delete, post-delete 404s.
5. **Blobs**: `POST /api/blobs`, GET round-trip bytes, DELETE, oversize (>100 MiB real body, once), bad key shapes.
6. **Objects**: create (blob-first flow), list with `sinceVersion`/`limit` incl. clamp at 1000, get, update happy path, stale-version update → 409 body shape, delete, **re-delete → no-op tombstone**, delete with wrong version → 409. The clamp check seeds identical deterministic rows directly into both scratch databases, then requires exactly 1000 objects, `hasMore: true`, and the expected `nextCursor`; this keeps the boundary observable without adding 1000 unrelated mutation steps and identity bindings.
7. **Blob-objects + mutations**: create with `Mutation-Id`, exact replay → `replayed`, `GET create-mutations/:id` for done/unknown, update via blob-objects, batch upload (mixed create/update frames, malformed frame, >200 entries, whole-request cap), batch download (present/missing keys, `omitted` at cap). The per-entry `too_large` result is unreachable with the fixed v1 limits because the 32 MiB request cap is lower than the 100 MiB entry cap; exercise it only if a later change makes the limits configurable or raises `MAX_BATCH_BYTES`.
8. **Ownership**: user B (dev mode's multi-user) hits user A's collection/objects/blobs → 404 everywhere, never 403.
9. **SSE checkpoint** after a mutation burst.

Explicit non-goals for v1: seeded random fuzz generation, mid-request fault injection (kill-and-replay), performance comparison. The scripted set plus the Rust suite is the gate; fuzz is a possible follow-up.

## Part 2 — Rust acceptance suite vs Go server

`scripts/rust-acceptance.sh <ts|go>`: creates a scratch DB, boots the chosen server with `AUTH_MODE=dev PORT=3055 BLOB_DIR=<tmp> COOKIE_SECURE=false` (TS also `BLOB_GC_ENABLED=false`), waits on `/health`, then runs from `/home/justin/Developer/futo-notes`:

```
FUTO_TEST_SERVER=http://127.0.0.1:3055 \
  cargo test -p futo-notes-sync --test server_integration --test sse_live \
  -- --ignored --test-threads=1
```

Run `ts` first as a baseline (proves the suite is green before blaming Go), then `go`. Port 3055 per the client repo's isolation rule. No changes to the client repo.

## Implementation order

1. Dev login (Part 0) + tests + dev UI card.
2. Harness skeleton: scratch DBs, server spawn/health-wait/teardown, mirror client, type-strict differ + identity map.
3. Scenarios 1–4 (capability, auth both modes, collections). First real divergence triage.
4. Scenarios 5–6 (blobs, objects).
5. Scenario 7 (blob-objects, mutations, binary batch, limits).
6. Scenarios 8–9 (ownership, SSE) — wire SSE collectors.
7. Part 2 script; run baseline vs TS, then vs Go; triage failures.
8. Record any newly accepted deviations in `docs/Rewriting the server in Go.md`.

## Verification

- `go test ./...` stays green (dev login has unit coverage).
- Dev login demo visible and working at `/dev` (`DEV_UI=true AUTH_MODE=dev`, port 3005).
- `go run ./cmd/compare -mode all` runs end-to-end; every divergence is either fixed in Go or allowlisted with a doc reference. Exit 0 = wire parity.
- `scripts/rust-acceptance.sh ts` green (baseline), then `scripts/rust-acceptance.sh go` green.
- The launch gate is: differential run clean + Rust suite green against Go.
