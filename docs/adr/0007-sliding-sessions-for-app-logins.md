---
status: accepted
date: 2026-09-15
---

# App sessions slide; every other session stays fixed

Every session used to expire seven days after it was opened, with no renewal.
That was fine when the only login was a password the client had kept: a `401`
was answered by logging in again, invisibly. The hosted service has no such
password. Its only login path is the Login Hand-off, which ends in a system
browser and a person, so a seven-day session means every device is thrown into
a browser once a week. The client's flow ADR
(`futo-notes/futo-notes#172`, client ADR 0003) asked for the fix and
`docs/Hosted service plan.md` carries it as the first of four client-driven
follow-ups.

**A session opened by redeeming a Login Hand-off ticket expires 90 days after
its last authenticated request.** A device that syncs at least once a quarter
never logs in again. Everything else keeps the fixed seven days.

## What stays fixed, and why

**The browser cookie.** The hand-off's OIDC callback sets a `session` cookie
for the browser as well as parking the app's session against the ticket. Those
are two separate sessions on purpose — logging out in one does not end the
other — and only the app's slides. A browser on a shared or borrowed machine
is exactly where a long-lived credential is worst, and the person is standing
in front of a login page already.

**Password mode and dev mode: unchanged, still seven days fixed.** This is a
decision, not an omission. Both are self-hosted, both authenticate against a
secret the client already holds, and in both a session ending is one silent
re-login rather than a browser trip. The `401` contract that makes that
invisible is already documented and already implemented. Extending their
sessions would buy nothing and would hand a self-hosted deployment a
credential that outlives a stolen laptop by three months. If a self-hoster
ever asks, the switch is one `SessionKind` at one call site.

## Coarse renewal: at most one write per hour

A literal reading — "expires 90 days after the last request" — would write the
`sessions` row on every authenticated request. A syncing client polls; that is
a write per poll, per device, forever, on the row every request already reads.

So the expiry is written forward only once the previous write is an hour old.
A row's `expires_at` is its last renewal plus 90 days, so the row itself says
when it was last renewed and no second column is needed. A device polling
every few seconds writes its session 24 times a day instead of thousands.

The price is that a session can die up to an hour before its full 90 days.
That is 0.05% of the window, against a hazard — a device that has been silent
for 90 days minus an hour — that is indistinguishable from the 90-day cliff
itself. An hour is short enough that nobody who is actually using the app can
land in it and long enough that the write is rare. It is a constant, not a
setting: there is no deployment for which a different value is right, and an
operator who could get it wrong is an operator who can break their own logins.

A renewal that fails is logged and dropped, not turned into an error. The
session is already known valid by the time the write is attempted, and the
next request an hour later retries it. Failing instead would let a momentarily
busy database break authenticated reads that needed no write at all before
sessions slid.

## What this does not change

The `401` contract is untouched: a dead session still answers
`WWW-Authenticate: Bearer … error="invalid_token"` with
`{"error": "session expired or invalid", "code": "invalid_session"}`, and a
client still re-logs in and keeps its sync cursor. A longer session is
therefore invisible to a correct client — it just stops happening as often.

The hourly session reaper is untouched too. It deletes rows whose `expires_at`
has passed, which is still exactly the set of dead sessions.

The lifetime lives in the `sessions` row as a `sliding` boolean, defaulting to
false, so every session issued before this change keeps the lifetime it was
opened with.
