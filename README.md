# FUTO Notes Sync Server

End-to-end encrypted sync server for the [FUTO Notes](https://gitlab.futo.org/futo-notes/futo-notes) notes app. The server stores opaque encrypted blobs and never sees plaintext note content.

- [docs/API.md](./docs/API.md) — client integration guide (endpoints, auth, the sync protocol, SSE)
- [DESIGN.md](./DESIGN.md) — architecture, threat model, and scaling plan

## Self-hosting

The only thing you need installed is **Docker** (with the Compose v2 plugin). You don't need to clone this repo or install any language toolchain — everything runs from a prebuilt image.

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

### Connect the app

Open FUTO Notes, go to **Settings → Sync**, and enter:

- Server URL: `http://localhost:3005` (or whatever address you exposed)
- Password: the admin password you hashed above

### Day-to-day

```bash
docker compose ps                              # status
docker compose logs -f                         # tail logs
docker compose pull && docker compose up -d    # upgrade to the latest image
docker compose down                            # stop (data is preserved)
```

### Where your data lives

Encrypted blobs and Postgres data are bind-mounted from `./futo-notes-data/` next to your `.env` (override with `FUTO_NOTES_DATA_DIR`):

- `futo-notes-data/blobs/` — encrypted note content (opaque to the server)
- `futo-notes-data/postgres/` — Postgres data directory (sync metadata)

A snapshot of `$FUTO_NOTES_DATA_DIR` plus `.env` is a complete backup.

### HTTPS for remote access

The server only speaks HTTP internally. To expose it to the internet, put a reverse proxy in front that terminates TLS:

- [Tailscale Funnel](https://tailscale.com/kb/1223/funnel) — simplest if you already use Tailscale
- Caddy — automatic Let's Encrypt certificates
- nginx — classic, most flexibility

### Login rate limiting

`POST /api/auth/password/login` runs scrypt (tens of ms, ~16 MB) on every request and guards a single guessable password, so it's both a brute-force target and a CPU-amplification vector. **The server rate-limits this path for you** — by default 10 attempts per minute per client, returning `429` with a `Retry-After` header past that. You don't need to add a proxy rule.

If you put the server behind a reverse proxy (which you do for HTTPS, above), set `TRUST_PROXY=true` in `.env` so the limit keys on the real client IP from `X-Forwarded-For` rather than the proxy's address — and make sure the proxy sets that header. Leave it off when the server is exposed directly, since an untrusted `X-Forwarded-For` can be spoofed to dodge the limit.

Tune it with `AUTH_RATE_LIMIT` (attempts per window; `0` disables) and `AUTH_RATE_LIMIT_WINDOW_MS`. The limit is per-instance and in-memory, which is all the single-instance self-hosted setup needs; a proxy-level limit is still fine as defense-in-depth if you want one.

## Development

For working on the server itself. The toolchain is [Bun](https://bun.sh) (`curl -fsSL https://bun.sh/install | bash`) — it's the package manager, TypeScript runtime, and test runner. You also need Docker for Postgres. (Self-hosters don't need any of this — see [Self-hosting](#self-hosting) above.)

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

```bash
bun run test                    # full suite (needs Postgres running)
bun run build                   # esbuild → dist/index.js
```

The stack is TypeScript + Hono + PostgreSQL (Kysely). See [DESIGN.md](./DESIGN.md) for the architecture and [AGENTS.md](./AGENTS.md) for conventions.

### Repo layout

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
