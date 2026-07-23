# FUTO Notes Sync Server

End-to-end encrypted sync server for the [FUTO Notes](https://gitlab.futo.org/futo-notes/futo-notes) notes app. The server stores opaque encrypted blobs and never sees plaintext note content.

- [docs/API.md](./docs/API.md) — client integration guide (endpoints, auth, the sync protocol, SSE)
- [DESIGN.md](./DESIGN.md) — architecture, threat model, and scaling plan

## Self-hosting

The only thing you need installed is **Docker** (with the Compose v2 plugin). You don't need to clone this repo or install any language toolchain — everything runs from a prebuilt image.

The guided installer asks for an admin password and install location, writes a
private configuration file, and starts the server:

```bash
curl -fsSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/install.sh | sh
```

For a manual installation:

```bash
# 1. Make a directory for your install
mkdir my-futo-notes && cd my-futo-notes

# 2. Grab the compose file and a starter .env
curl -sSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/docker-compose.production.yml -o docker-compose.yml
curl -sSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/.env.production.example -o .env

# 3. Edit .env: set FUTO_NOTES_PASSWORD and set POSTGRES_PASSWORD to a
#    strong random string (e.g. `openssl rand -hex 32`).
$EDITOR .env

# 4. Keep the credential file private and start
chmod 600 .env
docker compose up -d
```

The server listens on `http://localhost:3005` (override with `FUTO_NOTES_PORT` in `.env`).

### Connect the app

Open FUTO Notes, go to **Settings → Sync**, and enter:

- Server URL: `http://<server-IP-or-hostname>:3005` (`localhost` only if the app runs on the server)
- Password: the admin password you configured

`FUTO_NOTES_PASSWORD` is stored as plaintext in `.env` for a simple setup, so
protect and back up that file like any other credential. For deployments where
environment/config exposure is a concern, set only `FUTO_NOTES_PASSWORD_HASH`
instead; generate it with `bun dist/index.js hash <password>` inside the server
image. A hash is safer at rest, though a weak password can still be guessed
offline. Setting both alternatives is rejected to avoid ambiguous configuration.

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

`POST /api/auth/password/login` guards the server's single password (and runs scrypt when the hash-based option is configured), so it is a brute-force target. **The server rate-limits this path for you** — by default 10 attempts per minute per client, returning `429` with a `Retry-After` header past that. You don't need to add a proxy rule.

## Development

For working on the server itself. The toolchain is [Bun](https://bun.sh) (`curl -fsSL https://bun.sh/install | bash`) — it's the package manager, TypeScript runtime, and test runner. You also need Docker for Postgres. (Self-hosters don't need any of this — see [Self-hosting](#self-hosting) above.)

```bash
bun install
docker compose up -d            # Postgres on localhost:5433
cp .env.example .env

# Set FUTO_NOTES_PASSWORD in .env. For hash-at-rest, generate a hash instead:
# bun src/index.ts hash <your-password>

bun run migrate                 # apply DB migrations
bun dev                         # http://localhost:3005
```

The default `.env.example` is set up for `AUTH_MODE=password`. For dev/test mode (passwordless `/api/auth/dev/login`), set `AUTH_MODE=dev` and clear both password variables.

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
