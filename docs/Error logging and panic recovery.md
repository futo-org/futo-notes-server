# Error logging and panic recovery

Plan for the "Centralized/shared error logging" item in the migration plan, plus
panic-recovery middleware in the style of Let's Go §6.4.

## Where things stand

The working tree is mid-refactor and does not build. `cmd/server/auth.go`,
`blobs.go`, `collections.go`, and `objects.go` already call a
`serverError(w, r, action, err)` helper at every 500 site, but the helper is not
defined yet (`undefined: serverError`). Step 1 below lands that helper and makes
the tree compile again.

Remaining loose log sites, all `log.Printf`/`log.Fatal` on the stdlib default
logger:

- `cmd/server/main.go` — startup fatals, migration notice, listen notice, `writeJSON` encode failures
- `cmd/server/objects.go` — `internalObjectError` (batch-path duplicate of `serverError`), batch staging/apply errors
- `cmd/server/blobs.go` — batch streaming error
- `cmd/server/sync.go` — SSE flush/encode errors
- `internal/jobs/runner.go` — job outcome lines
- `internal/jobs/jobs.go` — reconciliation skips and cap notice
- `internal/events/listener.go` — LISTEN reconnects and bad payloads

## Goal

One logger, one error path. Every 5xx the server produces is logged in one
consistent shape (what failed, method, path, the error), and a panic inside a
handler produces a logged stack trace plus a proper JSON 500 instead of an empty
reply. "Standardized location" from the migration plan means **structured lines
on stderr** — systemd/docker already collect stderr, so self-hosters have one
place to look without us growing file-path config.

Non-goals: log files, log rotation, request-ID plumbing, log-level or
log-format env vars. None of these is needed by the current request; add later
if a real need shows up.

## Design

### 1. Logger: `log/slog` text handler on stderr

In `main()`, build the app logger once and install it as the default:

```go
logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
slog.SetDefault(logger)
```

Text handler, not JSON: `key=value` lines are readable over ssh and still
machine-parseable. Using `slog.SetDefault` means packages (`internal/jobs`,
`internal/events`) call `slog.Info`/`slog.Error` directly — no logger threading
through constructors for a single-process server.

### 2. `serverError` helper (fixes the build)

New `cmd/server/errors.go`:

```go
// serverError logs an internal failure with request context and answers
// with the generic 500 body. action says what the handler was doing.
func serverError(w http.ResponseWriter, r *http.Request, action string, err error) {
    slog.Error(action, "err", err, "method", r.Method, "path", r.URL.Path)
    writeError(w, http.StatusInternalServerError, "internal server error")
}
```

Contract note: the client-visible body stays exactly
`{"error":"internal server error"}` with status 500 — same as what the handlers
wrote before the refactor. Only the log side changes.

Fold `internalObjectError` (objects.go:339) into `serverError` — it is the same
thing minus the request context. Its call sites in the batch path have `r` in
scope.

### 3. Panic recovery middleware — yes, worth it

Go's HTTP server already recovers handler panics, so the process never dies
either way. What stdlib recovery gives the client is an **empty reply on a
closed connection** — to the notes client that is indistinguishable from a
network drop, so it burns a mutation-ID retry cycle for something that will
fail identically on retry. And the stack trace goes to the server's default
error log, outside the standardized stream we are building. ~20 lines of
middleware fix both, so: worth it.

New middleware in `cmd/server` (per Let's Go §6.4, adapted for a JSON API):

```go
func recoverPanic(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        defer func() {
            pv := recover()
            if pv == nil {
                return
            }
            if pv == http.ErrAbortHandler {
                panic(pv) // stdlib sentinel for intentional aborts; preserve it
            }
            slog.Error("panic", "err", fmt.Sprintf("%v", pv),
                "method", r.Method, "path", r.URL.Path,
                "stack", string(debug.Stack()))
            w.Header().Set("Connection", "close")
            writeError(w, http.StatusInternalServerError, "internal server error")
        }()
        next.ServeHTTP(w, r)
    })
}
```

Wire it outermost so it covers auth middleware, the API, and the dev UI:

```go
log.Fatal(http.ListenAndServe(addr, recoverPanic(mux)))
```

Details that matter:

- **`http.ErrAbortHandler` re-panics.** It is the stdlib's sanctioned way to
  abort a response; swallowing it would log noise and change semantics.
- **Streams (SSE, blob batch) that panic mid-body can't get a 500** — headers
  are already written, so the `WriteHeader` inside `writeError` is a no-op and
  the client sees a truncated stream. That is unavoidable; the stack still gets
  logged, which is the part we control.
- `Connection: close` makes Go close the connection after the response
  (stripped automatically under HTTP/2).

### 4. Migrate the loose log sites

Mechanical sweep, same commit or a follow-up:

- `log.Fatal` in startup → keep fatal semantics: `slog.Error(...)` +
  `os.Exit(1)`, or keep `log.Fatal` (it forwards to the default slog handler
  once `SetDefault` is called — verify, then pick the simpler one).
- Every remaining `log.Printf` → `slog.Info` for notices (migrations applied,
  listening, job summaries, listener reconnects) or `slog.Error` for failures
  (SSE encode, batch staging, writeJSON encode).
- Drop the `"log"` import from files that no longer need it.

### 5. Dev UI demo

Per CLAUDE.md every feature needs a `/dev` demo. Add a dev-only route
`POST /dev/panic` that panics, and a card in `devui.html` that calls it — the
demo shows the JSON 500 + `Connection: close` in the response panel while the
stack trace lands on stderr. Registered only under `cfg.DevUI`, like the job
triggers.

## Tests

- `serverError`: answers 500 with body `{"error":"internal server error"}`.
- `recoverPanic`: a panicking handler yields status 500, JSON body,
  `Connection: close` header; `http.ErrAbortHandler` propagates (test with
  `defer`/`recover` around `ServeHTTP`); a non-panicking handler passes through
  untouched.
- Log assertions where cheap: point `slog.SetDefault` at a buffer handler in the
  test and check the `err`/`path` attrs appear. Don't over-test log wording.
- Full suite stays green: `GOTOOLCHAIN=auto go test ./...`.

## Order of work

1. `cmd/server/errors.go` with `serverError` + slog setup in `main()` — tree
   compiles again; run the suite.
2. Fold `internalObjectError` into `serverError`.
3. `recoverPanic` middleware + tests, wired outermost.
4. Sweep remaining `log.Printf`/`log.Fatal` sites to slog.
5. `/dev/panic` demo card.
6. Full build, vet, test pass.

## Deferred

- JSON log format / level via env — when a hosted deployment needs it.
- Request IDs for correlating log lines to client reports.
- Job/listener errors carrying structured attrs (job name is already in the
  message today; fine as-is).
