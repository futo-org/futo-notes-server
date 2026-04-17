# Stonefruit Sync Server

End-to-end encrypted sync server for the [Stonefruit](https://gitlab.futo.org/stonefruit/stonefruit) notes app. Built with TypeScript + Hono + PostgreSQL. The server stores opaque encrypted blobs and never sees plaintext note content.

See [DESIGN.md](./DESIGN.md) for the architecture and threat model.

## Self-hosting — one command

```bash
curl -sSL https://gitlab.futo.org/stonefruit/stonefruit-server/-/raw/main/install.sh | sh
```

This will:
1. Download the `stonefruit` installer binary to `/usr/local/bin/` (a static Go binary, no runtime dependencies)
2. Launch an interactive TUI that asks for a port, a notes storage directory, and an admin password
3. Write `docker-compose.yml` and a sibling `.env` (with the scrypt-hashed password) in the current directory
4. Pull the server + Postgres images and start the containers

### After setup

1. **Connect the app** — open Stonefruit on your phone or computer, go to **Settings → Sync**, and enter:
   - Server URL: `http://localhost:3005` (or whatever port you picked)
   - Password: the admin password you set during install
2. **Sync starts automatically** once connected.

### Requirements

- Docker (with `docker compose`)
- `curl`

### Other CLI commands

```bash
stonefruit status            # Check server health
stonefruit update            # Pull current track and restart
stonefruit release latest    # Switch to main-branch rolling builds
stonefruit release stable    # Switch back to tagged releases (default)
```

New installs default to the `stable` track (tagged releases). `latest` follows `main` — use it to dogfood unreleased fixes.

Run any command with `--help` for the full flag list.

### HTTPS for remote access

The server only speaks HTTP internally. To expose it to the internet, put a reverse proxy in front that terminates TLS:

- [Tailscale Funnel](https://tailscale.com/kb/1223/funnel) — simplest if you already use Tailscale
- Caddy — automatic Let's Encrypt certificates
- nginx — classic, most flexibility

## Running from source

```bash
pnpm install
docker compose up -d       # starts Postgres for local dev
cp .env.example .env
pnpm dev                   # server on http://localhost:3005
```

Tests:

```bash
pnpm test
```

Build a production bundle:

```bash
pnpm build                 # esbuild → dist/index.js
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
installer/      # Go + Bubble Tea `stonefruit` CLI (static binary)
tests/          # integration tests (node:test)
install.sh      # one-liner installer for end users
```
