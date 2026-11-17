# Hosted launch plan

The server work that remains before the hosted service can launch, on top of what
[Hosted service plan](Hosted%20service%20plan.md) built (MR !34). It comes from two
research passes over the Notes apps (`futo-notes`) and the billing prior art. The client
half of the plan lives in the apps repo at `docs/plan/hosted-sync-regional.md`.

Status: **planned, not started.** Nothing below is built.

## Decisions (2026-11-17)

| Topic | Decision |
|---|---|
| Vault unlock under SSO | A **vault passphrase**, separate from the Zitadel login, **plus a recovery key and QR device pairing**. The apps already implement all three. The server adds what the recovery key and pairing need (B1, B2). |
| Redirect scheme | Apps sign in through the custom scheme `tech.futo.notes`. Desktop uses a loopback redirect (always allowed). |
| Hostnames | Prod `https://notes.futo.cloud`, staging `https://notes-staging.apps.futo.network`. |
| Payment providers | **Pluggable.** The web provider (FUTOpay, Polar or Stripe) is not chosen yet. App Store and Play are providers behind the same interface. |
| Store rails | Web, App Store and Play subscriptions **all ship at launch**. |
| Price | One flat plan: **$5/month** on the web, **+15% in the stores** (about $5.75, pending confirmation). |
| Lapse | The account turns **read-only** (writes get 402) and is **purged after N days**. N is still open. |
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
(32 bytes, chart Secret), and rotating it invalidates every unexchanged successor
once. The reuse rule moves from "spent" to "spent and its successor spent".

### A2. A stable `code` on every hosted error

`HostedError` gains a required `code` alongside `statusCode`, `message` and
`requestId`, and storage errors get the same field. Several cases currently share
a status and differ only in the message:

- 409 on `/api/storage/token`: `refresh_token_rotating` (gone after A1) and `region_required`;
- 401: `refresh_token_invalid` and `refresh_token_reused`;
- `PUT /api/account/region` 409: `region_already_chosen`;
- `POST /api/account/restore` 409: `not_pending_deletion` and `purge_started`;
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

- Set `appRedirectSchemes: [tech.futo.notes]` in the chart's prod and staging
  values, and `APP_REDIRECT_SCHEMES=tech.futo.notes` in `compose.hosted.yml` and
  the mise dev tasks.
- Open: does a dev build use its own scheme (for example `tech.futo.notes.dev`)
  so dev and release apps installed side by side don't collide?
- Claimed `https` app links stay refused for now.

### A6. Batch upload and batch claim

Without batching, each new object costs two round trips: upload to storage, then
claim at the API. A first sync of a large vault pays that for every object. Add:

- **Storage `POST /blobs/batch`.** Framed like the self-hosted
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
- `DELETE /api/account` with an old session: require a session that signed in
  within the last N minutes, or accept any session. Open.

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
- **Dedup.** `billing_deliveries (source, delivery_id)` deduplicates webhooks.

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

### C4. Gating

- **Writes** answer `402` with code `subscription_required`, in the sync shape the
  apps already handle. Gated writes: object create, update and delete, the key
  `PUT` and collection delete.
- **Reads** are never gated, and neither are SSE, pairing, account, sessions,
  region or billing.
- **Storage access tokens** for an unentitled account carry a read-only scope,
  and storage refuses uploads without the write scope.
- **Precedence:** 401, then 403 (pending deletion), then 402.

### C5. Lapse and retention

- A lapsed account stays read-only.
- After N days with no entitlement, the existing purge removes it. The purge
  first cancels any web or Play subscription it still finds.
- `DELETE /api/account` cancels web and Play subscriptions at the end of the
  period. Apple subscriptions cannot be cancelled from the server, so the app
  tells the user how to cancel one.
- Open: who emails users before a purge? The API has no mail sender.

### C6. Jobs

All jobs run under `hostedjobs.locked`:

- **Entitlement reconciliation**, every 6 h: rows expiring within 24 h, or with
  no provider news for 7 days, are refreshed from the provider.
- **Delivery-log pruning** after 30 days.
- **Lapse purge** after N days.

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

## Order

1. **A1–A5, A7.** These fixes and B unblock the apps' sign-in and transport work.
2. **B1, B2.** Recovery envelope and pairing.
3. **D.** Lets the apps' CI run against the real hosted stack.
4. **A6.** Batch upload and batch claim.
5. **C.** Billing with all three providers. This gates the launch, since every
   rail ships at once.

Each step carries the OpenAPI changes, conformance tests and `/dev` demos, and
passes the existing gates. The self-hosted contract stays byte-identical
throughout.

## Open questions

1. Retention after lapse: how many days (N) before the purge?
2. Store price: confirm that "+15%" means about $5.75 per month.
3. Does a dev build use its own redirect scheme?
4. Should `DELETE /api/account` require a recent sign-in?
5. Who sends lapse and purge emails?
6. Family Sharing on the App Store subscription; refunds revoking access at once.
7. One Apple ID or Google account bound to two FUTO accounts: refuse with 409, or
   allow re-pointing?
8. The web provider: FUTOpay, Polar or Stripe. The design does not depend on it.
