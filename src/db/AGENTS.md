# Database

PostgreSQL 16 via Kysely (type-safe query builder, no ORM).

## Migrations

IMPORTANT: Every new migration needs TWO things:
1. A numbered file in `migrations/` exporting `up(db)` and `down(db)`
2. A corresponding entry in `migration-registry.ts` — the production esbuild bundle cannot discover migration files from the filesystem, so it falls back to this static registry

Numbering is sequential: `001_`, `002_`, etc.

## Schema types

`types.ts` defines the `Database` interface for Kysely. Update it whenever you change the schema — queries won't type-check otherwise.

## Conventions

- Bigint columns are typed as `string` in the Database interface (pg driver returns bigints as strings). That's a storage-layer fact only — it says nothing about the wire format. Server-computed response scalars (cursors, versions the server derives) are converted to `Number`; object row fields serialized straight from a query (`version`, `change_seq`, `size_bytes`) stay strings on the wire. Both forms coexist by design, so pick one deliberately for any new response field rather than assuming a blanket conversion.
- Use Kysely's `sql` template tag for expressions: `sql`now()``, `sql`version + 1``
- All queries serving an authenticated request must be scoped by `user_id` — composite indexes support this. Exception: scheduled background maintenance (`src/maintenance/sessionReaper.ts`, blob ledger/storage reconciliation and mutation-result expiry in `src/collection-contents/index.ts`) runs with no auth context and deliberately queries across all users, reclaiming rows by timestamp/state instead. It returns no data to any request; see `AGENTS.md` (repo root) and DESIGN.md §Statelessness & scaling for why this doesn't weaken the invariant for request-handling code, and what it costs at Stage 3 (sharded).
