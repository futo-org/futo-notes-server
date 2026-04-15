# Stonefruit Sync Server

End-to-end encrypted sync server for the [Stonefruit](https://gitlab.futo.org/stonefruit/stonefruit) notes app. Built with TypeScript + Hono + PostgreSQL. The server stores opaque encrypted blobs and never sees plaintext note content.

See [DESIGN.md](./DESIGN.md) for the architecture and threat model.

## Self-hosting

The easiest way to run your own server is via the CLI. It generates a `docker-compose.yml` for you, pulls the server image, starts everything, and sets your admin password.

### Requirements

- Docker (with `docker compose`)
- Node.js 24+

### Install and run

```bash
# Option 1 — one-shot via npx (no install)
npx @futo-notes/cli setup

# Option 2 — install globally
npm install -g @futo-notes/cli
stonefruit setup
```

The setup command will:

1. Prompt for a port (default `3000`) and admin password
2. Generate `docker-compose.yml` in the current directory
3. Pull the server + Postgres images
4. Start the containers and wait for health
5. Set the initial password and print your admin token

**Save the admin token.** You need it to reset the password later — it's only shown once.

### Non-interactive setup

For scripts or first-boot provisioning:

```bash
stonefruit setup --port 3000 --password 'your-password-here' --yes

# Or via stdin (avoids password in shell history)
echo 'your-password-here' | stonefruit setup --password-stdin --yes
```

### Other commands

```bash
stonefruit status                          # Check server health
stonefruit update                          # Pull latest image and restart
stonefruit reset-password --admin-token <token>   # Change password
```

Run any command with `--help` for the full flag list.

### HTTPS

The server only speaks HTTP internally. Expose it to the internet via a reverse proxy that terminates TLS — [Tailscale Funnel](https://tailscale.com/kb/1223/funnel), Caddy, or nginx all work.

## Running from source

```bash
pnpm install
docker compose up -d                    # starts Postgres for local dev
cp .env.example .env
pnpm dev                                # server on http://localhost:3000
```

Tests:

```bash
pnpm test
```

Build a production bundle:

```bash
pnpm build                              # esbuild → dist/index.js
```

## Repo layout

```
src/
  auth/         # session + password auth
  blob/         # blob storage abstraction (filesystem for now)
  collections/  # collection API
  objects/      # per-object versioned sync API
  db/           # Kysely connection + migrations
  index.ts      # server entry point
packages/
  cli/          # @futo-notes/cli — self-hosting CLI
tests/          # integration tests (node:test)
```
