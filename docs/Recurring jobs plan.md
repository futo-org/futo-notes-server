# Building out the recurring jobs

Plan for the four background jobs on two timers — the last server feature in
the buildout before the comparison harness. Nothing here is client-visible;
the contract is the "Recurring Jobs" section of `Rewriting the server in
Go.md` (retention windows, timer cadence, the 500-row reconciliation cap).
Everything else — SQL shape, package layout, ordering of file vs. row
deletion — is ours to choose, and this doc chooses it.

## What the plan requires

Two timers, both firing first at 60 seconds after boot:

- **Timer 1** — then every hour: session reaper.
- **Timer 2** — then every 6 hours: storage reconciliation, mutation-results
  expiry, blob garbage collection.

Retention rules, verbatim from the migration plan:

- Expired sessions are deleted (the auth path only deletes them lazily, when
  an expired token is presented).
- Reconciliation adopts on-disk files with no `blob_ledger` row as `staged`,
  capped at **500 per run**, so a Postgres rollback can't strand bytes forever.
- Mutation results: `pending` rows expire after **24 hours**, successful
  creates are **never** expired, everything else goes after **30 days**.
- Blob GC purges: `staged` not claimed within **24 hours**, `retained`
  (superseded by an update) after **365 days**, and `purgeable` (collection
  was deleted) immediately.

Two states GC must never touch: `claimed` (live) and `legacy_shared`
(migration 010 explicitly marks these never eligible for cleanup).

## Design

One new package, `internal/jobs`: four job functions plus a small runner that
owns the two tickers. Each job takes `(ctx, *sql.DB)` — GC and reconciliation
also take the `*blobs.Store` — and returns counts of what it did, so the same
functions serve the timers, the tests, and the dev UI trigger without a
wrapper. Started from `main()` after migrations, alongside the SSE listener.

The runner is deliberately dumb: `time.Ticker` per timer, initial 60s delay
via the same loop, each firing runs its jobs sequentially and logs one line
per job with the counts (`sessions: reaped 3`). A job error is logged and
does not stop the timer. Intervals are parameters to the runner (so tests can
compress them), not env config — the plan fixes the cadence, nobody needs to
tune it.

All four jobs are safe to run concurrently on multiple servers sharing one
Postgres (the SSE design already assumes that topology): every step below is
either idempotent or row-locked with `SKIP LOCKED`.

### Session reaper — Timer 1

```sql
DELETE FROM sessions WHERE expires_at < now()
```

One statement, nothing to coordinate. Report rows deleted.

### Storage reconciliation — Timer 2

Walk `BLOB_DIR` (`filepath.WalkDir`), slash-normalize each file's relative
path into a candidate blob key, and for keys with no `blob_ledger` row insert:

```sql
INSERT INTO blob_ledger (blob_key, user_id, size_bytes, state)
VALUES ($1, $2, $3, 'staged')
ON CONFLICT (blob_key) DO NOTHING
```

Stop after 500 inserts; log when the cap is hit so a big backlog is visible
rather than silently spread across runs.

Details that matter:

- `blob_ledger.user_id` is NOT NULL with an FK to `users`. Keys are
  `{userId}/{uuid}`, so the first path segment is the owner — but a file
  under a directory that isn't a valid, existing user id can't get a row.
  Skip and log those (count them in the result); they're junk we can't
  adopt, not an error.
- `ON CONFLICT DO NOTHING` covers the race with a concurrent `Stage()`:
  Stage writes the ledger row *before* the file, so any file Stage produced
  already has a row by the time it's on disk, and the walk skips it.
- The insert's `state_changed_at` defaults to `now()`, so an adopted file
  gets a full 24 hours before GC eats it — matching the plan's intent that
  adopted strays flow into the normal staged-expiry path.

### Mutation-results expiry — Timer 2

Two DELETEs. Classification keys off `result->>'status'`, which exists in
three shapes (see `decodeStoredResult` in `internal/objects/objects.go`):
the Go writer's wrapped `{"status": <int>, "body": ...}`, the TS server's
legacy `{"status": "pending"}`, and legacy unwrapped bodies with no status
field at all (which decode as errors only when `result->>'error'` is one of
three known strings, otherwise as a 200).

```sql
-- pending, older than 24 hours
DELETE FROM mutation_results
WHERE created_at < now() - interval '24 hours'
  AND result->>'status' IN ('102', 'pending');

-- everything that is not pending and not a successful create, older than 30 days
DELETE FROM mutation_results
WHERE created_at < now() - interval '30 days'
  AND result->>'status' NOT IN ('102', 'pending')
  AND NOT (kind = 'create' AND (
        result->>'status' ~ '^2'                    -- wrapped 2xx
        OR (result->>'status' IS NULL               -- legacy unwrapped success
            AND (result->>'error' IS NULL OR result->>'error' NOT IN
                 ('not found', 'version conflict', 'blob is not staged',
                  'Mutation-Id reused for different intent')))
  ));
```

The second predicate mirrors `decodeStoredResult`'s fallback mapping exactly;
keep them in sync (a shared comment pointing each at the other). The bias is
conservative: an ambiguous row that might be a successful create is kept,
because wrongly deleting one turns a client retry into a duplicate object,
while wrongly keeping one costs a row in a small table.

### Blob garbage collection — Timer 2

Three eligibility classes, one query:

```sql
SELECT blob_key FROM blob_ledger
WHERE (state = 'staged'    AND state_changed_at < now() - interval '24 hours')
   OR (state = 'retained'  AND state_changed_at < now() - interval '365 days')
   OR  state = 'purgeable'
```

Then per key, in its own short transaction: `SELECT ... FOR UPDATE SKIP
LOCKED` re-checking the state is still eligible, `DELETE` the row, commit,
then `store.Remove(key)`. Row first, file second — that's the codebase's
established convention (`blobs.Delete` documents it): if the file removal
fails after the row is gone, reconciliation re-adopts the file as `staged`
and the next GC cycle purges it. The opposite order would leave a row
pointing at nothing, which is worse. `Remove` already treats a missing file
as success, which also handles staged rows whose file write never happened
(Stage's documented failure mode).

`SKIP LOCKED` plus the re-check settles the boundary race with the claim
path: object create locks the ledger row and requires `staged` less than 24
hours old, so whichever side gets the lock first wins consistently — GC never
deletes a row mid-claim, and a claim that loses the race fails cleanly and
the client retries.

Process keys in batches of 500 per pass, looping until a pass finds nothing —
runs stay bounded per transaction but a big backlog (the first run will purge
every `retained` row migration 010 seeded with an `orphaned_at` older than a
year) still clears in one firing. Report rows purged and files removed.

## Build order

1. `internal/jobs`: the four job functions. Integration tests per the
   existing convention (`JOBS_TEST_DATABASE_URL`, skip when unset, migrate,
   seed a throwaway user, clean up):
   - reaper: expired session deleted, live session kept.
   - reconciliation: orphan file adopted as staged; file under a bogus user
     dir skipped; cap honored (insert 501 orphans, expect 500); a key that
     already has a row untouched.
   - expiry: old pending deleted; fresh pending kept; successful create
     (wrapped 201 and legacy unwrapped shape) kept at any age; old non-create
     and old failed-create deleted.
   - GC: expired staged purged (row and file); fresh staged kept; purgeable
     purged immediately; `claimed` and `legacy_shared` never touched; staged
     row with no file deleted without error.
2. The runner: two tickers, 60s initial delay, error-and-continue. Unit test
   with compressed intervals — fires happen, an erroring job doesn't kill the
   loop.
3. Wire into `main()`.
4. Dev UI (`cmd/server/devui.go`): a jobs panel with a run-now button per
   job showing the returned counts. Backed by four `POST /dev/jobs/{name}`
   handlers registered only when `DEV_UI=true`, calling the same job
   functions the timers call. This is the feature's `/dev` demo: stage a
   blob in the existing panels, run GC, watch the counts.

## Out of scope

- No env config for intervals or retention windows — the plan fixes them.
- No job-run history table or metrics endpoint; a log line per firing is
  enough until something needs more.
- Centralized error logging (the buildout list's separate item) — jobs log
  through the same `log` calls the rest of the server uses today.
