# Self-hosting plan

Goal: when `go-rewrite` becomes `main`, a self-hoster can install the server
from the website in one command, find a README written for them rather than for
maintainers, and read a short page on notes.futo.tech before committing an
evening to it.

## Why now

The 2026-08-27 merge of `main` into `go-rewrite` (d9e86b0) resolved
`install.sh` and `.env.production.example` as deleted. The website homepage
advertises `curl -fsSL https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/install.sh | sh`
as "Run the server". The moment this branch ships, that URL 404s. The `main`
README's "HTTPS for remote access" section also has no equivalent here.

## Decisions

- Docker Hub (`futotech/notes-server`) is the source of truth for images. No
  GitLab registry fallback in the installer.
- The advertised install URL becomes `https://notes.futo.tech/install-server.sh`, a
  302 redirect on the website to the raw file on GitLab `main`. `curl -fsSL`
  follows it. Retargeting later is a one-line website change. No ordering
  problem: `main` has an `install.sh` today, and the SQLite version replaces it
  at the same URL on merge.
- The release binary stays the secondary path: documented in the README with a
  short systemd unit example, artifacts left in the GitLab package registry and
  linked. No apt/rpm packaging until a self-hoster asks.
- Website MR 2 (`mr/seo-growth`) is not merging soon. Website work branches from
  `main`; MR 2 will need the same two URL string swaps in `index.astro` when it
  rebases.

## Server repo (this branch)

1. **`install.sh`**, rewritten for SQLite. Checks for Docker and Compose v2.
   Prompts (from `/dev/tty`, with env overrides `FUTO_NOTES_DIR`,
   `FUTO_NOTES_PORT`, `FUTO_NOTES_PASSWORD`, `FUTO_NOTES_DATA_DIR`,
   `FUTO_NOTES_IMAGE`, `FUTO_NOTES_COMPOSE_URL`) for the sync password and data
   directory. Writes a private `.env`, fetches `docker-compose.production.yml`,
   runs `up -d`, polls `/health`, prints the URL to paste into the app and a
   pointer to the HTTPS section. No Postgres anywhere.
2. **`.env.production.example`** restored, matching the current compose file's
   variables (`FUTO_NOTES_PASSWORD`, `FUTO_NOTES_DATA_DIR`, `FUTO_NOTES_PORT`,
   `COOKIE_SECURE`, `FUTO_NOTES_IMAGE`).
3. **README split.** README keeps only the self-hoster path: what this is,
   install with the one-liner, manual Compose, the binary plus systemd unit,
   HTTPS for remote access (plain `http://` works on a LAN and the apps accept
   it; Tailscale Funnel or Caddy for remote), connect the app (Settings →
   Self-hosted sync → Server URL), upgrade (`docker compose pull` then
   `up -d`), back up, links to the TypeScript upgrade guide and the
   Postgres-to-SQLite guide. Everything else moves verbatim to
   `docs/DEVELOPMENT.md`.

## Website repo (`../website`, branch from `main`)

1. `public/_redirects`:
   `/install-server.sh  https://gitlab.futo.org/futo-notes/futo-notes-server/-/raw/main/install.sh  302`
2. Swap both hardcoded install URLs in `src/pages/index.astro` (the `<pre>`
   block and `SERVER_CMD`) to `curl -fsSL https://notes.futo.tech/install-server.sh | sh`.
3. New `src/pages/docs/self-hosting.md` on `DocLayout`, same shape as
   `docs/linux-install.md`. Shorter than the README; links to GitLab for the
   rest.
4. Repoint the homepage "View the server on GitLab" button at
   `/docs/self-hosting/`.
5. Bookkeeping from the website CLAUDE.md: `public/llms.txt` Pages list,
   `dist/docs/self-hosting.md` twin exists, `llms-full.txt` has the prose,
   `robots.txt` unchanged.

## Verify after deploy

```sh
curl -sIL https://notes.futo.tech/install-server.sh | grep -iE '^(HTTP|location)'
curl -sI https://notes.futo.tech/docs/self-hosting/ | grep -i link
curl -s https://notes.futo.tech/docs/self-hosting.md | head
```

Then run the one-liner on a clean VM and connect an app to it over `http://`.
