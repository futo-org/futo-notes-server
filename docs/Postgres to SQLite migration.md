# Moving your FUTO Notes server from Postgres to SQLite

Since v0.7.0 the server is written in Go and uses SQLite by default. New
installs are one container, and the database sits in the same folder as your
encrypted notes, so a backup is just a copy of that folder.

Your existing Postgres install keeps working as it is. Moving to SQLite is
optional. The copy is one-way, but it never modifies Postgres and never moves
blob files, so Postgres stays a working rollback target.

## Why?

It's faster and more lightweight.

| | Postgres | SQLite |
| --- | --- | --- |
| Memory while idle | 220 MB | 16 MB |
| Time to start | 3.7 s | 6 ms |
| Typical request | 5.3 ms | 0.37 ms |
| Backup | `pg_dump` plus copying the notes folder | copy one folder |

## Do it

This is for Docker Compose installs, the way the installer sets one up. Run it
from the directory that holds your `docker-compose.yml` and `.env`:

```bash
curl -fsSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/migrate-to-sqlite.sh | sh
```

That is the whole procedure. The script prints what it is about to do, waits
for you to confirm, and stops on the first thing that looks wrong.

Expect about a minute of downtime, and set aside a few minutes of your own for
the checkpoint in the middle. Running the server as a binary or systemd unit
instead? See [Doing it by hand](#doing-it-by-hand) below.

### What it does

1. Works out where your notes, database and published port actually are, by
   inspecting the running containers rather than assuming a layout.
2. Backs up the Postgres database, `.env` and `docker-compose.yml` into a
   `before-sqlite-<date>` folder next to your compose file.
3. Upgrades the server container to the current image, still running on
   Postgres, and waits for `/health`. This is also what applies any missing
   database migrations, which the copy in step 5 expects.
4. **Pauses.** Open the app, check your notes are there and that an edit syncs.
   Nothing has been converted yet, so this is the safe place to stop.
5. Copies Postgres into `<your data folder>/db/notes.db`.
6. Restarts on the single-container SQLite compose file and waits for
   `/health` again.

Your encrypted note files are never written to, and Postgres is only read.
When it finishes, the script prints where everything ended up and the exact
command to go back.

### If the script stops early

It stops rather than guessing whenever your install does not look like the one
it knows how to convert — a blob directory that is not a host folder named
`blobs`, an authentication mode other than `password`, a `notes.db` that
already exists. Each message says what it found and what to do about it.

Nothing has been changed at that point beyond, at most, a stopped server and a
backup folder, and the message tells you how to start the server again.

## Going back

Postgres is exactly as it was when the copy ran, so rolling back is restoring
the two backed-up files:

```bash
docker compose down
cp before-sqlite-<date>/docker-compose.yml before-sqlite-<date>/.env .
docker compose up -d
```

Edits accepted while SQLite was active exist only in the SQLite file. Rolling
back discards them — there is no reverse converter — so keep the `db` folder if
you might want them later.

Once you are sure you will not go back, the Postgres data directory (or volume)
and the `POSTGRES_PASSWORD` line in `.env` are unused and can go.

## Afterwards

Back up your data folder together with `.env`. That folder is the whole
server: the SQLite database and the encrypted notes. There is no database
server to dump any more.

## Doing it by hand

For a binary or systemd install, or if you would rather run the steps
yourself. The Compose script above does all of this, plus the backup and the
health checks.

Before you start:

1. If you are still on the TypeScript server, upgrade to the Go image while
   staying on Postgres first. The copy expects the Go server's schema, which
   the Go server applies at boot.
2. Confirm the Go server is healthy and syncing normally.
3. Choose the SQLite path. It must not already hold a database, and the user
   the server runs as must be able to write to its directory.

Keep `DATABASE_URL` pointed at Postgres while you run the copy — it is the
source being read. For the install layout in the README, that is:

```bash
sudo systemctl stop futo-notes-server
pg_dump -Fc 'postgres://user:password@localhost/notes' > ~/futo-notes-before-sqlite.dump
sudo -u futo-notes env DATABASE_URL='postgres://user:password@localhost/notes' \
  /usr/local/bin/futo-notes-server migrate-to-sqlite -to sqlite:/srv/futo-notes/notes.db
```

Run the copy as the user the server runs as, or the new file ends up owned by
root and the server cannot open it.

Then point `DATABASE_URL` at the new file and start the server again:

```bash
sudo sed -i 's|^DATABASE_URL=.*|DATABASE_URL=sqlite:/srv/futo-notes/notes.db|' /etc/futo-notes-server.env
sudo systemctl start futo-notes-server
curl --fail http://localhost:3005/health
```

Open an existing note, make an edit, and confirm it reaches another client. To
roll back, stop the server and point `DATABASE_URL` back at the untouched
Postgres database.

### On a Compose stack, by hand

Run the same copy in a one-off container, mounting the target where the
replacement server will look for it. `/data/db/notes.db` is the image default:

```bash
docker compose run --rm \
  --volume "${FUTO_NOTES_DATA_DIR:-./futo-notes-data}/db:/data/db" server \
  futo-notes-server migrate-to-sqlite -to sqlite:/data/db/notes.db
```

This needs your `server` service to already be running the Go image with
`DATABASE_URL` pointing at Postgres. Afterwards, switch to
`docker-compose.production.yml`, whose image default is already
`sqlite:/data/db/notes.db`. Make sure `FUTO_NOTES_DATA_DIR` covers both the
`blobs` and `db` folders, and that nothing else about the blob directory
changed.

## What the copy checks

The `migrate-to-sqlite` command reads one repeatable Postgres snapshot and
copies users, collections, objects, the blob ledger, mutation results,
sessions, and server configuration. It then checks table counts, every
collection's version, ledger bytes, SQLite integrity, and foreign keys, and
prints its table of row counts only after all of them pass. A failed run
deletes the partial SQLite files rather than leaving them for the server to
open.

Blob files are not copied, and `BLOB_DIR` must keep pointing at the same
directory it did before.

## One safety net to know about

If the server ever starts with a brand-new empty database while your notes
folder already has notes in it, it refuses to run and says so. That almost
always means the compose file or `.env` was changed and the server lost track
of its database. Fix the configuration rather than letting devices sync
against an empty vault.
