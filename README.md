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

The server ships as a Docker image. A starting-point `docker-compose.production.yml` is included.

```bash
# 1. Generate the admin password hash
docker run --rm \
  gitlab.futo.org:5050/futo-notes/futo-notes-server/server:stable \
  node dist/index.js hash <your-password>

# 2. Create a sibling .env with:
#      POSTGRES_PASSWORD=<a strong random string>
#      FUTO_NOTES_PASSWORD_HASH=<the hash from step 1>
#      FUTO_NOTES_PORT=3005          # optional, defaults to 3005

# 3. Start the stack
docker compose -f docker-compose.production.yml up -d
```

The server binds to `localhost:3005` (or whatever you set `FUTO_NOTES_PORT` to). Postgres data lives in the `pg-data` Docker volume; encrypted blobs in `blob-data`.

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
