# FUTO Notes server

Self-hosted encrypted sync for FUTO Notes. Your notes are encrypted on your
device before they are uploaded, so this server stores opaque encrypted blobs
and never sees their contents. It is single-user: one sync password, one vault,
no accounts to manage. New installs need no database server, only a directory on
disk. This implementation is wire- and storage-compatible with the previous
TypeScript server, so existing clients and existing data keep working.

## Install with one command

```bash
curl -fsSL https://notes.futo.tech/install-server.sh | sh
```

The script checks that Docker and the Compose v2 plugin are present, writes a
private `.env` and a `docker-compose.yml` into `~/futo-notes`, and starts the
server. It then waits for the server to report healthy and prints the URL to
paste into the app.

It asks you for three things: the install directory (`~/futo-notes` by
default), the port to expose (3005 by default), and the sync password you will
enter in the app. Set `FUTO_NOTES_PASSWORD`, `FUTO_NOTES_DIR`,
`FUTO_NOTES_PORT`, or `FUTO_NOTES_DATA_DIR` in the environment to skip the
prompts and run it unattended.

## Install with Docker Compose by hand

Download the compose file and the example environment file, fill in the
password, and start the server:

```bash
mkdir -p ~/futo-notes && cd ~/futo-notes
curl -fsSLO https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/docker-compose.production.yml
curl -fsSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/.env.production.example -o .env
chmod 600 .env
$EDITOR .env
docker compose -f docker-compose.production.yml up -d
curl --fail http://localhost:3005/health
```

`.env` needs at least a sync password, and an absolute data directory if you do
not want it beside the compose file:

```dotenv
FUTO_NOTES_PASSWORD=replace-with-your-sync-password
FUTO_NOTES_DATA_DIR=/absolute/path/to/futo-notes-data
```

SQLite metadata and encrypted blobs both live under `FUTO_NOTES_DATA_DIR`.

## Run a release binary instead

Download a binary for your platform from the
[package registry](https://gitlab.futo.org/futo-notes/futo-notes-server/-/packages).
Each release tag publishes Linux amd64/arm64, macOS amd64/arm64, and Windows
amd64 binaries plus a SHA-256 checksum file.

New installs do not need a database server or `DATABASE_URL`. Set one password
variable and an absolute `BLOB_DIR`; SQLite defaults to `./data/notes.db`:

```bash
FUTO_NOTES_PASSWORD='sync password' \
BLOB_DIR='/srv/futo-notes/blobs' \
PORT=3005 \
./futo-notes-server
```

Run the process from a stable working directory, or explicitly set an absolute
SQLite URL such as `DATABASE_URL=sqlite:/srv/futo-notes/notes.db`.

As a safety check, the server refuses to create a new SQLite file when
`BLOB_DIR` already contains blob files; this usually means an existing install
lost its `DATABASE_URL`. Recover the intended configuration instead of syncing
against an empty vault. `ALLOW_FRESH_DATABASE=true` is available only for an
intentional fresh database beside pre-existing blob files.

To keep it running, put the configuration in `/etc/futo-notes-server.env`
(mode 600, owned by root) and install a unit at
`/etc/systemd/system/futo-notes-server.service`:

```ini
[Unit]
Description=FUTO Notes sync server
After=network-online.target
Wants=network-online.target

[Service]
User=futo-notes
Group=futo-notes
EnvironmentFile=/etc/futo-notes-server.env
WorkingDirectory=/srv/futo-notes
ExecStart=/usr/local/bin/futo-notes-server
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Create the user and directory first, then enable it:

```bash
sudo useradd --system --home /srv/futo-notes --shell /usr/sbin/nologin futo-notes
sudo install -d -o futo-notes -g futo-notes /srv/futo-notes
sudo systemctl enable --now futo-notes-server
```

## Connect the app

In FUTO Notes, go to Settings, then Self-hosted sync, and set the Server URL to
your server, for example `http://192.168.1.10:3005`. Sign in with the sync
password from your `.env`. The Android and iOS apps accept plain `http://` URLs,
so a server on your own network needs no certificate.

## HTTPS for remote access

The server speaks plain HTTP. To reach it from outside your own network, put a
TLS reverse proxy in front of it. Two easy options:

- [Tailscale Funnel](https://tailscale.com/kb/1223/funnel) publishes the server
  on a `*.ts.net` hostname with a certificate, without opening a port on your
  router.
- [Caddy](https://caddyserver.com) gets a certificate itself. The whole
  Caddyfile is three lines:

  ```caddyfile
  notes.example.com {
      reverse_proxy localhost:3005
  }
  ```

The login endpoint is rate-limited to 10 attempts per minute per client address.
Behind a reverse proxy the server sees only the proxy's address, so every client
shares one bucket; keep that in mind if several devices sign in at once.

## Upgrade

With Docker Compose, from your install directory:

```bash
docker compose pull && docker compose up -d
```

If you installed by hand and kept the file named
`docker-compose.production.yml`, add `-f docker-compose.production.yml` to both
commands.

With a release binary, replace the file and restart the service:

```bash
sudo systemctl restart futo-notes-server
```

## Back up

The data directory is the whole server. Stop the server, copy it, start again:

```bash
docker compose -f docker-compose.production.yml stop server
cp -a /absolute/path/to/futo-notes-data /absolute/path/to/futo-notes-data.backup
docker compose -f docker-compose.production.yml start server
```

Back up `.env` alongside it, since it holds your sync password.

## Existing TypeScript installations

Existing TypeScript installations must first follow
[the TypeScript-to-Go upgrade guide](docs/UPGRADING_FROM_TYPESCRIPT.md). They
continue using Postgres. Afterward, they can optionally follow
[Switching from Postgres to SQLite](docs/Switching%20from%20Postgres%20to%20SQLite.md).

---

Working on the server itself? See [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).
