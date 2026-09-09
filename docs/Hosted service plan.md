# Hosted service plan

Status: decisions settled 2026-09-09 in a design interview. Spec: issue #7. Implemented so
far: `AUTH_MODE=oidc` (commit cbe395c, MR !19), then build-order steps 1 and 2 — the Login
Hand-off endpoints and the email-domain allowlist (issue #8) — step 3, the Plan record,
the Storage Usage counter, and `quota_exceeded` (issue #9) — then step 4: the
subscription record, the Polar webhook, and the Entitlement gate (issue #10), and
checkout, the customer portal, and the success page (issue #11). Step 4's code is
done; one thing in it is not, and it gates the MVP being usable — **nobody has run
a checkout against the Polar sandbox yet**, so the 100%-forever discount below is
documented rather than observed.

Step 5, the Manifest deployment (issue #12), is under way. Its operational
half — the full environment contract, the Manifest entry, and the one-time
Zitadel, Polar, DNS, and database setup — is in
[Hosted deployment](./Hosted%20deployment.md). What exists so far is the
Manifest service definition, the documented contract, and
`cmd/hostedrehearsal`, which walks the whole hosted path against a real server
process on localhost with local stand-ins for Zitadel and Polar. **Nothing is
deployed**, and the four steps that need a console — DNS, the Zitadel
application for the public redirect URI, the Polar discount and webhook
endpoint, and the 1Password item — have not been done.

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

**Both questions answered, from Polar's own documentation
([Discounts](https://polar.sh/docs/features/discounts), read 2026-09-09):**

- **A recurring discount can cover every renewal.** A discount's duration is
  one of Once, Several Months, or Forever, and Forever "is applied
  indefinitely". So `POLAR_DISCOUNT_ID` has to point at a **100% percentage
  discount with duration Forever**. A "Once" discount at 100% would make month
  one free and bill $5 in month two, which is exactly the failure the MVP
  cannot have.
- **A 100%-forever checkout does not ask for a card.** Polar's wording, in the
  per-customer-limits section: "Free products and 100% forever subscriptions
  never ask for a card, so those match on ID and email alone." That sentence
  is scoped to the *forever* case, which is another reason the discount has to
  be Forever rather than Several Months: a discount that lapses has a charge
  coming, so Polar would collect a card up front.

Not yet done against the live sandbox — nobody has created the discount there
or run a checkout through it. The two facts above come from Polar's docs, not
from a checkout anyone has watched. **Confirm both on the first real sandbox
run, and check the discount's duration reads Forever in the Polar dashboard
before pointing `POLAR_DISCOUNT_ID` at it.**

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

Settled while building checkout and the portal:

- **A Polar customer is created lazily, at most once per account.** The first
  checkout or portal request registers the account with `POST /v1/customers/`
  under its OIDC `sub` as Polar's external customer id and its email address,
  and stores Polar's customer id on the subscription row. That is the same id
  the webhook names the account by, so the round trip needs no lookup table.
  Polar refuses a second customer carrying an external id it has already seen,
  so the stored id is what makes abandoning a checkout and starting another
  safe. Registering at login instead would create a Polar customer for every
  person who never pays.
- **The customer id lives on the subscription row, which meant relaxing two
  columns.** `polar_subscription_id` and `last_event` became nullable
  (migration `015` / sqlite `005`): a row that holds only a customer describes
  a subscription that has not happened yet, and a sentinel there would read
  like a real subscription and a real delivery. `state` on such a row is
  `none`, which is what an account with no row already read as, so nothing
  about `GET /api/billing` changed. Saving a customer id never overwrites a
  live subscription — opening the portal must not roll an active account back
  to unsubscribed.
- **The checkout's success URL is derived from `OIDC_REDIRECT_URI`.** The
  server has no configured public origin of its own, and that variable already
  has to be an absolute URL whose path ends in the OIDC callback, so trimming
  the callback off leaves the origin. Same trick the Login Hand-off uses to
  build its login URL. Adding a second "what is my public URL" setting would
  give an operator two chances to get it wrong.
- **The success page is `GET /billing/done`**, served outside `requireAuth`
  because the browser Polar returns holds no app session, and mounted only in
  oidc mode. It says "go back to FUTO Notes" and nothing about the account:
  Entitlement moves when the webhook lands, not when the browser arrives, so
  the page cannot honestly report success. The app learns by re-reading
  `GET /api/billing`.
- **An entitled account asking for a checkout gets its billing status
  instead**, in exactly the body `GET /api/billing` returns. Handing it a
  checkout URL would let it open a second subscription. The client tells the
  two answers apart by whether `url` is present.
- **Polar failures are `500`, and nothing is written.** Both routes call Polar
  synchronously; a refusal or a timeout leaves the account exactly as it was
  and the same request can be retried. This is the only place the server waits
  on Polar — the sync path never does.
- **Known gap: a lost customer id cannot be recovered.** Registering the
  customer is read-then-create-then-store with nothing atomic across the
  three. Two checkouts started at the same instant are fine — the loser gets
  Polar's "external_id is already used" as a `500` and its retry finds the
  stored id — but if the create succeeds and the *store* fails, Polar holds a
  customer this server has no id for, and every later checkout or portal
  request for that account 500s forever. The escape is one extra Polar call,
  `GET /v1/customers/external/{external_id}`, on the create-refused path;
  issue #11 asked for three calls and got three, so this is written down
  rather than built. Worth doing before anyone outside FUTO signs up, or the
  first time it happens.

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
- Recover a lost Polar customer id rather than 500ing forever — see the last
  bullet under decision 4.

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
