# SQLite by default for self-hosting

Plan for making SQLite the default database for the self-hosted server.
`Rewriting the server in Go.md` deliberately punted this ("punting on the
other changes we're thinking about, like introducing SQLite"); this doc
un-punts it. Nothing here changes the wire contract — the client never sees
which engine is underneath.

## Product decisions (made 2026-08-25)

- **New installs get SQLite with no choice offered.** The docs stop
  describing Postgres as a self-host option. Postgres URLs keep working in
  `DATABASE_URL` — existing installs and the future hosted service need them
  — they are just no longer part of the documented new-install path.
- **Existing installs change nothing.** A Postgres install keeps running on
  Postgres indefinitely. Switching is opt-in, one-way, via a built-in
  command (below).
- **This ships inside the go-rewrite launch**, not after it. New installs
  never set up Postgres. The cost is accepted: launch scope grows, and the
  staging soak has to cover a SQLite instance too. The TypeScript→Go upgrade
  path is unchanged — TS installs are Postgres installs and land on Go
  running Postgres; switching engines is a separate, optional second step.
- **No reverse tool.** The switch never modifies the Postgres data, so the
  rollback story is "point `DATABASE_URL` back at Postgres." Edits made
  while on SQLite stay behind; the switch guide says so explicitly. A
  SQLite→Postgres converter can be built later if hosted import wants one.
- **Hosted stays on Postgres** and is unaffected. One binary serves both;
  the engine is picked per install by `DATABASE_URL`.

## What is Postgres-specific today

An audit of every SQL call site (about 64, all in `internal/` plus
`cmd/server/devui.go`) finds the coupling concentrated in seven things:

1. **Row locks** — `FOR UPDATE` in `internal/objects/objects.go`,
   `internal/collections/collections.go`, `internal/collections/key.go`,
   `internal/blobs/blobs.go`; `FOR UPDATE SKIP LOCKED` in
   `internal/jobs/jobs.go`. SQLite cannot parse either clause.
2. **Advisory locks** — `pg_advisory_xact_lock(hashtextextended(...))` for
   mutation idempotency (`objects.go:250`) and a fixed-key advisory lock
   guarding the migration run (`internal/db/migrate.go:77`).
3. **LISTEN/NOTIFY** — the sync SSE doorbell. `events.Publish` runs
   `pg_notify` inside the mutation transaction; `events.Listen` holds a
   dedicated pgx connection.
4. **Time in SQL** — `now()` and `now() - interval '...'` in inserts,
   updates, and all the retention queries in `jobs.go`.
5. **One regex** — `result->>'status' ~ '^2'` in `ExpireMutationResults`.
   SQLite has no built-in regex.
6. **One data-modifying CTE** — `ReconcileStorage` does an
   `INSERT ... ON CONFLICT` inside a `WITH`. SQLite does not allow writes in
   CTEs.
7. **Types and casts** — `uuid`, `bytea`, `timestamptz`, `jsonb`, a
   `$2::jsonb` cast in `key.go`, and `$N` placeholders.

Everything else already sits in the portable subset: `ON CONFLICT DO
NOTHING/DO UPDATE`, `RETURNING`, and the `->>` JSON operator all work in the
SQLite the chosen driver bundles.

## Engineering decisions

### Driver: `modernc.org/sqlite`

The pure-Go port, not `mattn/go-sqlite3`. The Dockerfile builds with
`CGO_ENABLED=0` and releases ship Linux/macOS/Windows binaries from one
builder; a CGO driver would break both. The driver bundles its own current
SQLite, so features we rely on (`RETURNING`, `->>`, upsert) are pinned by
`go.mod`, not by whatever the host OS ships.

### Engine selection

`DATABASE_URL` picks the engine by scheme:

- `postgres://` / `postgresql://` — Postgres, exactly as today.
- `sqlite:<path>` — SQLite at that file path.
- unset — SQLite at the default path. Bare binary: `./data/notes.db`
  (created on first boot; `BLOB_DIR` keeps its `./blobs` default). Docker
  image: the image sets `ENV DATABASE_URL=sqlite:/data/db/notes.db`, so the
  database lives beside `/data/blobs` and one `/data` volume is the whole
  server state.

`DB_POOL_MAX` / `DB_POOL_IDLE_TIMEOUT_MS` keep applying to Postgres; under
SQLite the pool is sized internally (see concurrency) and these are ignored
without warning.

### Fresh-database guard

Making `DATABASE_URL` optional creates a footgun: an existing install that
loses its env (edited compose file, lost `.env`) would silently boot a brand
new empty SQLite database, and clients would sync against an empty vault.
Guard: **if the SQLite file is being created fresh and `BLOB_DIR` already
contains blob files, refuse to boot** with an error naming the likely cause
and the override (`ALLOW_FRESH_DATABASE=true`). The guard is SQLite-only —
a missing Postgres URL already fails loudly today, and a fresh Postgres
database with an old blob dir is a scenario the reconciliation job
deliberately supports.

### One query set, a small dialect seam

We do not fork the SQL. `internal/db` grows a `Dialect` that `Open` returns
alongside the `*sql.DB`, carrying the few divergent pieces:

- **Placeholders**: a mechanical `$N` → `?N` rewrite for SQLite. This is
  correctness, not cosmetics: SQLite treats `$N` as a *named* parameter
  indexed by first appearance, so a query like the `UPDATE ... SET
  collection_id = $2 ... WHERE blob_key = $1` in `objects.go` would bind
  arguments to the wrong slots. `?N` binds by the explicit number, matching
  Postgres semantics exactly. The rewrite is a tested helper, applied at the
  call sites through the seam.
- **Lock clauses**: `dialect.ForUpdate` / `dialect.ForUpdateSkipLocked`
  strings appended to the locking queries — the current clauses under
  Postgres, empty under SQLite, where the immediate write transaction
  (below) already serializes writers. The existence checks those queries
  perform still run unchanged.
- **Mutation lock**: `lockMutation` becomes a dialect hook — advisory lock
  under Postgres, no-op under SQLite (the single writer makes the
  `mutation_results` upsert race it guards impossible).
- **JSON cast**: the `::jsonb` in `key.go` comes from the dialect (`""` for
  SQLite).
- **Events**: an interface (next section).
- **Migrations**: per-dialect migration directories (below).

### Dialect-neutral SQL first (no-behavior-change refactor)

Before SQLite exists, normalize the shared SQL so the seam stays small:

- **Time moves into Go.** Every `now()` and `now() - interval '...'`
  becomes a parameter computed in Go (`time.Now().UTC()`, cutoffs as
  `now.Add(-24h)` etc.). Inserts that today rely on `default now()` pass
  timestamps explicitly. This trades the database clock for the app clock —
  in every supported topology they are the same machine or the skew is
  irrelevant to 24-hour/30-day/365-day windows. It also makes retention
  tests clock-injectable, which they currently are not.
- `result->>'status' ~ '^2'` becomes `result->>'status' LIKE '2%'` — same
  meaning (status is stored as a number-string), valid in both engines.
- The `ReconcileStorage` CTE splits into two plain statements inside a short
  transaction (check owner, then `INSERT ... ON CONFLICT DO NOTHING`); the
  race it closed is covered by the transaction.

This lands as its own commit series on Postgres only, gated by the existing
tests and the comparison harness, so any behavior drift is caught while
there is still exactly one engine.

### SQLite schema: one baseline, then per-dialect migrations

The eleven Postgres migrations do not replay on SQLite — several are
unportable (`array_agg` backfills, multi-column `ALTER`, `DROP CONSTRAINT`)
and all of that history is Postgres-install history. SQLite gets
`migrations/sqlite/001_baseline.sql`: the current end-state schema, minus
`orphaned_blobs` (superseded by migration 010; nothing reads or writes it —
it survives only so existing Postgres databases keep their shape).

Type mapping: `uuid`/`text`/`timestamptz` → `TEXT`, `bytea` → `BLOB`,
`bigint` → `INTEGER`, `boolean` → `INTEGER` (the driver maps Go `bool`),
`jsonb` → `TEXT`. `CHECK` constraints and `ON DELETE` behavior carry over
verbatim; SQLite ships with foreign keys **off**, so the connection setup
turns them on (below) and a test asserts the cascades actually fire.

Timestamps are stored as fixed-width UTC text, `2006-01-02T15:04:05.000Z`,
always exactly three fractional digits, written and parsed only by our code
— never by driver defaults. Fixed width makes lexicographic order equal
chronological order, so every `<` comparison in the retention queries stays
a plain text comparison.

`migrate.go` is shared: same `kysely_migration` bookkeeping tables, same
gap/unknown-name checks, with the migration *set* chosen per dialect
(`migrations/postgres/`, `migrations/sqlite/`) and the boot lock per
dialect — `pg_advisory_xact_lock` under Postgres, `BEGIN IMMEDIATE` under
SQLite. Every migration written after this plan lands twice, once per
directory.

### Concurrency model under SQLite

Connection setup via DSN pragmas: `journal_mode(WAL)`,
`busy_timeout(10000)`, `foreign_keys(1)`, `synchronous(NORMAL)`, and
`_txlock=immediate` so every transaction takes the write lock at `BEGIN`
rather than deadlocking on lock upgrade mid-transaction. WAL gives
concurrent readers under a single writer; the pool stays small (writes
serialize anyway, and the database holds only metadata — note bytes live in
`BLOB_DIR`). All mutation transactions are short (blob bytes are written
outside them), so a 10-second busy timeout is headroom, not a workaround.

One consequence to document, not engineer around: SQLite means one server
process. Two processes sharing the file would work for data but the
in-process SSE doorbell would not cross them. Postgres remains the answer
for multi-process topologies (hosted).

### Sync events without LISTEN/NOTIFY

`events` grows a `Publisher` seam with two implementations:

- **Postgres**: today's behavior, untouched — `pg_notify` inside the
  transaction, the dedicated listener connection, reconnect-with-backoff,
  `CloseAll` on gaps.
- **SQLite**: publishes buffer on the transaction and flush straight into
  the in-process `Hub` after `Commit` succeeds — preserving the "doorbell
  only rings for committed work" ordering. `events.Listen` is simply not
  started; the reconnect/gap machinery has nothing to reconnect to, and
  in-process delivery cannot gap.

Only `objects.go`'s three publish sites change shape, since they own their
transactions.

### The switch command

A subcommand of the server binary — no extra tools to install, works
everywhere the server runs:

```bash
futo-notes-server migrate-to-sqlite [-to sqlite:/data/db/notes.db]
```

Behavior:

- Requires `DATABASE_URL` to be Postgres; the target defaults to the
  engine-default path. Refuses an existing non-empty target file.
- Run with the server **stopped** (documented; seconds of downtime on a
  personal server). As a backstop the whole read happens in one
  `REPEATABLE READ` snapshot, so a concurrent server produces a consistent
  copy of a moment in time rather than a torn one.
- Copies in FK order: `users`, `collections`, `objects`, `blob_ledger`,
  `mutation_results`, `sessions` (so clients stay logged in),
  `server_config`. Skips `orphaned_blobs` (folded into `blob_ledger` by
  migration 010) and the Postgres migration history — the SQLite migration
  names are recorded as applied instead. Timestamps convert to the fixed
  text format; `jsonb` values are written as their compact text rendering.
- Blob files are not touched. `BLOB_DIR` stays exactly where it was.
- Verifies before reporting success: per-table row counts, per-collection
  `max(change_seq)` against `current_version`, `sum(size_bytes)` over the
  ledger, then SQLite's own `PRAGMA integrity_check` and
  `PRAGMA foreign_key_check`. Prints a summary table; non-zero exit on any
  mismatch, and a failed run deletes its partial target.

Then the operator flips `DATABASE_URL` to the sqlite URL (or deletes the
variable where the image default applies) and drops the postgres service
whenever they feel safe. The guide spells out the rollback: the Postgres
data was never written to; repointing at it discards whatever synced after
the switch.

### Packaging and docs

- **Image**: `ENV DATABASE_URL=sqlite:/data/db/notes.db`; entrypoint
  creates and chowns `/data/db` the way it already handles `$BLOB_DIR`.
- **`docker-compose.production.yml`** becomes the single-service SQLite
  stack with one `/data` volume. The current two-service stack is preserved
  verbatim as `docker-compose.postgres.yml` for existing installs, and
  `UPGRADING_FROM_TYPESCRIPT.md` is updated to reference it. An existing
  Postgres user who grabs the new production file by habit is caught by the
  fresh-database guard instead of silently starting empty.
- **README** self-host section: SQLite is the only documented path; backup
  is "stop, copy `/data`, start" — one directory now holds everything.
- **New doc**: `docs/Switching from Postgres to SQLite.md` — prerequisites,
  the command, the env flip, verification, rollback, and the
  post-switch-edits caveat.
- **Dev compose**: `docker compose up -d postgres` stays, for working on
  the Postgres side; plain `go run ./cmd/server` now needs no database at
  all, which is itself a developer win.
- **`/dev`**: the dev page shows the active engine and database path, and
  the existing dev job triggers exercise the retention queries against
  whichever engine is live — that is the feature's demo per the repo rule.

## Phases

Each phase is independently mergeable and gated before the next starts.

**Phase 0 — dialect-neutral SQL, Postgres-only.** Time-as-parameters,
`LIKE` for the regex, CTE split, explicit insert timestamps. Gate: full
test suite plus `go run ./cmd/compare -mode all` — this phase must be
invisible.

**Phase 1 — the seam and the engine.** `Dialect`, placeholder rewrite, lock
clauses from the dialect, `Publisher` seam, SQLite open path with pragmas,
`migrations/sqlite/001_baseline.sql`, fresh-database guard, engine selection
by scheme (explicit `sqlite:` URLs only; no default flip yet). Integration
tests run on SQLite *by default* — a temp file, no env vars — and on
Postgres when the existing `*_TEST_DATABASE_URL` variables are set, so
`go test ./...` finally exercises real storage on a bare machine and CI
covers both engines in the one `test:go` job.

**Phase 2 — parity and robustness gates.** A comparison-harness engine mode
running Go-on-Postgres against Go-on-SQLite over the existing frame
comparison (seeding made dialect-aware — `cmd/compare/scenarios.go` seeds
with Postgres-typed SQL today). A concurrency hammer: parallel mutations,
SSE subscribers, and the GC/reconciliation jobs against one SQLite file,
asserting no `database is locked` ever surfaces to a client. One audit item
to settle here: Postgres `jsonb` normalizes the stored collection-key KDF
JSON (key order, duplicate keys) while SQLite text preserves the client's
bytes — expected to be an accepted deviation since JSON object key order is
not semantic, but the harness proves it either way.

**Phase 3 — the switch.** `migrate-to-sqlite`, its verification suite, and
the switch guide. Gate: rehearsal that creates a populated Postgres install
(reusing the adoption-rehearsal machinery), switches it, and drives the
real client against the result — existing notes open, edits sync, staged
blobs still claim, retention jobs run.

**Phase 4 — the default flip and launch integration.** `DATABASE_URL`
becomes optional, image ENV set, compose/README/UPGRADING changes, `/dev`
engine display. Launch gates grow a SQLite leg: `rust-acceptance.sh go`
runs against a SQLite-backed server too, `compose-rehearsal.sh` gains a
new-install SQLite pass, and staging gets a SQLite instance for the soak —
as a second manifest-managed instance beside the current Postgres one, so
the running soak is not reset (the staging box is manifest-managed;
coordinate the manifest change, don't hand-edit the host).
`Launch readiness plan.md` gets these gates added when Phase 4 starts.

## What deliberately does not change

- The wire contract, blob directory layout, session tokens, password
  hashing, job cadence and retention windows.
- Postgres behavior. After Phase 0's audited normalization, the Postgres
  path is byte-for-byte the code that soaked — the dialect seam returns its
  current clauses and the listener machinery is untouched.
- The TypeScript upgrade path: still Postgres-to-Postgres, still governed
  by `UPGRADING_FROM_TYPESCRIPT.md` and the adoption rehearsal.

## Risks

- **Placeholder semantics** are the sharpest correctness edge (out-of-order
  `$N` binds wrong under SQLite's named-parameter rules). Mitigated by the
  `?N` rewrite plus a test that walks every query through the rewriter.
- **Foreign keys default off** in SQLite. The pragma is in the DSN, and a
  test deletes a user and asserts the sessions/collections/objects/ledger
  cascades fired.
- **Single-writer stalls**: a long transaction would block all writes.
  Mutation transactions are short by construction (blob bytes are written
  outside them); the Phase 2 hammer is the gate that proves it.
- **Conversion fidelity** (timestamp format, jsonb rendering, NULLs):
  covered by the switch tool's verification pass plus the Phase 3
  real-client rehearsal.
- **Lost-env footgun** creating fresh databases over live installs: the
  boot guard.
- **App clock replacing DB clock** for retention windows: irrelevant at
  self-host scale (same machine), noted for the hosted design review.

## Open questions

None blocking. The product questions (launch sequencing, reverse path) were
decided 2026-08-25 and are recorded above.
