# Building out Sync SSE

Plan for `GET /api/sync/events`, the last unbuilt route in the buildout order.
Contract lives in `openapi.yaml` (`/api/sync/events`) and the lifecycle sections
of `Rewriting the server in Go.md`. This doc is about how to build it here.

## What the contract requires

From the OpenAPI spec:

- A doorbell, not a pipe. Events carry no object content.
- Three event types:
  - `ready` — sent once per connection, empty data. Client treats it as "pull now".
  - `change` — `{"collectionId": "...", "currentVersion": N}`. `currentVersion`
    is a JSON **number** (computed scalar, not a row field — see the
    ints-vs-strings rule).
  - `ping` — empty data, every 25 seconds.
- Pending `change` events are **coalesced per collection** (newest version wins),
  not queued, so a slow reader can't grow memory without bound.
- The stream is lossy. No event log, no `Last-Event-ID` replay.
- When the server's upstream Postgres `LISTEN` connection drops, the server
  **deliberately ends every client stream** so clients reconnect and re-pull
  rather than trust a silently-dead stream.
- Response headers include `X-Accel-Buffering: no`.
- `401` for missing/invalid credentials — the route sits behind the existing
  `requireAuth` middleware like every other `/api/*` route.

From the migration plan:

- Transport is Postgres `LISTEN`/`NOTIFY`, so multiple server processes sharing
  one Postgres all see every change.
- The create, update, and delete lifecycles each include a "publish SSE
  notification" step *inside* the transaction. None of the write paths publish
  anything today — that's part of this work.

One detail neither doc states outright but which follows from the spec's
ownership invariant ("anything the authenticated user does not own returns 404,
never 403, so the API does not leak existence across tenants"): a user's stream
must only carry `change` events for **their own** collections. Broadcasting
every collection id to every connected client would leak existence. So the
NOTIFY payload carries the owning `userId` for routing, and the hub delivers an
event only to that user's subscribers. The `userId` never appears in the SSE
data itself.

## Design

One new package, `internal/events`, with three pieces: a publish helper called
from inside write transactions, a listener goroutine holding a dedicated
Postgres connection, and a hub that fans notifications out to per-request
subscriber channels.

### Publishing — inside the write transaction

`NOTIFY` issued inside a transaction is only delivered on commit, and is
discarded on rollback. That is exactly the semantics the lifecycles ask for, so
publishing is one statement added to each write path, no post-commit hook
machinery:

```go
// events.Publish(ctx, tx, userID, collectionID, currentVersion)
SELECT pg_notify('notes_changes', $payload)
```

Payload is JSON: `{"userId": "...", "collectionId": "...", "currentVersion": N}`.
(Postgres caps NOTIFY payloads at ~8000 bytes; two UUIDs and an int are nowhere
close.)

Call sites — every path that bumps `collections.current_version` and commits:

- `objects.Create` — the success path only, after the collection version bump,
  before `COMMIT`. Replay/conflict/not-found paths commit without publishing
  (nothing changed).
- `objects.Update` — success path only.
- `objects.Delete` — success path, including the delete-specific-version path
  when it actually performs the delete. The already-deleted early-out publishes
  nothing.

That covers the plain object routes, both blob-object routes, and the batch
route for free, since they all funnel through these three functions. Collection
create/delete and key writes don't bump `current_version` through this path and
the lifecycles don't list a publish step for them, so they stay silent.

### Listening — one dedicated connection

`pgx/v5` through `database/sql` doesn't surface notifications, so the listener
owns a **native** `pgx.Conn` (dialed from `cfg.DatabaseURL`, outside the pool):

1. Connect, `LISTEN notes_changes`.
2. Loop on `conn.WaitForNotification(ctx)`, decode the JSON payload, hand it to
   the hub. A payload that fails to decode is logged and dropped.
3. On any error: per the spec, **close every subscriber** (ends their streams),
   then reconnect with backoff (1s doubling to 30s) for future clients. Clients
   that got cut reconnect, receive `ready`, and re-pull — nothing is lost that
   the lossy contract didn't already allow.

Started from `main()` after migrations, before the HTTP server.

### The hub — coalescing fan-out

Per connected client, keyed by user id:

- A subscriber is a mutex-guarded `map[collectionID]currentVersion` plus a
  1-buffered wake channel. On notification: overwrite the map entry (newest
  version wins — versions only go up, so a plain overwrite is correct), then a
  non-blocking send on the wake channel. This is the coalescing the spec
  mandates; memory per subscriber is bounded by the user's collection count.
- The handler goroutine drains the map on wake and writes one `change` event
  per entry.
- `Subscribe(userID)` / `Unsubscribe` on connect/disconnect; the hub indexes
  subscribers by user id so routing is a map lookup.
- `CloseAll()` for the listener-drop case: closes every wake channel with a
  flag telling handlers to end the response rather than keep waiting.

### The handler

`GET /api/sync/events` on the authed mux:

1. Check `http.NewResponseController(w)` can flush (it can, on net/http's
   default server).
2. Headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
   `X-Accel-Buffering: no`.
3. Write `event: ready\ndata:\n\n`, flush.
4. Subscribe. Loop with a `select` over: wake channel (drain map, write
   `change` events, flush), 25s ticker (write `ping`, flush),
   `r.Context().Done()` (client gone), hub shutdown (end response).
5. Unsubscribe on every exit path.

Event framing detail: the exact `data:` layout for empty-data events (bare
`data:` line vs omitted) should match what the TS server emits — verify against
a captured stream or the spec examples before locking the tests in, since
EventSource implementations differ on tolerance.

One net/http footnote: the server's default write timeout would kill a
long-lived stream, but `main.go` uses `http.ListenAndServe` with no timeouts
set, so nothing to change — just don't add a blanket `WriteTimeout` later
without exempting this route.

## Build order

1. `internal/events`: payload type, `Publish` (tx statement), hub with
   coalescing. Unit-test the hub in isolation — coalescing (three
   notifications for one collection → one entry, highest version), per-user
   isolation, wake semantics, `CloseAll`.
2. Wire `Publish` into `objects.Create` / `Update` / `Delete` success paths.
   Integration-test against Postgres: a committed create delivers exactly one
   notification with the right payload; a conflict (409) delivers none; a
   replay delivers none.
3. Listener with reconnect/close-all behavior.
4. Handler + route registration in `main.go`. Integration test with a real
   HTTP client: connect → `ready`; create an object → `change` with matching
   collectionId and a numeric currentVersion; second user sees nothing;
   listener kill → stream ends.
5. Dev UI: add an SSE panel to `/dev` (`cmd/server/devui.go`) — connect button,
   live event log, so a create in the existing objects panel visibly rings the
   doorbell.

## Out of scope

- `Last-Event-ID` / replay — spec explicitly says lossy, no recovery.
- CORS changes for browser `EventSource` — spec documents the limitation and
  accepts it.
- Recurring jobs (next work item after this, unblocked once write lifecycles
  are complete).
