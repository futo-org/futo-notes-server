# Upgrade from the TypeScript server

The Go server is a drop-in replacement for the latest TypeScript server. It
uses the same PostgreSQL schema, Kysely migration history, session tokens, blob
layout, password hashes, and HTTP API.

## Before upgrading

1. Stop the TypeScript server so the database and blob directory cannot change
   during the backup.
2. Back up PostgreSQL and the entire blob directory. For a Compose install,
   also back up `.env`:

   ```bash
   docker compose stop server
   docker compose exec -T postgres pg_dump -U futo_notes -d futo_notes -Fc > futo-notes-before-go.dump
   cp -a /absolute/path/to/blobs /absolute/path/to/blobs.before-go
   cp .env .env.before-go
   ```

3. Record the exact TypeScript image tag you are replacing.

Do not proceed without both the database and blob backup. PostgreSQL metadata
and blob bytes are one logical data set.

## Cut over

Use the same `DATABASE_URL`, authentication setting, password or password hash,
and blob directory. Set `BLOB_DIR` to an absolute path for a binary install. Its
default (`./blobs`) is relative to the process working directory in both
servers, which makes an otherwise-correct upgrade appear to lose blob files.

For Compose installs, replace the compose file with
`docker-compose.postgres.yml` from this release and then run:

```bash
docker compose pull server
docker compose up -d
docker compose logs -f server
```

Wait for `GET /health` to return HTTP 200, then connect one existing client and
confirm that an existing note opens and a new edit syncs to a second client.
The Go server applies only missing migrations at boot. A database already used
by the latest TypeScript server needs no migration.

## Environment changes

| TypeScript variable | Go behavior / replacement |
| --- | --- |
| `DATABASE_URL` | Honored. Keep the existing Postgres URL during this upgrade; pgx accepts TLS options in it. |
| `PORT`, `AUTH_MODE`, `FUTO_NOTES_PASSWORD`, `FUTO_NOTES_PASSWORD_HASH`, `COOKIE_SECURE`, `BLOB_DIR`, `DB_POOL_MAX`, `DB_POOL_IDLE_TIMEOUT_MS` | Honored. |
| `BLOB_GC_ENABLED` | Honored. Set exactly `false` to disable destructive blob garbage collection; reconciliation and non-blob expiry still run. |
| `DB_SSL=true` | Add `sslmode=require` (or `verify-full`) to `DATABASE_URL`. |
| `DB_SSL_CA=/path/ca.pem` | Add `sslrootcert=/path/ca.pem` to `DATABASE_URL`. |
| `DB_SSL_INSECURE=true` | Use `sslmode=require`. Prefer certificate verification whenever possible. |
| `TRUST_PROXY` | Dropped. Login rate limiting uses the direct peer address. Behind a reverse proxy, attempts share the proxy's 10-per-60-second bucket. |
| `AUTH_RATE_LIMIT`, `AUTH_RATE_LIMIT_WINDOW_MS` | Dropped; fixed at 10 attempts per 60 seconds. |
| `MAX_BLOB_BYTES` | Dropped; fixed at 100 MiB per upload. Existing larger blobs remain downloadable. |
| `MAX_BATCH_BYTES` | Dropped; fixed at 32 MiB. |
| `BLOB_GC_INTERVAL_MS` | Dropped; maintenance uses the built-in hourly/six-hourly schedules. |
| `BLOB_RETENTION_DAYS` | Dropped; retained merge ancestors use the fixed 365-day policy. |
| `LOG_LEVEL` | Dropped; the Go server currently uses its built-in log level. |

At boot the Go server warns once for every recognized dropped variable that is
still set, including its replacement.

### PostgreSQL TLS URL examples

```text
postgres://user:pass@db.example/notes?sslmode=require
postgres://user:pass@db.example/notes?sslmode=verify-full&sslrootcert=/etc/ssl/private/db-ca.pem
```

## Roll back

The rollback target is the **latest TypeScript server**, not necessarily the old
image you upgraded from.

1. Stop the Go server.
2. If Go applied no migrations, start the latest TypeScript image against the
   same database and blob directory. Writes made through Go remain readable and
   syncable by TypeScript.
3. If the Go startup log says it applied migrations 009–011, an older
   TypeScript image will refuse the now-newer Kysely migration history. Use the
   latest TypeScript image, or restore both the pre-upgrade database and blob
   backups together.

The automated `scripts/adoption-rehearsal.sh all` gate exercises both rollback
paths: a current TypeScript database (zero migrations) and a historical
migration-008 database (exactly migrations 009–011).
