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

- Bigint columns are typed as `string` in the Database interface (pg driver returns bigints as strings)
- Use Kysely's `sql` template tag for expressions: `sql`now()``, `sql`version + 1``
- All queries must be scoped by `user_id` — composite indexes support this
