# Hosted service plan

Status: decisions settled 2026-09-09 in a design interview. Spec: issue #7. Implemented so
far: `AUTH_MODE=oidc` (commit cbe395c, MR !19), then build-order steps 1 and 2 — the Login
Hand-off endpoints and the email-domain allowlist (issue #8) — step 3, the Plan record,
the Storage Usage counter, and `quota_exceeded` (issue #9) — and the first half of step 4,
the subscription record, the Polar webhook, and the Entitlement gate (issue #10). Checkout
and the customer portal are issue #11.

Goal: a paid sync service, $5/month, that a person signs up for from inside the
app without visiting the web. Once signed up, hosted and self-hosted must feel
identical to the client. The first milestone is an internal-only, free MVP that
still exercises a real checkout.

Vocabulary is in `CONTEXT.md` (Login Hand-off, Entitlement, Grace Period, Plan,
Storage Quota, Storage Usage, Vault). Client-side work is out of scope for this repo and is
only mentioned where it fixes a server contract.

## Decisions

### 1. Login: server-brokered Login Hand-off

Zitadel's login page needs a browser. The server stays the confidential OIDC
client and hands the app a session:

1. App calls `POST /api/auth/handoff`, gets `{ticket, url}`.
2. App opens `url` (the OIDC start route carrying the ticket) in whatever
   browser surface the client team picks: system browser, platform auth sheet,
   or webview. The server does not care.
3. After the callback, the server binds the ticket to the account that
   authenticated, for a few minutes, and shows a "you can go back to the app"
   page. As built, the ticket row records the account and never a session
   token; the session is opened at redemption, so a leak of that table yields
   no usable bearer credential and the browser's cookie session stays
   independent of the app's.
4. App polls `GET /api/auth/handoff/{ticket}`: 202 while pending, the
   `{user, token}` body exactly once, 404 afterwards and after expiry.

Rejected: the app as the OIDC client (per-platform custom-scheme plumbing,
server must validate Zitadel tokens, client ID baked into the app), the OAuth
device flow (worse UX, user types a code), a native login UI on Zitadel's
Session API (best UX, most work by far; the hand-off endpoint is shaped so it
could drop in later and produce the same session).

**Accepted risk: the hand-off is phishable.** Nothing binds a ticket to the
person who completes it, so whoever mints a ticket redeems whatever login
finishes it. A crafted start URL that a victim opens and signs in through
hands the ticket's minter that victim's session — and the victim sees a
genuine "Log in with FUTO" page for the real product, so there is nothing
off about it to notice. The server cannot tell an app-initiated login from a
phished one, because the app opens the URL itself and no code passes through
the person's hands. That binding is exactly what the rejected device flow
buys, which is why it stays the fallback if this ever matters more than the
UX.

Mitigations considered and not built: pinning the ticket to the minting
client's IP (breaks a login that moves between networks, and stops nothing
on a shared one), and shortening the window (changes the attack's
convenience, not the attack). What makes this acceptable today is decision 2
— an attacker's target has to be someone already inside the email-domain
allowlist, and the whole user base is FUTO staff. It stops being acceptable
the moment sign-up opens, so it is on the pre-launch list below.

### 2. MVP gate: email-domain allowlist

`HOSTED_ALLOWED_EMAIL_DOMAINS=futo.org`, checked at the OIDC callback. Anyone
else gets a clear "not open yet" error. Unset the variable to open sign-up. No
Zitadel-side configuration, because the Zitadel org is shared with other FUTO
services and already holds their customers.

### 3. Entitlement: gate writes, never reads

- Uploads and object mutations require Entitlement. Pulls, blob downloads, and
  the SSE stream never do. A lapsed card never strands a device.
- Entitled = subscription active, or within the 7-day Grace Period after Polar
  reports a payment failure.
- No subscription at all is treated as lapsed. First run is login, checkout,
  sync.
- Gated writes fail with error code `subscription_required`.

### 4. Billing: Polar

- Truth lives locally: a subscription record per account, written by Polar
  webhooks, read by the entitlement check. No Polar call on the hot path. A
  manual refresh from Polar is fine as an operator tool, not a request path.
  Yucca's PR immich-app/yucca#119 has the same shape and is the reference
  (NestJS; do not copy files).
- Dev and CI run against Polar's sandbox. Tests use a fake webhook sender.
- Prod runs the real $5 product. During the internal MVP the checkout is
  created with a pre-applied 100% discount (`POLAR_DISCOUNT_ID`); dropping the
  variable is the launch flip. No product-ID swap on launch day and no
  hand-rolled mock.
- Verify in Polar before relying on it: whether a $0-after-discount checkout
  still asks for a card, and whether the discount can apply to every renewal
  ("forever"). If it cannot, internal accounts get billed on month two.

Settled while building the webhook and the gate:

- **Polar signs with Standard Webhooks.** Polar's own note is that secrets
  generated on or after 8 September 2026 follow
  [Standard Webhooks](https://www.standardwebhooks.com/), and older ones use
  a Polar-specific HMAC. The server implements the current scheme only —
  `whsec_`-prefixed base64 key, HMAC-SHA256 over `<id>.<timestamp>.<body>`,
  base64, carried as space-separated `v1,<sig>` entries — because the MVP's
  endpoint is being created after that date. A secret older than the cutover
  would have to be regenerated.
- **The signature is checked with a five-minute timestamp tolerance**, which
  is what stops a captured delivery replaying indefinitely. Without it a
  signature stays valid as long as the secret does.
- **`state` is Polar's `status`, stored as delivered**, not the event type.
  That is what makes cancel-at-period-end behave correctly: Polar leaves the
  status `active` until it revokes the subscription, so a person who cancels
  keeps writing through the period they paid for.
- **The five-minute tolerance does not strand a retry.** Standard Webhooks
  re-signs each attempt: "every time an attempt is retried the timestamp of
  the attempt is updated, while the timestamp of the original event remains
  the same." A delivery missed during a restart still verifies hours later,
  and only a genuine replay of captured bytes is refused.
- **An unmatched or unhandled delivery is acknowledged, not failed.** A
  non-2xx would have Polar retrying an event that will never apply: the
  endpoint is shared with the rest of the organization's Polar traffic, and
  a customer this server does not know will not appear on a retry.

Not settled by the ticket, decided while building it:

- **`subscription_required` answers `402 Payment Required`**, with
  `{error, code}`. Issue #10 names no status code; this one was chosen at
  the keyboard and confirmed. Deliberately not `403`: nothing about the
  caller's identity is wrong, and the fix is a payment rather than a
  permission.
- **The vault claim is not gated; the vault delete is.** Issue #10's gated
  list reads "object create, update, delete, batch create, blob stage, batch
  upload, vault key claim or rotate", under the heading "every route that
  mutates collection contents or stages blobs". "Vault key claim or rotate"
  is read as the one route that writes key material,
  `PUT /api/collections/{id}/key`. `POST /api/collections` stays open: it
  writes collection identity, which `CONTEXT.md` says is not collection
  contents, and a lapsed account still has to find its collection to pull
  from it. `DELETE /api/collections/{id}` is gated — deleting a collection
  makes its blobs purgeable, so it is a contents mutation like any other.
  Someone who wants their data gone after lapsing asks for it to be deleted;
  what is left sitting there is ciphertext the server cannot read.

### 5. Limits

| Limit | Value | Where enforced |
|---|---|---|
| Storage Quota | 10 GB per account | blob upload; reject with `quota_exceeded` when the new blob would cross it |
| Per-blob cap | 100 MiB (unchanged) | existing constant, becomes a Plan field |
| Per-batch cap | 32 MiB (unchanged) | existing constant |
| Rate limits, object counts | none | monitor first |

Both plan values come from a Plan record, one plan for now. Bytes used is a
counter maintained by the blob-ledger state transitions (claimed and retained
count, staged and purgeable do not), never a `SUM` at upload time.

Settled while building step 3:

- **10 GB is decimal**, 10,000,000,000 bytes. The per-blob cap stays 100 MiB,
  104,857,600 bytes, which is the constant the server already enforced.
- **The Storage Quota is enforced only in oidc mode.** A self-hosted server
  has no plan ceiling and stores whatever fits on its disk; giving one a 10 GB
  limit it never had would be a regression. The per-blob cap does come from
  the Plan everywhere, and the one plan carries today's value, so nothing
  changes for a self-hoster.
- **`quota_exceeded` answers `507 Insufficient Storage`**, with
  `{error, code, quota_bytes, bytes_used}`. Deliberately not the `413` an
  over-cap blob gets: a smaller blob may fit under the per-blob cap, but the
  account being full is a different problem with a different fix.
- **The claim is the enforcing check**, not the upload. Staged bytes do not
  count until an object claims them, so an upload check on its own can be
  walked past without limit and without any concurrency: stage as many blobs
  as you like against an empty counter, then claim them all. The check
  therefore lives in the object mutation, inside the transaction that moves
  the ledger row and the counter together, where the ledger's size is
  authoritative and the client's declared `size_bytes` cannot lie about it.
- Uploads still check, as a courtesy rather than a guarantee: it avoids
  writing ciphertext that the claim is going to reject. `POST /api/blobs`,
  both inline blob-object routes, and each batch entry do this, and a batch
  weighs each entry against what the earlier ones claimed.
- A refused claim commits nothing and does not spend its Mutation ID. A quota
  refusal is transient, so the same retry has to succeed once the account has
  room, rather than replaying the refusal for the ID's retention window.
- The check does not lock the counter. It does not have to yet: every object
  mutation already holds its collection's row lock by the time it checks, and
  an account has one vault, so its claims are serialized. **A second
  collection per account reopens that window** — the check would have to lock
  the counter row itself. Worth remembering when multiple vaults land, since
  the lock that closes it today is taken for an unrelated reason.

### 6. Data model: no structural change

- Vault is the root collection. One root per account, still enforced in the
  claim route, not by a constraint.
- Every new hosted table keys on the account (`user_id`), never on a
  collection, so multiple vaults and ADR 0003's folder subtrees are later,
  additive migrations.
- The CRDT op-log (client MR !224) is a later protocol version. Hosted code
  never reaches into `objects` or `blob_ledger`; it asks the sync layer for
  "bytes used" and "is this a write" through existing seams so a new write
  path can plug in without touching billing.

### 7. Deployment: Postgres from day one, blobs on a host volume

See ADR 0004. Internal MVP runs on FUTO's Patroni Postgres via Manifest with the
filesystem blob store on a host volume. Destination is Yucca's Kubernetes
(CloudNativePG for Postgres, Ceph RGW for S3), which is not production-ready
today. The S3 blob store is the piece that unlocks that move and is the first
task after MVP.

### 8. Client contract

Capability: `billing: true` on hosted servers. The app shows the sign-up path
when it sees `auth_mode: oidc` and `billing: true`. Self-hosted servers return
`billing: false` and none of the routes below exist.

| Route | Purpose |
|---|---|
| `POST /api/auth/handoff` | mint a Login Hand-off ticket and the URL to open |
| `GET /api/auth/handoff/{ticket}` | 202 pending, `{user, token}` once, then 404 |
| `GET /api/billing` | `{entitled, state, grace_until, plan: {storage_quota_bytes, blob_max_bytes}, usage: {bytes_used}}` |
| `POST /api/billing/checkout` | Polar checkout URL to open in the browser; success page says "go back to the app" |
| `GET /api/billing/portal` | Polar customer portal URL for cancel and card changes |
| `POST /api/billing/polar/webhook` | Polar → server, signature-verified |

New error codes: `subscription_required`, `quota_exceeded`.

No usage graphs, invoices, or plan switching in the app. Polar's portal covers
them.

## Not in the MVP, required before anyone outside FUTO can sign up

- Account deletion: `DELETE /api/account` cascades users → sessions,
  collections, objects, blobs; cancels the Polar subscription; forgets the
  Polar customer ID.
- S3 blob store against Yucca's Ceph RGW, with an env-driven store factory.
- A dedicated host rather than a second instance on the shared staging box.
- Remove `HOSTED_ALLOWED_EMAIL_DOMAINS` and `POLAR_DISCOUNT_ID`.
- Answer the Login Hand-off phishing risk in decision 1. The allowlist is what
  makes it tolerable now; removing the allowlist removes the mitigation, so
  these two are one piece of work, not two.

## Flagged, owned by the client team

How the vault key is unlocked when login is OIDC: no password reaches the
client, so PBKDF2-from-vault-password no longer has an input. Open since June
(`docs/MANAGED-LAUNCH.md`). It becomes visible the first time an internal
person signs up, so the client team needs an answer before the MVP is usable.

## Build order

Small chunks, each with a `/dev` demo (`cmd/server/devui.go`) and tests:

1. Login Hand-off endpoints on top of the existing oidc mode.
2. Email-domain allowlist.
3. Plan record and the bytes-used counter; `quota_exceeded` on upload.
4. Polar: subscription record, webhook, checkout, portal, entitlement gate,
   `GET /api/billing`, `billing: true` capability.
5. Manifest deployment on Patroni Postgres with a host volume.
