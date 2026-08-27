# Resource comparison: TypeScript + Bun + Postgres vs Go + SQLite

Both servers were run on the same machine, against the same synthetic sync
workload, with the same auth mode (`AUTH_MODE=dev`, so no password hashing cost
lands on either side).

- Old stack: `main` @ `079f136`, `bun dist/index.js` (Bun 1.3.14) + `postgres:16`
  in Docker.
- New stack: `go-rewrite` @ `9637e9c`, a single static binary, SQLite via
  `modernc.org/sqlite` (no cgo).
- Host: Linux 7.1.8, 32 cores, 125 GB RAM.
- Load generator: a Go program driving `N` virtual clients through
  login → claim collection → then repeatedly: upload 4 KB ciphertext, create
  object, re-upload ciphertext, PUT next version, incremental pull, download
  ciphertext. Six requests per sync round, zero errors in every run reported
  here.
- App memory is `VmRSS` from `/proc`. Postgres memory and CPU are the container
  cgroup's `memory.current` and `cpu.stat`, so its background workers are
  counted.

## Idle — the number a self-hoster actually pays for

A personal sync server spends nearly all its life idle.

| | TypeScript + Postgres | Go + SQLite |
| --- | --- | --- |
| Resident memory, idle | 78 MB app + 142 MB Postgres = **220 MB** | **16 MB** |
| CPU over 60 s idle | 0.06 s app + 1.01 s Postgres = **1.07 s** (1.8% of one core) | **0.00 s** (below the 10 ms clock tick) |
| Processes to supervise | 2 | 1 |

Idle memory drops **14x**. Idle CPU goes from a steady 1.8% of a core — almost
all of it Postgres background work — to nothing measurable.

## Startup and deployment

| | TypeScript + Postgres | Go + SQLite |
| --- | --- | --- |
| Cold start to `/health` 200 | 3583 ms (Postgres accepting connections) + 86 ms (app) ≈ **3.7 s** | **6 ms**, including creating and migrating a new database |
| Container images to pull | 180 MB app + 451 MB `postgres:16` = **631 MB** | **107 MB** |
| Self-contained binary | n/a (needs the 89 MB Bun runtime + 51 MB `node_modules`) | **15.5 MB**, statically linked |

## Light load — two clients, 3604 requests

This is the realistic shape for a self-hoster: one or two devices syncing.

| | TypeScript + Postgres | Go + SQLite |
| --- | --- | --- |
| p50 latency | 5.303 ms | **0.366 ms** |
| p95 latency | 6.782 ms | **1.724 ms** |
| p99 latency | 10.203 ms | **3.953 ms** |
| Throughput | 484 req/s | **2738 req/s** |
| CPU per request | 1.044 ms (2.28 s app + 1.48 s Postgres) | **0.561 ms** |
| Peak resident memory | 112 MB app + 188 MB Postgres = 300 MB | **28 MB** |

p50 latency improves **14.5x**, and each request costs **46% less CPU**.

## Saturation — sixteen clients, 19232 requests

| | TypeScript + Postgres | Go + SQLite |
| --- | --- | --- |
| Wall time | 31.67 s | **5.94 s** |
| Throughput | 607 req/s | **3236 req/s** |
| p50 / p95 / p99 | 17.0 / 57.4 / 62.2 ms | **3.55 / 11.0 / 22.1 ms** |
| CPU per request | 0.869 ms (9.63 s app + 7.09 s Postgres) | **0.582 ms** |
| Peak resident memory | 210 MB app + 189 MB Postgres = **399 MB** | **28 MB** |

Under load the old stack's memory grows 1.8x from idle and never fully returns
(90–115 MB app, 175–181 MB Postgres after the load stopped). The Go server's
peak is 28 MB and it stays there.

## Storage

Same workload, near-identical row counts (3869 objects on Postgres, 3840 on
SQLite; the small difference is warm-up traffic).

| | TypeScript + Postgres | Go + SQLite |
| --- | --- | --- |
| Metadata on disk | 14 MB | **5.9 MB** (after WAL checkpoint) |
| Backup procedure | `pg_dump` plus the blob directory | stop, copy one directory, start |

## What this means

A Raspberry Pi with 512 MB of RAM could not comfortably host the old stack: 220
MB resident before a single request, 631 MB of images to pull, and a database
server to keep alive and back up separately. The Go server idles at 16 MB, ships
as one 15.5 MB binary, starts in 6 milliseconds, and keeps everything — metadata
and encrypted blobs — under a single directory you can copy.

It is also faster where it matters: a device syncing against it sees p50
latencies of 0.4 ms instead of 5.3 ms, while using less than half the CPU per
request.

## Which change did what: Go, or SQLite?

The comparison above changes two things at once — language and database. To
separate them, the Go server was run a third way, against the same Postgres
container using its Postgres migrations (`DATABASE_URL=postgres://...`), on the
same workload.

### Latency and throughput: that is SQLite, not Go

| Two clients, 3604 requests | p50 | p95 | p99 | Throughput |
| --- | --- | --- | --- | --- |
| TypeScript + Postgres | 5.303 ms | 6.782 ms | 10.203 ms | 484 req/s |
| **Go + Postgres** | **5.327 ms** | **6.604 ms** | **6.900 ms** | **507 req/s** |
| Go + SQLite | 0.366 ms | 1.724 ms | 3.953 ms | 2738 req/s |

| Sixteen clients, 19232 requests | p50 | p95 | p99 | Throughput |
| --- | --- | --- | --- | --- |
| TypeScript + Postgres | 16.973 ms | 57.381 ms | 62.232 ms | 607 req/s |
| **Go + Postgres** | **17.234 ms** | **56.851 ms** | **65.596 ms** | **643 req/s** |
| Go + SQLite | 3.550 ms | 11.038 ms | 22.122 ms | 3236 req/s |

Rewriting in Go bought **essentially no latency** — 5.327 ms against 5.303 ms,
and 17.2 ms against 17.0 ms. Postgres is the wall in both cases, and the language
in front of it barely matters.

The reason is that the requests were never CPU-bound. At two clients, TypeScript
+ Postgres burned 1.044 ms of CPU per request across both processes while taking
5.303 ms of wall time — **80% of the latency was waiting**, not computing: a TCP
round-trip to another process for each of the several queries a request makes,
plus connection-pool acquisition and query parse and plan on the far side. SQLite
runs in the same process, so a query is a function call and a page-cache read.
Go + SQLite spends 0.561 ms of CPU per request against a 0.366 ms p50 — CPU time
exceeds median latency, which is only possible because the work is genuinely
parallel across cores with almost nothing blocking.

This is also why the win is so large for *this* workload specifically: many tiny
queries, where per-round-trip overhead dominates. A workload built on a few large
analytical queries would not improve this way, and would likely be worse.

### CPU and memory: that is Go

| Two clients, 3604 requests | App CPU per request | App peak RSS |
| --- | --- | --- |
| TypeScript + Postgres | 0.633 ms | 112.3 MB |
| **Go + Postgres** | **0.269 ms** | **21.8 MB** |

| Sixteen clients, 19232 requests | App CPU per request | App peak RSS |
| --- | --- | --- |
| TypeScript + Postgres | 0.501 ms | 210.0 MB |
| **Go + Postgres** | **0.305 ms** | **27.7 MB** |

Holding the database constant, the Go process uses **2.35x less CPU** at two
clients and **1.64x less** at sixteen, in **7.6x less memory**. Combined CPU
across app and database lands at 0.510 ms per request for Go + Postgres and
0.560 ms for Go + SQLite, against 1.044 ms for TypeScript + Postgres — so the
CPU saving is the rewrite, and it is roughly the same whichever database sits
behind it.

### Summary

- **Go** cut CPU per request roughly in half and dropped the process memory floor
  from ~78 MB to ~16 MB.
- **SQLite** cut latency 14x and removed Postgres's 142 MB and its 1.8%-of-a-core
  idle cost entirely.
- The headline resource numbers need both, but the *speed* headline is SQLite's.
