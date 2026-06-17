# FUTO Notes Sync Server

End-to-end encrypted sync server for the [FUTO Notes](https://gitlab.futo.org/futo-notes/futo-notes) notes app. Built with TypeScript + Hono + PostgreSQL. The server stores opaque encrypted blobs and never sees plaintext note content.

- [docs/API.md](./docs/API.md) — client integration guide (endpoints, auth, the sync protocol, SSE)
- [DESIGN.md](./DESIGN.md) — architecture, threat model, and scaling plan

## Running from source

Requirements: Bun 1.3+, Docker (for Postgres).

```bash
bun install
docker compose up -d            # Postgres on localhost:5433
cp .env.example .env

# Generate a password hash and paste it into .env as FUTO_NOTES_PASSWORD_HASH
bun src/index.ts hash <your-password>

bun run migrate                 # apply DB migrations
bun dev                         # http://localhost:3005
```

The default `.env.example` is set up for `AUTH_MODE=password`. For dev/test mode (passwordless `/api/auth/dev/login`), set `AUTH_MODE=dev` and clear `FUTO_NOTES_PASSWORD_HASH`.

### Tests

```bash
bun run test                    # full suite (dev + password modes)
bun run test:dev                # AUTH_MODE=dev only
bun run test:password           # AUTH_MODE=password only
```

Tests need the Postgres container running (`docker compose up -d postgres`).

### Build

```bash
bun run build                   # esbuild → dist/index.js (OSS entrypoint)
bun run build:hosted            # → dist/hosted.js (hosted entrypoint)
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
  bun dist/index.js hash 'your-admin-password'

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

### Rate-limit the auth path

`POST /api/auth/password/login` runs scrypt (tens of ms, ~16 MB) on every unauthenticated request, and there is exactly one admin password to guess. The server does not throttle this itself — your reverse proxy MUST rate-limit `/api/auth/` to blunt online guessing and CPU-amplification.

nginx — declare a zone and apply it to the auth location:

```nginx
limit_req_zone $binary_remote_addr zone=auth:10m rate=5r/m;

location /api/auth/ {
    limit_req zone=auth burst=5 nodelay;
    proxy_pass http://localhost:3005;
}
```

Caddy has no built-in rate limiter; install the [`caddy-ratelimit`](https://github.com/mholt/caddy-ratelimit) plugin (build with `xcaddy build --with github.com/mholt/caddy-ratelimit`), then:

```caddy
example.com {
    rate_limit {
        zone auth {
            match path /api/auth/*
            key {remote_host}
            events 5
            window 1m
        }
    }
    reverse_proxy localhost:3005
}
```

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
tests/          # integration tests (bun:test)
```
