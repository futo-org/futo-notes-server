# Launch readiness: swapping the TS server for Go

Goal: enough confidence to tell self-hosters "point the new server at your existing
database and blob dir." There is no central prod. Every cutover runs unsupervised,
on someone else's box, against data and env vars we've never seen, possibly from a
TS version several migrations behind. That reframes what confidence means:

1. The **migration path itself** must be proven repeatable — not one snapshot
   rehearsed once, but an automated rehearsal that synthesizes a TS installation
   and swaps Go into it, runnable on every commit.
2. The **rollback path** must be documented and tested, because we can't be there
   when a cutover goes sideways.
3. The **env contract** must be settled: every old var is either honored or
   deliberately dropped with a note.

## Where things stand (verified 2026-08-21)

Implementation update later on 2026-08-21:

- Step 1 is implemented. `BLOB_GC_ENABLED=false` omits only the destructive GC
  job, and boot warnings identify every recognized dropped variable.
- Step 2 is implemented by `scripts/adoption-rehearsal.sh`; both `latest` and
  the pinned migration-008 variant pass locally, including rollback. The CI
  adoption job runs this plus the wire comparison and both full Rust targets.
- Step 3's build definitions and documentation are implemented: multi-arch
  Docker publishing, cross-platform binaries/checksums, production Compose, and
  `docs/UPGRADING_FROM_TYPESCRIPT.md`. Publishing awaits a release tag.
- Step 4 started on 2026-08-21 at 20:15 UTC. Both manifest-managed staging
  hosts run immutable candidate `candidate-a337e5b7` on port 3010. The cutover
  also corrected the app database route from direct replica port 5510 to the
  Patroni-primary HAProxy port 15510. `scripts/staging-soak-check.sh` records
  the repeatable version, health, restart, log, job, and resource checks.
- Step 5 remains pending the dogfood soak and release tag.

Implementation update on 2026-08-24:

- The container-level swap is now covered by `scripts/compose-rehearsal.sh`. It
  builds both images, brings the TypeScript one up in a throwaway Compose project
  on `docker-compose.production.yml`, seeds real traffic, then swaps by
  re-tagging the single image tag the Compose file names and running
  `docker compose up -d --wait` again — no Compose or `.env` edit between phases,
  which is the closest local stand-in for `docker compose pull && up -d`. It
  passes: 33 asserts green, TypeScript to Go to TypeScript, exit 0. Beyond what
  the process rehearsal already proves, it establishes that Compose recreates the
  container on the tag change, that the image healthcheck reports healthy on both
  images, that the mapped host port serves `/health`, that blobs written by either
  server land in the bind-mounted volume owned by uid 1000, and that the boot
  warning for the dropped `LOG_LEVEL` reaches a self-hoster's `docker compose logs`.
- That rehearsal is not wired into CI. The runners use docker-in-docker, where a
  bind mount the job names is created inside the daemon container instead of being
  shared with the job, so the volume and uid-1000 asserts would read an empty
  directory and the published port would not be on `127.0.0.1`. Verified by
  probing a dind daemon directly, not assumed. The script refuses to start against
  a non-local `DOCKER_HOST` rather than failing confusingly, and it is a
  pre-release manual gate until the runner shares a volume with its dind service.

The wire-parity gate from the migration plan passes today:

- `GOTOOLCHAIN=auto go test ./...` — green. (go.mod requires Go ≥ 1.27; local
  toolchain 1.26.5 needs `GOTOOLCHAIN=auto`.)
- `GOTOOLCHAIN=auto go run ./cmd/compare -mode all` — 185 steps, 175 matched,
  10 accepted deviations (all documented), **0 divergences**, exit 0.
- `./scripts/rust-acceptance.sh ts` then `go` — 28 + 2 tests green against both.
- `./scripts/adoption-rehearsal.sh all` — latest-TS and migration-008 adoption,
  destructive-job auditing, Go writes, and rollback to latest TS all green.
- `./scripts/compose-rehearsal.sh` — the same swap and rollback at the container
  level, through one Compose project and one re-tagged image, all green.
- `docker buildx build --platform linux/amd64,linux/arm64 ...` — both image
  architectures build successfully; the cross-platform release script also
  produces Linux, macOS, and Windows binaries plus checksums.

Also verified: `internal/db/migrate.go` is built for heterogeneous adoption — it
applies only pending migrations, refuses on history gaps, and refuses when the
database records a migration this binary doesn't ship (a newer server touched it).
The adoption rehearsal now proves both a current TS-authored database (zero Go
migrations) and a migration-008 TS-authored database (exactly 009–011 applied).

## Step 1 — Settle the env contract (small code, do first)

Go reads: `DATABASE_URL`, `PORT`, `AUTH_MODE`, `FUTO_NOTES_PASSWORD`,
`FUTO_NOTES_PASSWORD_HASH`, `COOKIE_SECURE`, `DEV_UI`, `BLOB_DIR`, `DB_POOL_MAX`,
`DB_POOL_IDLE_TIMEOUT_MS`. The TS server reads twelve more. Justin is fine
dropping old vars as long as nothing breaks. Proposed disposition:

**Drop, no note needed** — fixed v1 values replace the tunables; nobody's data is
affected: `AUTH_RATE_LIMIT`, `AUTH_RATE_LIMIT_WINDOW_MS`, `MAX_BLOB_BYTES`,
`MAX_BATCH_BYTES`, `BLOB_GC_INTERVAL_MS`, `BLOB_RETENTION_DAYS`, `LOG_LEVEL`.
(Edge case accepted: a hoster who raised `MAX_BLOB_BYTES` loses uploads over
100 MiB. Existing oversized blobs still download — the limit is upload-side only.)

**Drop, with an upgrade-guide note:**
- `DB_SSL`, `DB_SSL_CA`, `DB_SSL_INSECURE` — the driver is pgx/v5, which takes
  `?sslmode=require&sslrootcert=...` in `DATABASE_URL`. Guide shows the mapping.
- `TRUST_PROXY` — the rate limiter keys on the direct peer IP
  (`cmd/server/auth.go:141`). Self-hosters commonly sit behind a reverse proxy,
  so all their devices share the proxy IP and the 10-per-60s login limit becomes
  per-instance. For a single-account instance the worst case is a ~60s login
  delay when one device is spamming a wrong password; logins are otherwise rare
  (7-day tokens, silent re-login). Accept and revisit if the hosted version needs it.

**Honor** — `BLOB_GC_ENABLED`. The one var whose silent dropping does something
irreversible: a hoster who explicitly disabled GC gets their files deleted by a
job they turned off. Skip registering the GC job when it's `false`. Few lines.

**Plus one courtesy:** at boot, log a warning for each recognized-but-dropped var
that is set in the environment, naming the replacement. Cheap, and it's the only
channel we have to a self-hoster mid-upgrade.

## Step 2 — Automated adoption rehearsal (the centerpiece)

A script (shape of `scripts/rust-acceptance.sh`) that proves the swap and the
rollback, repeatably, with no real user's data required:

1. **Synthesize a TS installation**: scratch DB + blob dir, boot the TS server,
   drive real traffic through the Rust client (notes, updates, deletes, key
   round-trip), stop it. Then age the copy with SQL: backdate a superseded
   blob-ledger row past 365 days, a staged row past 24 h, mutation results past
   both expiry tiers, one session past its TTL.
2. **Swap Go in** on the same `DATABASE_URL` and `BLOB_DIR`. Assert: zero
   migrations applied, boots clean.
3. **Continuity asserts**: the TS-issued session token still authenticates (no
   re-login); the TS-minted scrypt hash verifies; a pre-swap sync cursor pulls
   exactly the expected delta; blob bytes round-trip; the Rust client syncs.
4. **Jobs audit**: trigger all four jobs via the dev handlers and assert they
   touch exactly the aged rows planted in step 1 — GC deletes precisely the
   backdated blobs and nothing else. This is the only irreversible operation in
   the system and today it has only ever fired against fresh no-op state.
5. **Rollback**: write through Go (create/update/delete), stop it, boot the TS
   server on the same DB, assert the client still syncs. This proves the
   back-out line in the upgrade guide.
6. **Old-version variant**: rebuild the fixture with the TS server checked out at
   a pre-011 migration state (e.g. 008), swap Go in, assert it applies exactly
   009–011 with Kysely-compatible bookkeeping and everything above still holds.
   Caveat for the guide: after Go has migrated, rollback is to the *latest* TS
   server, not the old version the hoster came from — old Kysely errors on
   migration names it doesn't know.

## Step 3 — Release artifacts + upgrade guide

The self-hoster deliverables:

- **Multi-arch Docker image** (TS ships via Docker Hub multi-arch; self-hosters
  are on amd64 and ARM boxes alike) and a plain single binary — the binary was a
  stated motivation for the rewrite. Update `docker-compose.yml` to match.
- **Upgrade guide**, one page: back up DB + blob dir first; same `DATABASE_URL`
  and `BLOB_DIR` (note: `BLOB_DIR` defaults are cwd-relative on both servers —
  tell people to set it absolute); the dropped-env-var table from Step 1;
  rollback instructions proven in Step 2.5, with the pre-011 caveat from 2.6.

## Step 4 — Dogfood soak

- Swap the staging box (100.76.177.70:3010 — manifest-managed, change via the
  manifest, never by hand) to the Go image.
- Cut over Justin's own instance as self-hoster #1, using the upgrade guide
  verbatim — the guide is part of what's being tested.
- Run 1–2 weeks. This exercises what no scripted run can: long-lived SSE across
  network blips, hourly/6-hourly job fires on accumulating state, memory over days.

## Step 5 — Ship

Re-run the full gate set on the release commit (`go test`, `cmd/compare -mode all`,
both `rust-acceptance.sh` targets, the Step 2 rehearsal), tag, publish image +
binary + guide.

## Optional (explicit non-gates)

- **Concurrency soak**: the harness is serial and the Rust suite runs
  `--test-threads=1`; the write path's `FOR UPDATE` + advisory locks deserve one
  short two-client hammering. Cheap.
- **Performance sanity check**: not a gate, but performance motivated the rewrite.
- **Fuzz / fault injection**: documented follow-up, not needed for the swap.
