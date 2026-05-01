# FUTO Notes Sync Server

End-to-end encrypted sync server for the [FUTO Notes](https://gitlab.futo.org/futo-notes/futo-notes) notes app. Built with TypeScript + Hono + PostgreSQL. The server stores opaque encrypted blobs and never sees plaintext note content.

See [DESIGN.md](./DESIGN.md) for the architecture and threat model.

## Running from source

Requirements: Node 20+, pnpm 10, Docker (for Postgres).

```bash
pnpm install
docker compose up -d            # Postgres on localhost:5433
cp .env.example .env

# Generate a password hash and paste it into .env as FUTO_NOTES_PASSWORD_HASH
pnpm exec tsx src/index.ts hash <your-password>

pnpm migrate                    # apply DB migrations
pnpm dev                        # http://localhost:3005
```

The default `.env.example` is set up for `AUTH_MODE=password`. For dev/test mode (passwordless `/api/auth/dev/login`), set `AUTH_MODE=dev` and clear `FUTO_NOTES_PASSWORD_HASH`.

### Tests

```bash
pnpm test                       # full suite (dev + password modes)
pnpm test:dev                   # AUTH_MODE=dev only
pnpm test:password              # AUTH_MODE=password only
```

Tests need the Postgres container running (`docker compose up -d postgres`).

### Build

```bash
pnpm build                      # esbuild → dist/index.js (OSS entrypoint)
pnpm build:hosted               # → dist/hosted.js (hosted entrypoint)
```

## Self-hosting with Docker

Requires Docker with the Compose v2 plugin. One command:

```bash
curl -sSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/install.sh | bash
```

The installer asks where to keep your encrypted notes (default: `./futo-notes-data`) and prompts for an admin password, then writes `docker-compose.production.yml` + `.env` into the current directory and starts the stack on `http://localhost:3005`.

Re-run the same command later to upgrade — your password and database credentials in `.env` are reused.

### Options

```bash
curl -sSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/install.sh -o install.sh
bash install.sh --help
```

Flags worth knowing:

- `--data-dir DIR` — where encrypted blobs and Postgres data live (skips the prompt). Back this up to back up your notes.
- `--port N` — host port to expose the server on (default: 3005).
- `--password PW` + `--non-interactive` — for unattended installs.

### Where your data lives

By default, everything sits under `./futo-notes-data/`:

- `futo-notes-data/blobs/` — encrypted note content (opaque to the server)
- `futo-notes-data/postgres/` — Postgres data directory (sync metadata)

Move the directory anywhere by setting `FUTO_NOTES_DATA_DIR` in `.env` and restarting:

```bash
docker compose -f docker-compose.production.yml down
# edit .env: FUTO_NOTES_DATA_DIR=/srv/futo-notes-data
docker compose -f docker-compose.production.yml up -d
```

A snapshot of `$FUTO_NOTES_DATA_DIR` plus `.env` is a complete backup.

### Connect the app

Open FUTO Notes, go to **Settings → Sync**, and enter:

- Server URL: `http://localhost:3005` (or whatever port you exposed)
- Password: the admin password you set during install

### HTTPS for remote access

The server only speaks HTTP internally. To expose it to the internet, put a reverse proxy in front that terminates TLS:

- [Tailscale Funnel](https://tailscale.com/kb/1223/funnel) — simplest if you already use Tailscale
- Caddy — automatic Let's Encrypt certificates
- nginx — classic, most flexibility

## Repo layout

```
src/
  app.ts        # buildApp() factory — shared by both entrypoints
  index.ts      # OSS entrypoint (ships in public Docker image)
  hosted/       # hosted-only middleware + separate entrypoint
  server.ts     # shared lifecycle (runServer, CLI subcommands like `hash`)
  auth/         # session + password-mode login
  blob/         # blob storage abstraction (filesystem for now)
  collections/  # collection API
  objects/      # per-object versioned sync API
  db/           # Kysely connection + migrations
tests/          # integration tests (node:test)
```
