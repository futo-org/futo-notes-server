# Stonefruit Sync Server

End-to-end encrypted sync server for the [Stonefruit](https://gitlab.futo.org/stonefruit/stonefruit) notes app. Built with TypeScript + Hono + PostgreSQL. The server stores opaque encrypted blobs and never sees plaintext note content.

See [DESIGN.md](./DESIGN.md) for the architecture and threat model.

## Self-hosting — one command

```bash
curl -sSL https://gitlab.futo.org/stonefruit/stonefruit-server/-/raw/main/install.sh | sh
```

This will:
1. Download the `stonefruit` CLI to `~/.local/bin/`
2. Prompt for a port (default `3005`)
3. Generate `docker-compose.yml` in the current directory
4. Pull the server + Postgres images and start the containers

### After setup

1. **Create your account** — open `http://localhost:3005/start` in your browser and sign up with an email and password. Your credentials stay on your server; nothing is sent anywhere else.
2. **Connect the app** — open Stonefruit on your phone or computer, go to **Settings → Sync**, and enter:
   - Server URL: `http://localhost:3005` (or whatever port you picked)
   - The same email and password
3. **Sync starts automatically** once connected.

### Requirements

- Docker (with `docker compose`)
- Node.js 20+
- `curl`

### Other CLI commands

```bash
stonefruit status     # Check server health
stonefruit update     # Pull latest image and restart
```

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
  auth/         # session + password auth, signup
  blob/         # blob storage abstraction (filesystem for now)
  collections/  # collection API
  objects/      # per-object versioned sync API
  db/           # Kysely connection + migrations
  routes/       # /start signup page
  index.ts      # server entry point
packages/
  cli/          # @futo-notes/cli — self-hosting CLI
tests/          # integration tests (node:test)
install.sh      # one-liner installer for end users
```
