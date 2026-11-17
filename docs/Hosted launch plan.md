# Hosted launch plan

The server work that remains before the hosted service can launch, on top of what
[Hosted service plan](Hosted%20service%20plan.md) built (MR !34). It comes from two
research passes over the Notes apps (`futo-notes`) and the billing prior art. The client
half of the plan lives in the apps repo at `docs/plan/hosted-sync-regional.md`.

Status:

| Step | Items | State |
|---|---|---|
| S1 | A1–A5, A7 | Done, in MR !34 |
| S2 | B1, B2, D | Done, in the MR stacked on !34 (`feat/hosted-vault-unlock-test-stack`) |
| S3 | A6 | Done, in the MR stacked on S2 (`feat/hosted-batch-and-mail`). Storage batch upload is `POST /blobs/batch-upload`, next to batch fetch at `POST /blobs/batch` |
| S4 | E | Done, in the same MR: the sender, templates, stamps and dev stack. Billing (S5) calls it. |
| S5 | C | Done, in the MR stacked on S3/S4 (`feat/hosted-billing`): the model, gating, endpoints, webhooks, the mock web provider, the lapse and purge jobs, the emails, the dashboard, `/dev`, and the App Store and Play providers. The real web adapter waits for the provider choice. |

## Decisions (2026-11-17)

| Topic | Decision |
|---|---|
| Vault unlock under SSO | A **vault passphrase**, separate from the Zitadel login, **plus a recovery key and QR device pairing**. The apps already implement all three. The server adds what the recovery key and pairing need (B1, B2). |
| Redirect scheme | Release apps sign in through `tech.futo.notes`, dev builds through `tech.futo.notes.dev`. Desktop uses a loopback redirect (always allowed). |
| Hostnames | Prod `https://notes.futo.cloud`, staging `https://notes-staging.apps.futo.network`. |
| Payment providers | **Pluggable.** The web provider (FUTOpay, Polar or Stripe) is not chosen yet. App Store and Play are providers behind the same interface. |
| Store rails | Web, App Store and Play subscriptions **all ship at launch**. |
| Price | One flat plan: **$5/month** on the web, **$5.75/month** in the App Store and Play. |
| Lapse | The account turns **read-only** (writes get 402) and is **purged 90 days** after it lapses. |
| Lapse and purge emails | Sent by a new mail sender with a **Resend** driver (E). |
| Family Sharing | **Off** for the App Store subscription. |
| Account deletion | Requires a **recent sign-in** (A7). |
| One store account, two FUTO accounts | The subscription stays with the account it was bought under (C1). |
| Client CI | The server **releases the hosted binaries plus a compose bundle** the apps' test harness can start. |

## A. Contract fixes before MR !34 merges

### A1. A lost refresh response must not sign the device out

Today a spent storage refresh token gets a 409 with no token within 10 s and
revokes the session after that (`internal/hosted/account/refresh.go`). Suppose a
mobile app is killed after the server commits but before the keychain write, or
the response is dropped. The device then holds a spent token, and its next
exchange signs it out for good.

Fix: **derive the successor from the token it replaces**,
`successor = HMAC-SHA256(rotation_key, presented_token)`, base64url. Re-presenting
a token then works like this:

- **Its successor has never been exchanged:** answer 200 with that same
  successor and a fresh access token. This is idempotent, so it covers both a
  lost response and two concurrent exchanges. The 409 case goes away.
- **Its successor has already been exchanged:** the chain has moved on, so this
  is reuse. Revoke the session (401 `refresh_token_reused`), as today.

The server still stores only hashes. The new secret is `STORAGE_REFRESH_ROTATION_KEY`
(32 bytes, chart Secret). Each successor's hash is stored when it is first issued,
so rotating the key only breaks re-presenting a token exchanged under the old key
(401 `refresh_token_invalid`, session kept). The reuse rule moves from "spent" to
"spent and its successor spent".

### A2. A stable `code` on every hosted error

`HostedError` gains a required `code` alongside `statusCode`, `message` and
`requestId`, and storage errors get the same field. Several cases currently share
a status and differ only in the message:

- 409 on `/api/storage/token`: `refresh_token_rotating` (gone after A1) and `region_required`;
- 401: `refresh_token_invalid` and `refresh_token_reused`;
- `PUT /api/account/region` 409: `region_already_chosen`;
- `POST /api/account/restore` 409: `purge_started`. Restoring an account with no
  deletion pending stays an idempotent 200, so a client can retry after a lost
  response;
- receipt 400: `receipt_invalid`, `receipt_expired`, `receipt_wrong_region`, `receipt_wrong_user`;
- receipt 409: `receipt_used`;
- 403: `account_pending_deletion`.

Sync routes keep the self-hosted body shape. Their hosted-only failures (403, 402)
use the sync shape's existing `code` field, as `invalid_session` does today.

### A3. The capability document says blobs are regional

The hosted `GET /` adds `"blob_storage": "regional"`. Clients choose their
transport from that field rather than from `auth_mode: "oidc"`, which self-hosted
OIDC may also use one day. The self-hosted capability bytes do not change.

### A4. A device name on sign-in

`POST /api/auth/oidc/token` accepts an optional `device_name` (1–100 characters).
The session records it, and the devices list and dashboard show it. Without it,
the server falls back to the User-Agent. Today it only records the User-Agent of
the token request, which is the Rust HTTP client's (`reqwest/…`).

### A5. Redirect schemes

- Release apps use `tech.futo.notes`. Dev builds use `tech.futo.notes.dev`, so
  dev and release apps installed side by side never claim each other's redirects.
- Prod values allow `tech.futo.notes`. Staging values and the dev stack
  (`compose.hosted.yml`, the mise dev tasks) allow both.
- Claimed `https` app links stay refused for now.

### A6. Batch upload and batch claim

Without batching, each new object costs two round trips: upload to storage, then
claim at the API. A first sync of a large vault pays that for every object. Add:

- **Storage `POST /blobs/batch-upload`.** Framed like the self-hosted
  `blob-objects/batch` request (1–200 frames, 32 MiB cap), but each frame carries
  only blob bytes. It answers with one receipt per frame, in order, or a
  per-frame failure.
- **API `POST /api/collections/{id}/objects/batch`.** 1–200 entries, each with
  its own `Mutation-Id`, object id, version (for updates) and receipt. It answers
  with per-entry outcomes mirroring the self-hosted batch result shape. Each
  entry replays independently under ADR 0002.

Both go in the OpenAPI specs and conformance tests, plus a parity test showing a
batched entry and the equivalent single call give the same outcome.

### A7. Smaller fixes

- The hosted key `PUT` rejects unknown fields (strict decode), so an older
  client cannot store half of an envelope it believes was saved.
- **Account deletion requires a recent sign-in.**
  - Sessions record when their OIDC sign-in happened (`auth_time`).
  - `DELETE /api/account` and the dashboard's delete need a sign-in within the last
    10 minutes. Otherwise they answer `403` with code `reauthentication_required`.
  - The app and the dashboard then sign in again with `max_age=0`, which forces
    the provider to ask for credentials, and retry.
  - `POST /api/auth/oidc/start` gains an optional `reauthenticate=true` to pass
    that through.

## B. Vault unlock support

The apps already speak both contracts below. They come from decision records on
the earlier `origin/hosted-server` branch (its ADRs 0006 and 0008). The server
adopts them **on hosted only**. The self-hosted key routes keep their exact bytes.

### B1. Recovery envelope

- **Fields.** Three optional fields on hosted key material, mirroring the
  password envelope: `recovery_key_salt`, `recovery_key_kdf` (an object) and
  `recovery_encrypted_vault_key`.
- **All three or none.** A `PUT` carrying some but not all answers 400, and a
  partial row reads back as none.
- **A `PUT` replaces both envelopes.** Leaving the recovery fields out deletes
  the stored recovery envelope. That is why a passphrase change must resend the
  recovery envelope, and the docs state it as a deletion, not an omission.
- **One revision.** Both envelopes share one `key_updated_at`.
- **Storage.** A hosted-track table `collection_recovery_envelopes`, keyed by
  collection, written in the same transaction as the key material. The shared
  `collections` table does not change.

### B2. Pairing relay

```
POST /api/pairings            { public_key, device_name, platform } → 201 { id, expires_at }
POST /api/pairings/{id}/key   { ciphertext }                        → 204 | 409 (already posted)
GET  /api/pairings/{id}                                             → 202 | 200 { ciphertext } | 404
```

- **Scope.** Every call is authenticated and limited to the calling account.
- **Window.** A pairing lives 5 minutes from creation.
- **Collecting spends it.** Reading the ciphertext and deleting the row happen in
  one transaction.
- **One 404 for everything.** Unknown, expired, already collected and another
  account's pairing all get the same 404.
- **Bounds.** Bodies are capped at 8 KiB and each string at 1024 characters.
- **Logging.** Ciphertext is never logged.
- **Cleanup.** A sweep deletes expired pairings hourly.
- **Gating.** Pairing is not gated on payment: a lapsed account still sets up
  devices to read. It is blocked during the deletion grace period, like the other
  sync routes.

## C. Billing

### C1. Model

- **Entitlements.** One table, `entitlements`, keyed by `(source, external_id)`.
  `source` is `web`, `app_store` or `play`. Each row records the user, product,
  status (`active`, `grace`, `on_hold`, `expired`, `revoked`), `will_renew`,
  `expires_at`, `grace_until`, `environment` (production or sandbox), `event_at`
  and the raw provider payload.
- **Who is entitled.** An account is entitled while any of its rows is active or
  in grace at `now()`, so expiry needs no job.
- **Event order.** `event_at` drops events older than the stored state, so a late
  webhook retry cannot roll an account back.
- **Store binding.** `users.store_account_token`, a UUID, is passed to StoreKit as
  `appAccountToken` and to Play as `obfuscatedAccountId`. That binds a store
  purchase to the account without exposing the OIDC `sub`.
- **One store account, two FUTO accounts.** A store subscription belongs to the
  FUTO account it was bought under, matched by its token and its
  `original_transaction_id` or purchase token.
  - **What happens when a second FUTO account shows up.** Someone signs in to a
    second FUTO account on the same Apple ID or Google account:
    - The store treats the subscription as already owned and will not sell a
      second one to that store account.
    - Submitting it, or "Restore Purchases", answers `409`
      `store_subscription_owned_elsewhere`. The server never moves it.
    - The app says: "This Apple ID's (Google account's) subscription belongs to
      another FUTO account. Sign in with that account, or subscribe on the web."
      It does not name the other account.
  - **Why not move it automatically.** Moving on restore would let anyone holding
    a shared store login take a subscription away from its owner, and Apple
    cannot change an `appAccountToken` after purchase anyway.
  - **When it can move.**
    - Support can move one with an admin action. The server just re-points the
      entitlement row.
    - Once the owning account is purged, the binding is released, so the next
      account to submit that subscription gets it.
- **Dedup.** `billing_deliveries (source, delivery_id)` deduplicates webhooks.

- **As built (S5a).**
  - `users.store_account_token` and `users.lapsed_at`, the `entitlements` and
    `billing_deliveries` tables: hosted migration `006_billing.sql`.
  - An event older than the stored `event_at` is ignored; an equal one is
    applied again, which changes nothing.
  - A store event's owner is its row's owner, or else the account its
    `AccountToken` names. A submission binds an unowned subscription to the
    submitter unless the token names another live account (`409`).
  - Entitled: a row `active` with `expires_at` after `now()`, or `grace` with
    `grace_until` after `now()`. `Principal.Entitled` comes from the same
    query as the session.
  - Applying an event that entitles the account clears `lapsed_at` and the
    three email stamps in the same transaction, unless the purge already
    claimed the account.

### C2. Pluggable providers

```go
type Provider interface {
	Source() string                                     // "web", "app_store", "play"
	Verify(ctx, delivery) ([]Event, error)              // webhook body + headers → normalized events
	Refresh(ctx, ref) (Event, error)                    // current truth, for reconciliation
	Cancel(ctx, ref) error                              // at period end; ErrNotSupported for Apple
}
type WebProvider interface {
	Provider
	CheckoutURL(ctx, user) (string, error)
	PortalURL(ctx, user) (string, error)
}
type StoreProvider interface {
	Provider
	Submit(ctx, user, purchase) (Event, error)          // app-posted transaction or purchase token
}
```

- **Web.** One web provider is configured: FUTOpay, Polar or Stripe, each an
  adapter.
- **Stores.** App Store: StoreKit 2 signed transactions, the App Store Server API,
  and Server Notifications v2. Play: the Play Developer API `subscriptionsv2`,
  with acknowledgement within 3 days, and real-time developer notifications over
  Pub/Sub.
- **Dev mocks.** Every adapter has a mock in the style of `cmd/mock-oidc`, and a
  `/dev` panel grants, lapses, refunds and expires subscriptions.

### C3. Endpoints

| Route | Purpose |
|---|---|
| `GET /api/billing` | Entitlement state, source, renewal, `store_account_token`, price shown per rail |
| `POST /api/billing/checkout` | Web checkout URL. Refused when any source is already active, to prevent paying twice. |
| `GET /api/billing/portal` | Web portal URL. A store source gets 409, and the app opens the store's own screen. |
| `POST /api/billing/app-store/transactions` | The app posts the JWS transaction; the server verifies it, binds it and upserts the entitlement |
| `POST /api/billing/play/purchases` | The app posts the purchase token; the server verifies, acknowledges and upserts |
| `POST /api/billing/webhooks/{provider}` | Signed provider notifications, deduplicated |

The capability document's `billing` becomes `true`.

As built (S5a): `GET /api/billing` answers `{entitled, status, source,
product_id, expires_at, grace_until, will_renew, store_account_token,
rails: {web, app_store, play}}`. The subscription fields are `null` for an
account that never subscribed. A rail is `true` when its provider is
configured; prices come from the stores' own SDKs and the web checkout, not
from this response. Webhooks answer `200 {"duplicate": bool}`, `400
invalid_delivery`, `404` for an unconfigured source, and `413` over 1 MiB.
The web provider is `BILLING_WEB_PROVIDER` (empty or `mock`; the mock needs
`DEV_UI`). The store providers register in `cmd/api/billing.go`.

### C4. Gating

- **Writes** answer `402` with code `subscription_required`, in the sync shape the
  apps already handle. Gated writes: object create, update and delete, the key
  `PUT` and collection delete.
- **Reads** are never gated, and neither are SSE, pairing, account, sessions,
  region or billing.
- **Storage access tokens** for an unentitled account carry a read-only scope,
  and storage refuses uploads without the write scope.
- **Precedence:** 401, then 403 (pending deletion), then 402.
- **As built (S5a).**
  - The gated writes are the five above plus the batch claim
    (`POST .../objects/batch`). Claiming a collection (`POST /api/collections`)
    is not gated.
  - The 402 body is `{"code":"subscription_required","error":"subscription required"}`.
  - The read-only scope is `blobs.read`. Storage answers uploads with it
    `403 write_not_allowed`.
  - **Gating follows billing.** With no provider configured, `billing` is
    `false` and nothing is gated, so a deployment with no way to pay stays
    writable. Production must configure a provider.

### C5. Lapse and retention

- A lapsed account stays read-only.
- 90 days after it lapses, the existing purge removes it. The purge first cancels
  any web or Play subscription it still finds.
- Resubscribing at any point before the purge ends the countdown.
- Emails (E):
  - when the account lapses;
  - 7 days before the purge;
  - when the purge is done.
- `DELETE /api/account` cancels web and Play subscriptions at the end of the
  period. Apple subscriptions cannot be cancelled from the server, so the app
  tells the user how to cancel one.

### C6. Jobs

All jobs run under `hostedjobs.locked`:

- **Entitlement reconciliation**, every 6 h: rows expiring within 24 h, or with
  no provider news for 7 days, are refreshed from the provider.
- **Delivery-log pruning** after 30 days.
- **Lapse purge** 90 days after lapse, sending the emails in E on the way.

As built (S5a):

- All three billing jobs run in the 6-hourly maintenance group, in this
  order: reconciliation, `subscription lapses`, account purge, delivery
  pruning.
- Reconciliation refreshes rows that are still live (`active`, `grace`,
  `on_hold`) and end within 24 h, or have had no update for 7 days. `expired`
  and `revoked` rows are final. A provider reports a resubscription itself.
- `subscription lapses` does the following:
  - It sets `lapsed_at` when an account that has subscribed stops being
    entitled. The value is when its entitlement ended: the paid period's
    end, or a refund or hold reported before then.
  - It sends the lapsed email, the warning at `lapsed_at + 83 days`, and
    then claims the purge at `lapsed_at + 90 days` (`purge_at = now()`) and
    purges.
  - A failed send holds that account's later steps until the next run, so
    nobody is purged unwarned.
  - An account without an email address is purged without emails.
  - Accounts pending a requested deletion are skipped: that purge comes
    sooner.
- The purge cancels web and Play renewals before deleting anything, and
  stays due while a cancellation fails. For a lapsed account, it then
  deletes the collections, sends the purged email, and deletes the user.
- The lapse emails carry a Resend `Idempotency-Key`,
  `lapse-email/<user>/<template>/<lapsed_at>`, and the purge date
  `lapsed_at + 90 days`, so a repeated send is the same message.

### C7. Config

- **Web provider:** its kind, plus its URL, key and webhook secret.
- **Apple:** bundle id, issuer, key id, `.p8` key, product ids, and whether to
  accept sandbox (App Review uses sandbox).
- **Play:** package name, service account, product ids, and the RTDN audience and
  service account.

All secrets come from the chart Secret, as today. The deployment guide gains the
outbound and inbound network paths these need.

## D. Client CI

- **Release artifacts.** The release publishes the `api`, `storage` and
  `mock-oidc` binaries alongside `server`. Today `scripts/build-release.sh` builds
  only `server`.
- **Compose bundle.** A pinned compose bundle starts Postgres, MinIO, one API, N
  storage regions and mock-oidc. The apps' harness pins the release and starts
  this bundle.
- **Sign-in in tests.** Test sign-ins use the mock issuer's `/test/authorize?sub=`
  shortcut, which replaces the old stand-in mode.

## E. Email

Built in `internal/hosted/mail` (S4). Nothing sends the lapse emails until
billing (S5) calls `Notifier.SendOnce`.

The API gains a mail sender, modelled on Backups (`~/src/backups/docs/email.md`):

- **Interface.** A `Mailer` interface with a **Resend** driver (`POST /emails` over
  `net/http`). It stamps the `From` address and a category tag.
- **Templates.** Go `html/template` and `text/template` files, embedded. Each email
  is `{subject, html, text}`.
- **Config.**
  - `RESEND_API_KEY` (secret).
  - `RESEND_API_URL`, `https://api.resend.com` by default.
  - `EMAIL_FROM_ADDRESS`, for example `FUTO Notes <noreply@notes.futo.cloud>` (the
    domain is to confirm).
- **Without a key** the sender logs and skips every send, so environments that don't
  care about email keep working.
- **Dev.** A mock Resend server delivers into Mailpit, both in `compose.hosted.yml`,
  so nothing leaves the machine. Tests assert through Mailpit's API.
- **No duplicate sends.** Each email has a stamp on the user
  (`lapse_notified_at`, `purge_warned_at`, `purge_notified_at`), set only after the
  send succeeds, so a retried job never sends twice.
- **Recipient.** The address comes from the ID token's `email` claim, refreshed at
  every sign-in.

## Order

1. **A1–A5, A7.** These fixes and B unblock the apps' sign-in and transport work.
2. **B1, B2.** Recovery envelope and pairing.
3. **D.** Lets the apps' CI run against the real hosted stack.
4. **A6.** Batch upload and batch claim.
5. **C, E.** Billing with all three providers, and the mail sender its lapse emails
   need. This gates the launch, since every rail ships at once. The web adapter
   waits until the web provider is chosen.

Each step carries the OpenAPI changes, conformance tests and `/dev` demos, and
passes the existing gates. The self-hosted contract stays byte-identical
throughout.

## Open questions

1. The web provider: FUTOpay, Polar or Stripe. To be decided later; the design does
   not depend on it.
2. The `From` domain for email.
