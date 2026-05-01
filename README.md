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

Requires Docker with the Compose v2 plugin.

```bash
# 1. Make a directory for your install
mkdir my-futo-notes && cd my-futo-notes

# 2. Grab the compose file and a starter .env
curl -sSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/docker-compose.production.yml -o docker-compose.yml
curl -sSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/.env.production.example -o .env

# 3. Generate an admin password hash
docker run --rm \
  gitlab.futo.org:5050/futo-notes/futo-notes-server/server:stable \
  node dist/index.js hash 'your-admin-password'

# 4. Edit .env: paste the hash from step 3 into FUTO_NOTES_PASSWORD_HASH,
#    and set POSTGRES_PASSWORD to a strong random string (e.g. `openssl rand -hex 32`).
$EDITOR .env

# 5. Start
docker compose up -d
```

The server listens on `http://localhost:3005` (override with `FUTO_NOTES_PORT` in `.env`).

### Day-to-day

```bash
docker compose ps
docker compose logs -f
docker compose pull && docker compose up -d   # upgrade
docker compose down                            # stop (data is preserved)
```

### Where your data lives

Encrypted blobs and Postgres data are bind-mounted from `./futo-notes-data/` next to your `.env` (override with `FUTO_NOTES_DATA_DIR`):

- `futo-notes-data/blobs/` — encrypted note content (opaque to the server)
- `futo-notes-data/postgres/` — Postgres data directory (sync metadata)

A snapshot of `$FUTO_NOTES_DATA_DIR` plus `.env` is a complete backup.

### Connect the app

Open FUTO Notes, go to **Settings → Sync**, and enter:

- Server URL: `http://localhost:3005` (or whatever port you exposed)
- Password: the admin password you hashed above

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
