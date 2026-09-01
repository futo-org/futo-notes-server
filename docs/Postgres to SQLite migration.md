# Postgres to SQLite: why and how the server migrates

The FUTO Notes sync server moved from Postgres to SQLite as the default
database for self-hosted installs in v0.7.0. This document explains why, what
the migration does, and what it leaves alone. The step-by-step commands are in
[Switching from Postgres to SQLite](Switching%20from%20Postgres%20to%20SQLite.md).

## Summary

- New installs use SQLite. There is no database server to run, and the
  database lives in the same directory as the encrypted blobs, so one
  directory is the whole server.
- Existing Postgres installs keep working on Postgres indefinitely. Nothing
  forces a switch.
- Switching is opt-in, one-way, and done with a command built into the server
  binary: `futo-notes-server migrate-to-sqlite`. It reads Postgres, writes a
  new SQLite file, verifies the copy, and never modifies Postgres.
- Rollback is pointing `DATABASE_URL` back at the untouched Postgres database.
- The client never sees which engine is underneath. The wire contract is
  unchanged.

## Why SQLite

The server is a small, single-user service that stores encrypted blobs and a
few metadata tables. Postgres was the wrong size for that job on a
self-hoster's box. Measured on one machine with the same workload, the old
TypeScript + Postgres stack against the new Go + SQLite stack:

| | TypeScript + Postgres | Go + SQLite |
| --- | --- | --- |
| Resident memory, idle | 220 MB (78 app + 142 Postgres) | 16 MB |
| CPU over 60 s idle | 1.07 s, about 1.8% of a core | below the 10 ms clock tick |
| Cold start to healthy | about 3.7 s | 6 ms, including creating the database |
| p50 latency, light load | 5.3 ms | 0.37 ms |
| Peak memory under saturation | 399 MB | 28 MB |
| Metadata on disk | 14 MB | 5.9 MB |
| Backup | `pg_dump` plus the blob directory | stop, copy one directory, start |

The full method and every figure are in
[Resource comparison, TypeScript vs Go](Resource%20comparison,%20TypeScript%20vs%20Go.md).
The latency difference comes from the database being in-process: every query
that used to cross a socket to another container is now a function call.

The operational argument matters as much as the numbers. With Postgres, a
self-hoster had two containers to keep alive, a database password to manage,
and a backup that needed `pg_dump` for one half and a file copy for the other.
With SQLite, backup is: stop the server, copy the data directory, start it.

## What stays on Postgres

- **Existing installs.** A Postgres URL in `DATABASE_URL` works exactly as
  before. The TypeScript-to-Go upgrade lands you on Go running Postgres, and
  you can stay there.
- **Hosted and multi-process deployments.** SQLite is for one server process.
  The sync doorbell that wakes connected clients runs in-process under SQLite,
  so it cannot cross process boundaries. Postgres remains the engine for
  anything that runs more than one server process.

One binary serves both. The engine is chosen per install by the `DATABASE_URL`
scheme: `postgres://` or `postgresql://` for Postgres, `sqlite:<path>` for
SQLite, and unset means SQLite at a default path (`./data/notes.db` for the
bare binary, `/data/db/notes.db` in the Docker image).

## Order of operations for an existing install

1. Upgrade from the TypeScript server to the Go server, staying on Postgres.
   See [the upgrade guide](UPGRADING_FROM_TYPESCRIPT.md).
2. Confirm the Go server is healthy and syncing.
3. Optionally, switch engines with `migrate-to-sqlite`.

The engine switch is deliberately a separate second step so that any problem
can be attributed to one change at a time.

## What `migrate-to-sqlite` does

The command lives in the server binary and is implemented in
`internal/db/convert.go`. Given `DATABASE_URL` pointing at Postgres and `-to`
naming a SQLite path, it:

1. **Refuses an unsafe target.** If the SQLite path already holds a database,
   it stops. A zero-byte file is treated as absent.
2. **Takes one consistent snapshot of Postgres.** It opens a read-only
   transaction at `REPEATABLE READ` isolation, so every table is read as of
   the same instant even if the server were still writing. Nothing is written
   to Postgres at any point.
3. **Creates the SQLite schema** by running the server's own migrations
   against the new file.
4. **Copies each table inside one SQLite transaction**: users, collections,
   objects, the blob ledger, mutation results, sessions, and server
   configuration. Values are converted where the engines differ, such as
   compacting JSON columns.
5. **Verifies against the snapshot before committing.** Row counts per table
   and the current version of every collection are compared between the
   Postgres snapshot and the SQLite copy, along with the total bytes in the
   blob ledger.
6. **Commits, then verifies SQLite itself** with an integrity check and a
   foreign-key check.
7. **Prints a summary table** of copied row counts. It prints this only after
   every check has passed.

If any step fails, the partial SQLite files are removed so a half-copied
database cannot be mistaken for a finished one.

Blob files are not touched. The encrypted blobs stay in `BLOB_DIR`, and the
SQLite server reads them from the same place. Only the metadata moves.

## Rollback

Stop the server and point `DATABASE_URL` back at Postgres. Because the copy
never wrote to Postgres, that restores exactly the state at the moment of the
snapshot.

The limit: edits accepted while running on SQLite exist only in SQLite. There
is no reverse converter. Rolling back discards those edits, so keep the SQLite
file if you might need them later, and prove the SQLite install before
removing the Postgres service.

## How one codebase serves both engines

The SQL is not forked. A small dialect seam in `internal/db` handles the
places where the engines differ: `$N` placeholders become `?N` for SQLite,
Postgres row-lock clauses (`FOR UPDATE`, `SKIP LOCKED`) become no-ops under
SQLite's single writer, and the Postgres `LISTEN/NOTIFY` doorbell is replaced
by an in-process hub that fires after each commit. SQLite runs in WAL mode
with a busy timeout, foreign keys on, and a pure-Go driver, which is what lets
the same binary ship for Linux, macOS, and Windows without a C toolchain.

A safety guard covers the one new failure mode SQLite introduces. Because
`DATABASE_URL` is now optional, an install that lost its configuration could
boot a fresh, empty database beside a full blob directory and let clients sync
against an empty vault. The server refuses to create a new SQLite file when
`BLOB_DIR` already contains blobs, and names the likely cause. The override
`ALLOW_FRESH_DATABASE=true` exists only for an intentional fresh start.
