# Moving your FUTO Notes server from Postgres to SQLite

This guide assumes you run the server with Docker Compose, the way the
installer set it up. All commands run from the directory that holds your
`docker-compose.yml` and `.env`.

Since v0.7.0 the server is written in Go and uses SQLite by default. New
installs are one container, and the database sits in the same folder as your
encrypted notes, so a backup is just a copy of that folder. Your existing
Postgres install keeps working as it is. Moving to SQLite is optional, and
nothing in this guide changes your Postgres data, so you can always go back.

## Why bother

Same machine, same workload:

| | Postgres | SQLite |
| --- | --- | --- |
| Memory while idle | 220 MB | 16 MB |
| Time to start | 3.7 s | 6 ms |
| Typical request | 5.3 ms | 0.37 ms |
| Backup | `pg_dump` plus copying the notes folder | copy one folder |

## Step 1: update to the new server

The new server reads your existing Postgres database and notes folder as they
are. Back up first, then swap in the new compose file and pull:

```bash
docker compose stop server
docker compose exec -T postgres pg_dump -U futo_notes -d futo_notes -Fc > before-go.dump
cp -a futo-notes-data/blobs blobs.before-go
cp .env .env.before-go

curl -fsSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/docker-compose.postgres.yml -o docker-compose.yml
docker compose pull
docker compose up -d
curl --fail http://localhost:3005/health
```

The dump covers the database and the copy covers your encrypted notes; the
`futo-notes-data/postgres` folder belongs to the database container and does
not need copying by hand. If your notes folder is somewhere other than
`./futo-notes-data`, use that path instead. If your `.env` pins
`FUTO_NOTES_IMAGE` to a specific version, remove that line so the pull gets the
current server.

Open a note in the app, make an edit, and check it shows up on another device.
You can stop here and stay on Postgres for as long as you like.

## Step 2: switch to SQLite

Stop the server and run the built-in copy. It reads Postgres and writes a new
SQLite file next to your notes folder. Postgres is not modified.

```bash
docker compose stop server
docker compose run --rm --volume "$PWD/futo-notes-data/db:/data/db" server \
  futo-notes-server migrate-to-sqlite -to sqlite:/data/db/notes.db
```

When it finishes it prints a table of what it copied. It only prints that
table after checking row counts, every collection's version, and the SQLite
file's integrity against the Postgres snapshot. If anything fails it deletes
the half-written file and tells you why.

Now switch to the single-container compose file and start it. Keep the
Postgres one around in case you want to go back:

```bash
docker compose down
mv docker-compose.yml docker-compose.postgres.yml
curl -fsSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/docker-compose.production.yml -o docker-compose.yml
docker compose up -d
curl --fail http://localhost:3005/health
```

Edit a note on one device and confirm it reaches another. You are now on
SQLite. Your notes folder holds both the database and the encrypted notes;
back it up together with `.env`.

The `POSTGRES_PASSWORD` line in `.env` and the `futo-notes-data/postgres`
folder are no longer used. Leave them until you are sure you will not go back,
then delete them.

## Going back

Stop the server, put the Postgres compose file back, and start it:

```bash
docker compose down
mv docker-compose.postgres.yml docker-compose.yml
docker compose up -d
```

Postgres is exactly as it was when you ran the copy. Any edits made while on
SQLite stay in the SQLite file, so keep `futo-notes-data/db` if you might want
them.

## One safety net to know about

If the server ever starts with a brand-new empty database while your notes
folder already has notes in it, it refuses to run and says so. That almost
always means the compose file or `.env` was changed and the server lost track
of its database. Fix the configuration rather than letting devices sync
against an empty vault.
