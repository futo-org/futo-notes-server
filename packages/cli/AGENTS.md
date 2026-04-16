# Self-hosting CLI (`@futo-notes/cli`)

Commander.js CLI for self-hosting stonefruit-server via Docker Compose.

## Commands

```bash
pnpm dev    # Run CLI locally (tsx)
pnpm build  # Bundle → dist/cli.js (single ESM file with shebang)
```

## Conventions

- One file per command in `src/commands/`
- Interactive prompts through `src/lib/prompt.ts`
- Docker operations through `src/lib/docker.ts` (shells out to `docker compose`)
- User config stored at `~/.config/stonefruit/config.json`

## Build & distribution

Bundled with esbuild to a single ESM file. Output gets `chmod +x` and a node shebang. Distributed via GitLab generic package registry, downloaded by `install.sh` at the repo root.
