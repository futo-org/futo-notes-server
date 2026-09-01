# Migrating the server from Postgres to SQLite

Since v0.7.0 the FUTO Notes sync server uses SQLite by default. New installs
run one container with no database server; the SQLite file and the encrypted
blobs share one data directory, so backup is stop, copy that directory, start.

Existing Postgres installs keep working unchanged. Switching is optional,
one-way, and never modifies Postgres. Clients cannot tell which engine is
underneath.

## Why

Same machine, same workload, old TypeScript + Postgres stack against Go + SQLite:

| | Postgres | SQLite |
| --- | --- | --- |
| Idle memory | 220 MB | 16 MB |
| Cold start to healthy | 3.7 s | 6 ms |
| p50 request latency | 5.3 ms | 0.37 ms |
| Backup | `pg_dump` plus the blob directory | copy one directory |

SQLite is for one server process. Keep Postgres for multi-process or hosted
deployments.

## How to switch

1. Be on the Go server, still on Postgres, and healthy.
2. Stop the server. Back up Postgres, the blob directory, and `.env`.
3. Run the built-in copy. Binary install:

   ```bash
   DATABASE_URL='postgres://user:password@host/notes' \
   ./futo-notes-server migrate-to-sqlite -to sqlite:/srv/futo-notes/notes.db
   ```

   Docker Compose install, into the image's standard path:

   ```bash
   docker compose -f docker-compose.postgres.yml run --rm \
     --volume "${FUTO_NOTES_DATA_DIR:-./futo-notes-data}/db:/data/db" server \
     futo-notes-server migrate-to-sqlite -to sqlite:/data/db/notes.db
   ```

4. Point `DATABASE_URL` at the new `sqlite:` path (or use the current
   `docker-compose.production.yml`, which defaults to it), keep the same blob
   directory mounted, start, and check `curl --fail http://localhost:3005/health`.
5. Edit a note on one device and confirm it reaches another.

## What the copy does

It refuses a target that already holds a database. It opens one read-only
`REPEATABLE READ` snapshot of Postgres, creates the SQLite schema, and copies
users, collections, objects, the blob ledger, mutation results, sessions, and
server configuration inside one SQLite transaction. Before committing it
compares row counts, every collection's current version, and total ledger
bytes against the snapshot. After committing it runs SQLite's integrity and
foreign-key checks. Only then does it print a summary. On any failure the
partial SQLite files are deleted. Blob files are never touched.

## Rollback

Stop the server and set `DATABASE_URL` back to Postgres. That is the exact
state at the snapshot. Edits made while on SQLite stay in SQLite; there is no
reverse converter, so keep the SQLite file until you are done with it.

## Safety guard

Because `DATABASE_URL` is now optional, the server refuses to create a fresh
SQLite database when the blob directory already contains blobs. That usually
means an existing install lost its configuration. Fix the configuration
rather than syncing against an empty vault; `ALLOW_FRESH_DATABASE=true`
overrides the guard only for an intentional fresh start.
