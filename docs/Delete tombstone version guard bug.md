# Bug: repeat DELETE of a tombstoned object skips the version guard and freezes the cursor

Found 2026-09-04 during sync fuzzing. Reproduced on a `-race` build from the working tree at
`main`. Server-side only; not a client bug.

## Severity

Low–medium. No data loss and no crash. It is a contract violation: two documented behaviors do not
hold once an object is already soft-deleted.

## Endpoint

`DELETE /api/collections/{id}/objects/{objectId}[?version=N]`
(`cmd/server/main.go:116` → `handleDeleteObject`, `cmd/server/objects.go:189` parses `?version`.)

## Contract

`docs/API.md`, Delete section:

- `→ 409 { "error": "version conflict", ... }   // only if ?version is supplied and stale`
- "Deleting an already-deleted object is another mutation: without a replayed Mutation ID it
  advances the tombstone version and collection cursor again."

## Actual behavior

Once `object.deleted == true`, every subsequent DELETE returns `200` with the tombstone frozen at
its first value, regardless of `?version`:

- A repeat unconditional delete does not advance the object version or `collectionVersion`.
- A stale `?version=N` returns `200` instead of `409 version conflict`; the race guard is never
  evaluated.

## Root cause

`internal/objects/objects.go`, `Delete()` (`internal/objects/objects.go:695`).

The `if current.Deleted { ... }` branch at `internal/objects/objects.go:760` returns `200` with the
existing tombstone and commits, before the `expectedVersion` conflict check at
`internal/objects/objects.go:784` and before the `current_version + 1` update below it. So for a
tombstoned object the version guard is unreachable and the cursor is never bumped.

## Reproduction

Dev-mode server on `:3011`.

```bash
B=http://127.0.0.1:3011
tok=$(curl -s -X POST $B/api/auth/dev/login -H 'content-type: application/json' \
  -d '{"email":"bug@x.com"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
auth="Authorization: Bearer $tok"
cid=$(curl -s -H "$auth" -X POST $B/api/collections -H 'content-type: application/json' -d '{}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["collection"]["id"])')
oid=$(printf 'ciphertext' | curl -s -H "$auth" -X POST $B/api/collections/$cid/blob-objects \
  --data-binary @- -H 'content-type: application/octet-stream' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["object"]["id"])')

curl -s -H "$auth" -X DELETE $B/api/collections/$cid/objects/$oid                 # D1: 200, version 2, cv 2
curl -s -H "$auth" -X DELETE $B/api/collections/$cid/objects/$oid                 # D2: 200, version 2, cv 2
curl -s -H "$auth" -X DELETE "$B/api/collections/$cid/objects/$oid?version=1"     # D3: 200 (expected 409)
```

Object version and collection `current_version` after each call:

| Call | Request | Expected | Observed |
|------|---------|----------|----------|
| D1 | delete, no version | version 2, cv 2 | version 2, cv 2 |
| D2 | delete, no version | version 3, cv 3 | version 2, cv 2 |
| D3 | delete, `?version=1` (stale) | 409 version conflict | 200, version 2, cv 2 |

## Impact

A peer that detects change by watching `collectionVersion` never observes a repeated tombstone, so a
re-delete does not propagate. The edit-vs-delete race guard (`?version`) is silently a no-op against
an already-deleted object.

## Suggested fix

In `Delete()`, evaluate `expectedVersion` before the `current.Deleted` early return, and either
advance the tombstone version + `current_version` on an unconditional re-delete (matches the doc) or
change the doc to specify idempotent re-delete. If the code becomes idempotent, `?version` on a
tombstone should still `409` when stale.

## Related

Found by three independent fuzz agents (object-sync, object-concurrency, concurrency-race) and
reproduced directly. Full fleet summary and per-agent reports live outside the repo in the fuzzing
scratchpad.
