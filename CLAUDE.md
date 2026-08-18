This is a rethink of the FUTO Notes server, which was previously written in TypeScript.

Migration plan is at `docs/Rewriting the server in Go.md`.
OpenAPI spec for the old server lives at `docs/openapi.yaml`.

To the client, this rewrite should be invisible. It is ok for this rewrite to result in different internal behavior, but old contracts need to be honored.

- This is a rethinking from spec, not a port. Don't read the TypeScript server on `main` as a template.
- Contract facts come from the migration plan and OpenAPI spec. If a detail is missing from both, ask — don't inherit TS behavior silently.
- Build only what the current request needs. No speculative helpers, config, or error paths.
