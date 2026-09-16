---
status: accepted
date: 2026-09-16
---

# Adopting a TypeScript database is forward-only from v0.8.0

Until the hosted work landed, a hoster could swap the Go server in on a
TypeScript database and, if it went badly, put the TypeScript server back.
`scripts/adoption-rehearsal.sh` proved that round trip on every pipeline, and
`.gitlab-ci.yml` named it as the point of the `test:adoption` job. That is no
longer true, and this document records the decision to stop promising it
rather than to restore it.

## What changed

The Go server shipped its own migrations for the first time. Everything up to
`011_mutation_results` is a name the TypeScript server also has, so adopting a
current TypeScript database used to apply **zero** migrations. The hosted work
added seven the TypeScript server has never heard of:

`012_login_handoffs`, `013_plans_and_usage`, `014_subscriptions`,
`015_polar_customer_before_subscription`, `016_recovery_envelope`,
`017_sliding_sessions`, `018_device_pairings`.

Both servers share one bookkeeping table, `kysely_migration` — deliberately, so
each recognizes the other's work (`internal/db/migrate.go:18`). That is exactly
what makes the return trip impossible. Measured on 2026-09-16 against
PostgreSQL 16 and the TypeScript server at `5449502`:

1. TypeScript creates the database: 11 rows in `kysely_migration`.
2. Go adopts it and applies the seven above: 18 rows.
3. TypeScript will not start against it:

   ```
   Error: corrupted migrations: previously executed migration 012_login_handoffs is missing
   ```

   Kysely's `Migrator.migrateToLatest()` refuses a database holding executed
   migrations its provider does not list. This is Kysely's integrity check
   doing its job, not a bug in either server.
4. `DELETE FROM kysely_migration WHERE name >= '012_'` does let TypeScript
   start again with its data intact — but Go can then never adopt that
   database again:

   ```
   applying migration 016_recovery_envelope: ERROR: column "recovery_key_salt"
   of relation "collections" already exists (SQLSTATE 42701)
   ```

   The migration DDL is not idempotent, so the prune is a one-way door in the
   other direction. There is no sequence of supported operations that gets a
   hoster from an adopted database back to a working TypeScript server and
   then forward again.

## The decision

**Rollback to the TypeScript server is supported up to migration
`011_mutation_results` and ends at v0.8.0.** Once a Go server at v0.8.0 or
later has started against a database, that database is a Go database.

Rejected alternatives, and why:

- **Give the Go-only migrations their own bookkeeping table** so Kysely never
  sees them. It would work, but it splits migration history across two tables
  for one row of benefit, and every deployment that already ran 012–018 —
  including the one live instance — would need a hand-written repair before it
  could be upgraded again.
- **Make the migration DDL idempotent and document the prune.** Cheaper, but it
  makes "roll back" mean "run this SQL by hand against your production
  database, correctly, under stress". That is a worse promise than no promise.
- **Hold the hosted work until rollback is solved.** The cost is real and the
  benefit is not: see below.

What made the cost small enough to accept: there is no production hosted
deployment — neither `notes-sync.futo.org` nor `staging-notes-sync.futo.org`
resolves — and the only existing futo-notes sync deployment in
`manifest-inventory` (`inventory/futo-notes/sync-server.yml`) is tailnet-only,
staging-hosts-only, `AUTH_MODE=dev`, with no load balancer, and it is
**already running a Go candidate image** (`candidate-a337e5b7`). The one live
instance crossed this boundary before the boundary was written down.

## What this costs a self-hoster, and what they do instead

The group that pays is a self-hoster still on the TypeScript server. They are
not covered by the paragraph above, and they get an honest procedure rather
than silence:

- **Back up the database and the blob directory before starting the Go server.**
  `pg_dump` plus a copy of `BLOB_DIR`. This was always the first line of the
  upgrade guide; it is now the *only* way back, so it is not optional advice.
- **Roll back by restoring that backup**, not by putting the old binary back on
  the live database. Restore the dump into the same database name, restore the
  blob directory, start the TypeScript server. Anything written through the Go
  server after the backup is lost — which is the real reason to satisfy
  yourself the Go server works before you send traffic at it for a week.
- **Rehearse on a copy first.** `pg_dump` the live database into a scratch one,
  point a Go server at the scratch database and a copy of the blob directory,
  and drive a client at it. That rehearsal costs an afternoon and is now worth
  more than it used to be.
- A hoster coming from **before migration 011** is unaffected by the choice but
  not by the boundary: Go applies 009–018 in one go, and the rollback target
  was never their old version anyway.

## How this is enforced

`scripts/adoption-rehearsal.sh` asserts the forward-only contract instead of
the round trip: Go adopts a TypeScript database and applies exactly the
migrations the TypeScript server does not ship, the adopted database keeps
serving, and **starting the TypeScript server against the adopted database is
asserted to fail with the `corrupted migrations` error naming the first
Go-only migration**. The expected migration sets are derived from the two
checkouts' migration directories, not written down as a count, so a new
migration does not make the gate stale.

That negative assertion is the part worth keeping. If someone later makes the
return trip work — a separate bookkeeping table, idempotent DDL, anything —
the job fails and sends them here to change the decision on purpose.
