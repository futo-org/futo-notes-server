# Switching from Postgres to SQLite

This optional, one-way copy is for an existing Go-server installation. It does
not modify Postgres or move blob files. Plan a few seconds of downtime and keep
your Postgres backup until you are satisfied with the result.

## Before switching

1. Upgrade the TypeScript server to the Go server while staying on Postgres.
2. Confirm the Go server is healthy and syncing normally.
3. Stop the server. Back up Postgres, `BLOB_DIR`, the Compose file, and `.env`.
4. Keep `DATABASE_URL` pointed at Postgres while running the copy command.

For a binary install, choose the final SQLite path and run:

```bash
DATABASE_URL='postgres://user:password@database/notes' \
./futo-notes-server migrate-to-sqlite -to sqlite:/srv/futo-notes/notes.db
```

For the preserved Postgres Compose stack, run the command in a one-off server
container. Mount or map the target so it remains available to the replacement
server; `/data/db/notes.db` is the standard image location:

```bash
docker compose -f docker-compose.postgres.yml run --rm \
  --volume "${FUTO_NOTES_DATA_DIR:-./futo-notes-data}/db:/data/db" server \
  futo-notes-server migrate-to-sqlite -to sqlite:/data/db/notes.db
```

The target must not contain an existing database. The command reads one
repeatable Postgres snapshot, copies users, collections, objects, the blob
ledger, mutation results, sessions, and server configuration, and then checks
table counts, collection versions, ledger bytes, SQLite integrity, and foreign
keys. It prints a table only after every check succeeds. A failed run removes
the partial SQLite files.

Blob files are not copied. Keep `BLOB_DIR` exactly where it was.

## Start on SQLite

Change `DATABASE_URL` to the target SQLite URL, or use the new
`docker-compose.production.yml`, whose image default is
`sqlite:/data/db/notes.db`. Make sure the same blob directory is mounted, then
start the server and verify:

```bash
curl --fail http://localhost:3005/health
```

Open an existing note, make an edit, and confirm it reaches another client.
Once backups and the SQLite installation are proven, the old Postgres service
can remain stopped or be removed at your convenience.

## Roll back

Stop the server and point `DATABASE_URL` back at the untouched Postgres
database. That restores the exact state captured before the switch.

Edits accepted while SQLite was active exist only in SQLite. Rolling back to
Postgres discards those post-switch edits; there is no built-in reverse
converter. Preserve the SQLite database if you may need to recover them later.

SQLite is intended for one server process. Use Postgres for hosted or other
multi-process deployments because SQLite's in-process sync doorbell does not
cross process boundaries.
