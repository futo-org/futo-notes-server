# Hosted deployment

How to run the hosted FUTO Notes service on Kubernetes. The design is in
[Hosted service plan](Hosted%20service%20plan.md); local development is in
[DEVELOPMENT.md](DEVELOPMENT.md#hosted-development). Ingress, HPA,
NetworkPolicy and ExternalSecrets live in the infra repo, not here.

## What to deploy

Charts are published to `oci://oci.futo.org/futo-notes/charts` as
`0.0.<pipeline>` on every main merge, with `appVersion` set to the image tag
they ship with. Images are `oci.futo.org/futo-notes/notes-api` and
`oci.futo.org/futo-notes/notes-storage`. CI pushes both with the masked GitLab
variables `OCI_REGISTRY_USER` and `OCI_REGISTRY_PASSWORD`.

| Release | Chart | Count | Notes |
|---|---|---|---|
| `notes-db` | `cnpg-cluster` | 1 | CloudNativePG `Cluster`, plus barman-cloud backups when enabled. Needs the CNPG operator (and `plugin-barman-cloud` for backups) in `cnpg-system`. |
| `notes-storage-<region>` | `storage` | 3 | One per region: `us-east`, `eu-central`, `ap-southeast`. Two replicas each. |
| `notes-api` | `api` | 1 | Three replicas. Runs the migration hook Job. |

Install order on a fresh environment:

1. CNPG operator (and the barman-cloud plugin), then `cnpg-cluster`. Wait for
   the cluster to be ready and for its `notes-db-app` Secret to exist.
2. The Secrets below (ExternalSecrets in prod).
3. The three `storage` releases.
4. `api`. Its pre-install hook migrates the empty database before any pod
   starts.

Upgrades need no order, except where a key rotation (below) says otherwise.

## Values per environment

The charts' `values.yaml` carry **dev** defaults (MinIO, mock OIDC, debug logs,
plain-http cookies). Every prod release sets the following.

`cnpg-cluster`:

| Value | Prod |
|---|---|
| `clusterName` | `notes-db` (the API chart's `postgresClusterName` must match) |
| `instances` | 3 |
| `storage`, `storageClass`, `resources` | sized for the environment |
| `affinity` | `podAntiAffinityType: required` |
| `synchronous` | as the durability target requires; empty is async |
| `backup.enabled`, `.bucket`, `.endpointURL`, `.secretName` | on, with the object store and its Secret |

`storage` (each region):

| Value | Prod |
|---|---|
| `region` | the region id. Names the release's objects `notes-storage-<region>`, sets `REGION_ID`, and the bucket `notes-<region>` |
| `s3Bucket` | only if the bucket is not `notes-<region>` |
| `secretData` | `null` (the ExternalSecret provides the Secret) |
| `env` | replaces the whole list: `STORAGE_PORT=3015`, `LOG_LEVEL=info`, `S3_ENDPOINT`, `S3_REGION`, `S3_FORCE_PATH_STYLE` (true only for stores without virtual-host buckets), `OTLP_METRICS_ENDPOINT`, `OTLP_METRICS_URL_PATH`. Leave `DRAIN_DELAY_MS` and `SHUTDOWN_TIMEOUT_MS` unset (15 s and 120 s). |

`api`:

| Value | Prod |
|---|---|
| `publicUrl` | the external origin, no path: `https://notes.futo.cloud` in prod, `https://notes-staging.apps.futo.network` in staging |
| `oidcIssuer` | the Zitadel instance URL, exactly as its discovery document's `issuer` |
| `oidcClientId` | the Zitadel application's client id |
| `appRedirectSchemes` | the custom URL schemes the apps sign in through: `[tech.futo.notes]` (the release apps, and the chart default) in prod, `[tech.futo.notes, tech.futo.notes.dev]` in staging so dev builds can sign in too. Loopback `http://127.0.0.1:<port>` and `http://[::1]:<port>` are always allowed (desktop); `http` and `https` are refused, so claimed `https` app links are not accepted. |
| `allowedEmailDomains` | renders `ALLOWED_EMAIL_DOMAINS`: the email domains a **new** account may sign up with, e.g. `[futo.org, futo.tech]`; `[]` (the chart default) lets anyone sign up. See Sign-up restriction below. |
| `regions` | the regions file (format below) |
| `resendApiUrl` | `https://api.resend.com` (the default; `RESEND_API_URL`) |
| `emailFromAddress` | the `From` of every email (`EMAIL_FROM_ADDRESS`). The default, `FUTO Notes <noreply@notes.futo.cloud>`, is a **placeholder**: confirm the sending domain, verify it in Resend, then set this. |
| `postgresClusterName` | `notes-db` |
| `stripe.priceId` | the $5/month plan's Stripe price id; turns Stripe on (see Billing: web provider) |
| `stripe.publishableKey` | `pk_live_...` in prod, `pk_test_...` in staging |
| `stripe.selfPaymentCountries` | renders `STRIPE_SELF_PAYMENT_COUNTRIES`: the billing countries that pay on our own checkout page, e.g. `[US]`; `[]` (the chart default) sends every country to Stripe Managed Payments (see Billing: web provider) |
| `secretData` | `null` |
| `env` | replaces the whole list: `API_PORT=3005`, `LOG_LEVEL=info`, `OTLP_METRICS_ENDPOINT`, `OTLP_METRICS_URL_PATH`. Drop the dev entries `COOKIE_SECURE=false` and `DRAIN_DELAY_MS=1000`: the defaults are a `Secure` cookie and 15 s. Never set `DEV_UI` (it serves `/dev`, which runs storage jobs on demand). |

`useDevKeys` stays `false` in every chart outside local development: the dev
keys are committed.

## Secrets

| Secret | Keys | Source |
|---|---|---|
| `notes-db-app` | `host`, `port`, `dbname`, `username`, `password` | Generated by CNPG. The API pods and the migration Job map it to `POSTGRES_*`. |
| CNPG backup (`backup.secretName`) | `ACCESS_KEY_ID`, `ACCESS_SECRET_KEY`, `CA_CERT` | The backup object store. barman cannot skip TLS verification, so `CA_CERT` is required. |
| `notes-api` | `OIDC_CLIENT_SECRET` | The Zitadel application's client secret. |
| | `API_SIGNING_KEY` | One Ed25519 private key PEM (with `kid`): signs storage access and admin tokens. |
| | `STORAGE_REFRESH_ROTATION_KEY` | 32 random bytes, base64 (`openssl rand -base64 32`): derives each storage refresh token's successor, `base64url(HMAC-SHA256(key, token))`, so an exchange whose response was lost can be repeated. The API refuses to start without it. |
| | `RESEND_API_KEY` | A Resend API key with sending access for the `emailFromAddress` domain. Without it the API logs and skips every email (the lapse and purge notices), and starts anyway. |
| | `STRIPE_SECRET_KEY` | With Stripe: the secret key (`sk_live_...`), or a restricted key (`rk_...`) with write access to Customers, Checkout Sessions, Subscriptions and Customer Portal sessions. Staging uses the sandbox's `sk_test_...`. The API refuses a live key while `DEV_UI` is on. |
| | `STRIPE_WEBHOOK_SECRET` | With Stripe: the webhook endpoint's signing secret (`whsec_...`). |
| `notes-storage-<region>` | `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | Credentials for the region's bucket: get, put, list and delete objects. |
| | `API_PUBLIC_KEYS` | The public half of the API signing key, one PEM block per `kid` (several during rotation). Same value in every region. |
| | `RECEIPT_SIGNING_KEY` | This region's Ed25519 private key PEM (with `kid`): signs upload receipts. |

Generate a key pair with `mise run keygen -kid <kid> [-out <prefix>]`
(`cmd/hosted-keygen`). It prints, or writes, the private block (`.key.pem`)
and the public block (`.pub.pem`), each with a `kid:` PEM header. Use a kid that
names the key and its generation, for example `api-2026-11` or
`eu-central-2026-11`. Kids must be unique within a key set.

Pods read Secrets only at startup. After changing one, restart the Deployment
(`kubectl rollout restart deployment/<name>`).

## Billing: web provider

Billing is on when any payment provider is configured: the capability
document says `"billing": true`, writes from an account without a
subscription answer `402 subscription_required`, and its storage tokens are
read-only (`blobs.read`). With no provider, billing is off and nothing is
gated. The App Store and Play providers have their own settings (below).

The web provider is **Stripe**, selling one plan at $5/month (plus tax
where Stripe sells it as merchant of record).

| Variable | Where | What |
|---|---|---|
| `BILLING_WEB_PROVIDER` | rendered by the chart | `stripe` when `stripe.priceId` is set. Empty (the default) for none. `mock` is the dev and test-stack mock, whose checkout and portal are `/dev/billing` pages; the API refuses to start with it unless `DEV_UI=true`, so it never runs in production. |
| `STRIPE_PRICE_ID` | `stripe.priceId` | The plan's price (`price_...`). Required with `stripe`. |
| `STRIPE_PUBLISHABLE_KEY` | `stripe.publishableKey` | Mounts the checkout's card fields (Stripe.js) in the browser; not secret. Must be the same mode (live or test) as the secret key. Required with `stripe`. |
| `STRIPE_SELF_PAYMENT_COUNTRIES` | `stripe.selfPaymentCountries` | Comma-separated ISO 3166-1 alpha-2 billing countries, any case (`US,CA`), that pay on our own checkout page with FUTO as merchant of record. Empty (the default): every country pays through Stripe Managed Payments. An unknown code stops the API from starting. |
| `STRIPE_SECRET_KEY` | Secret | **Secret.** See Secrets. Required with `stripe`. |
| `STRIPE_WEBHOOK_SECRET` | Secret | **Secret.** See Secrets. Required with `stripe`. |
| `STRIPE_API_URL` | tests only | Replaces `https://api.stripe.com`. Never in a chart. |
| `BILLING_MOCK_WEBHOOK_SECRET` | dev and test stack only | With `mock`: the secret (16 characters or more) the mock signs its webhook deliveries with. Never in a chart. |

How it works:

- **Accounts are Stripe Customers keyed by OIDC `sub`.** The first checkout
  finds the account's Customer by `metadata.sub` (Stripe's customer search)
  or creates one. Every Checkout Session carries `client_reference_id` and
  `subscription_data.metadata.sub` set to the `sub`, and webhooks and
  refreshes find the account from the subscription's `metadata.sub`, or
  else its Customer's. Nothing is ever matched by email; the email is only
  copied onto the Customer for receipts.
- **Checkout starts with the billing country.** Subscribe on the dashboard,
  and the app's checkout link, first show a country list (English names,
  ISO 3166-1 alpha-2 codes plus Kosovo, Ascension Island and Tristan da
  Cunha) prefilled from the browser's `Accept-Language` (`en-GB` is the
  United Kingdom; nothing else, no IP lookup) and a Checkout button. The
  server checks the posted country again and routes it:
  - in `STRIPE_SELF_PAYMENT_COUNTRIES`: **our own page** (below), FUTO
    the merchant of record;
  - China, Cuba, Iran, Kosovo, North Korea, Russia, Syria, Ascension
    Island and Tristan da Cunha, which Managed Payments refuses (unless
    they are self-payment countries): "Paying on the web isn't available
    in your country yet.", and no Checkout;
  - every other country: **Stripe Managed Payments**, Stripe the merchant
    of record. The server opens a Checkout Session with
    `managed_payments[enabled]=true`, `mode=subscription`,
    `ui_mode=hosted_page`, the same price, the account's Customer and the
    same `client_reference_id` and `metadata.sub`, and redirects the
    browser to Stripe's hosted page. Stripe adds the tax due on top of the
    $5 price, may show it in the buyer's currency (Adaptive Pricing is
    always on there), and picks the payment methods. Its success URL is
    the same return as our page's, which confirms the session at once;
    its cancel URL is the country step (the dashboard's, or the app link's
    "No payment was taken" page). The session sends none of the
    parameters Managed Payments forbids (`adaptive_pricing`,
    `automatic_tax`, `tax_id_collection`, payment method types or
    configuration, `customer_update`, shipping, invoice settings). For
    these subscriptions Stripe does not let the API change
    `default_payment_method`; the server never does.
- **On our own page,** the server opens a
  Checkout Session with `ui_mode=elements`; our page shows the plan (its
  amount and period read from the price), the account's email, Stripe's
  Payment Element for the card (in Stripe's frames, so card data never
  touches our pages: PCI SAQ A), and our own Subscribe button, which
  confirms the session with Stripe.js (`initCheckoutElementsSdk`). The
  Payment Element is styled through Stripe's Appearance API from the
  dashboard's colour tokens and Barlow, and follows the system's light or
  dark scheme as it changes. The dashboard's Subscribe opens it inside the
  overview. The apps' `POST /api/billing/checkout` answers a one-time link,
  `<publicUrl>/billing/checkout/<token>` (HMAC-signed, names the account,
  15 minutes, opens one checkout), to the country step without a dashboard
  session. Opening or reloading it uses nothing; posting Checkout uses it,
  so a link preview never burns it. A self-payment country then gets a
  second one-time token for that country, `<publicUrl>/billing/checkout/<pay token>`,
  our checkout page; any other is sent to Stripe. The token key is derived
  from `STRIPE_SECRET_KEY`, so rotating that key also voids outstanding
  links.
- **Everything the checkout must not offer is off per session, in code.**
  Each Checkout Session sets `adaptive_pricing[enabled]=false` (the
  customer pays the price's own USD amount, never a converted one),
  `allowed_payment_method_types[]=card` and
  `wallet_options[link][display]=never`; the page also hides Apple Pay,
  Google Pay and Link in the Payment Element. None of it depends on the
  Stripe account's dashboard settings.
- **Entitlement does not wait for the webhook.** When the checkout
  completes, the page asks the server to confirm it: the server fetches the
  Checkout Session, checks it is the caller's (`sub`), and applies its
  subscription at once. Every later change (renewal, failed payment,
  cancellation, deletion) arrives by webhook.
- **The pages that host checkout** (the overview while unsubscribed, its
  subscribe view, and our page under a pay token) load Stripe.js from
  `js.stripe.com`, which Stripe forbids self-hosting, and are served a
  Content-Security-Policy widened by exactly what Stripe documents for
  Stripe.js ([security guide](https://docs.stripe.com/security/guide#content-security-policy)):
  `script-src` `js.stripe.com`, `*.js.stripe.com`; `frame-src` the same
  plus `hooks.stripe.com` (3-D Secure); `connect-src` `api.stripe.com`.
  Link, wallets and the Address Element are off, so nothing else is
  allowed. Every other page, including the app link's country step and
  the checkout's return and cancel pages, keeps the strict policy. The
  redirects to Stripe's hosted page are htmx `HX-Redirect`s (full page
  loads), which the policy's `form-action 'self'` does not govern.
- **The portal** (Manage subscription) is a Stripe Customer Portal session
  that returns to `/dashboard`.

Subscription states map to entitlements as follows:

| Stripe `status` | Entitlement | Entitles |
|---|---|---|
| `active`, `trialing` | `active` until the period ends | yes |
| `past_due` | `grace` until the current period ends | yes |
| `unpaid`, `paused`, `incomplete` | `on_hold` | no |
| `canceled`, `incomplete_expired` | `expired` | no |

The period end is the subscription item's `current_period_end` (at the
pinned API version, `2026-10-28.endive`, it lives on the items). A
subscription renews unless `cancel_at_period_end` is set or `cancel_at`
falls at or before the period's end.

Providers notify the API at `POST <publicUrl>/api/billing/webhooks/{web,app_store,play}`.
These routes skip session auth and are authenticated by each provider's
signature, so the ingress must admit them from the provider (see Network
paths). Each delivery's id is kept 30 days to drop retries.

### Stripe dashboard setup

Do this once per account (live for prod, the sandbox for staging):

1. **Product and price.** A product "FUTO Notes Sync" with one recurring
   price, $5.00 USD per month. Put the price id in `stripe.priceId`. Give
   the product the tax code **`txcd_10103000`** (Software as a service,
   personal use), which Managed Payments requires to be one of its
   eligible codes: `stripe products update <prod_id> --tax-code txcd_10103000`.
   Leave the price's tax behavior exclusive (or unspecified, which Managed
   Payments treats as exclusive): the $5 is before tax, and Stripe adds it.
   A dev copy, "FUTO Notes Sync (dev)" with lookup key
   `futo-notes-sync-dev-monthly`, exists in the sandbox with that tax code;
   the sandbox tests reuse it.
2. **Webhook endpoint.** Developers, Webhooks, Add endpoint:
   `https://notes.futo.cloud/api/billing/webhooks/web` (staging:
   `https://notes-staging.apps.futo.network/api/billing/webhooks/web`),
   listening to exactly `customer.subscription.created`,
   `customer.subscription.updated` and `customer.subscription.deleted`. Its
   signing secret goes in the Secret as `STRIPE_WEBHOOK_SECRET`. The
   payload version does not matter: the server reads only the event id,
   type and subscription id, and fetches the subscription itself.
3. **Customer Portal.** Settings, Billing, Customer portal: allow cancelling
   subscriptions (at the end of the period) and updating payment methods;
   do **not** allow switching plans or changing quantities. Save it, which
   also creates the default configuration the API's portal sessions use.
4. **Managed Payments.** Activate it once under Settings, Managed
   Payments (the sandbox's already is). Without it, every checkout outside
   the self-payment countries fails. Nothing else is set there: the
   server enables it per Checkout Session.
5. **Checkout.** Nothing to set for our page: the currency, payment
   methods and look are all set per session and on our page (see above),
   whatever the account's Adaptive Pricing, payment method and branding
   settings say. Stripe's hosted page for Managed Payments follows the
   account's branding settings.
6. **Tax.** Under Managed Payments Stripe calculates, collects and remits
   the tax as merchant of record. On our own page (the self-payment
   countries) no tax is calculated or collected; Stripe Tax is out of
   scope for it.

### Local development with the Stripe CLI

See [DEVELOPMENT.md](DEVELOPMENT.md#stripe-sandbox): `stripe listen`
forwards sandbox webhooks to `api:dev`.

The billing jobs run in the 6-hourly maintenance group, under the job lock:

- `entitlement reconciliation` asks providers about live subscriptions that
  end within a day or have had no news for a week.
- `subscription lapses` records when an account stops being entitled
  (`users.lapsed_at`), emails it then, warns it 83 days later, and purges it
  90 days later through the account purge. Resubscribing at any point
  before the purge clears the lapse and its email stamps.
- `account purge`, before deleting an account (requested or lapsed), stops
  its web and Play subscriptions renewing. App Store subscriptions cannot be
  cancelled from the server. `DELETE /api/account` does the same at once.
- `billing delivery pruning` drops delivery ids older than 30 days.

Each email carries a Resend `Idempotency-Key` made of the user, the email and
the lapse, so a send whose stamp failed to save is not delivered twice.

## Regions file

`api`'s `regions` value is rendered into the ConfigMap `notes-api-regions` as
`regions.json`, mounted at `/etc/notes-api/regions.json` (`REGIONS_FILE`). A
change to it rolls the API pods through a checksum annotation. It is a JSON
array. Unknown fields are rejected, and the API refuses to start on any error.

```json
[
  {
    "id": "eu-central",
    "name": "EU Central",
    "description": "Frankfurt, Germany",
    "endpoint": "https://eu-central.storage.notes.futo.org",
    "receipt_public_keys": "-----BEGIN PUBLIC KEY-----\nkid: eu-central-2026-11\n\nMCowBQYDK2VwAyEA...\n-----END PUBLIC KEY-----\n"
  }
]
```

- `id` must equal that region's storage `REGION_ID`. It is the `aud` of every
  token the API mints for the region and the `iss` of its receipts.
- `endpoint` is the storage service's public base URL. Clients get it from
  `GET /api/regions` and `POST /api/storage/token`, and the API's jobs call
  `<endpoint>/internal/*` on it too.
- `receipt_public_keys` lists every kid the region may sign receipts with: one
  normally, two during rotation.

A region, once users have chosen it, must never be removed or renamed: their
blobs live there.

## Zitadel application

One application in the Notes project serves both the dashboard and the apps,
because both sign in **through the API**. The apps never talk to Zitadel
directly.

- Type **Web**, authentication method **Code** (client secret, sent as HTTP
  Basic). The API is a confidential client and also sends a PKCE S256
  challenge on every sign-in, which Zitadel verifies.
- **Redirect URI**: exactly `<publicUrl>/api/auth/oidc/callback`. This is the only
  one. The dashboard and app flows both return there. The API then sets the
  dashboard cookie, or sends the app to *its* `redirect_uri` with a one-time
  code. The app's redirect target is checked by the API against
  `appRedirectSchemes` and loopback rules, and is not registered in Zitadel.

  | Environment | Redirect URI |
  |---|---|
  | Prod | `https://notes.futo.cloud/api/auth/oidc/callback` |
  | Staging | `https://notes-staging.apps.futo.network/api/auth/oidc/callback` |
  | Local | `http://localhost:3005/api/auth/oidc/callback`, only when pointing local dev at a real Zitadel application instead of mock-oidc. That application needs Dev mode on, because the URI is plain `http`. |
- No post-logout redirect URI: signing out ends only the Notes session.
- Scopes requested: `openid profile email`.
- Reauthentication: before deleting an account the API sends `max_age=0`
  and requires the ID token's `auth_time` to be under 10 minutes old. Zitadel
  honours `max_age` and sets `auth_time`, so there is nothing to configure.
- Token settings: enable **User Info inside ID Token**. The API reads `email`,
  `name` and `preferred_username` from the ID token only and never calls
  userinfo.
- Dev mode off (it allows `http` redirect URIs).
- **Confirm `sub` is public, not pairwise.** Users are keyed by `sub`. Zitadel's
  default `sub` is the user's global id, the same in every application of the
  instance. Check it once by signing the same person in to Notes and to another
  FUTO application and comparing `sub`. A pairwise or per-application subject
  would give the same person a different account in each service (see
  [MANAGED-LAUNCH.md](MANAGED-LAUNCH.md)). Email is not unique on hosted: the
  identity provider owns it, and an address can move between accounts.
- `email_verified` must be in the ID token (the same **User Info inside ID
  Token** setting) whenever `allowedEmailDomains` is set; without it every
  new account is refused.

## Sign-up restriction

`ALLOWED_EMAIL_DOMAINS` (chart value `allowedEmailDomains`) limits who can
create an account. It is a comma-separated list such as
`futo.org,futo.tech`; entries are trimmed and lowercased, and the API
refuses to start on one containing `@`, `*` or a space, or without a dot.
Empty or unset, the default, lets anyone sign up.

- It applies only when a sign-in would **create** an account, that is, no
  account has the ID token's `sub` yet. **Existing users are never locked
  out**: an account that exists signs in even if its domain was removed from
  the list or its email changed to another domain.
- A new account needs `email_verified: true` in the ID token and an email
  whose domain, the part after the last `@`, is exactly one listed, in any
  case. Subdomains are not implied: list `eng.futo.org` to admit it.
- A refused dashboard sign-in gets a `403` page, "Sign-ups are limited to
  approved email domains.", and an app's sign-in returns to its
  `redirect_uri` with `error=signup_not_allowed`. Neither creates an account
  or a session, and neither names the allowed domains. `auth.logins` counts
  them as `outcome=signup_not_allowed`.
- The capability document's `signup` stays `"open"`.
- To try it locally, set `ALLOWED_EMAIL_DOMAINS` for `mise run api:dev`
  (`HOSTED_STACK_ALLOWED_EMAIL_DOMAINS` for the hosted test stack); `/dev`
  shows whether sign-up is restricted, and mock-oidc's sign-in form has an
  "Email not verified" box (`email_verified=false` on `/test/authorize`).

## App Store and Google Play

Store billing is optional per store. A store is **on** when every one of its
required variables is set, **off** when none is, and the API refuses to start
when only some are. The chart sets the non-secret ones from its `appStore` and
`play` values, but only when `appStore.bundleId` or `play.packageName` is set.
The keys come from the `notes-api` Secret.

| Variable | Chart value or Secret key | What it is |
|---|---|---|
| `APPLE_BUNDLE_ID` | `appStore.bundleId` | `tech.futo.notes` |
| `APPLE_APP_APPLE_ID` | `appStore.appAppleId` | The app's numeric Apple ID (App Store Connect, App Information). Production notifications must carry it. |
| `APPLE_ISSUER_ID`, `APPLE_KEY_ID` | `appStore.issuerId`, `appStore.keyId` | The In-App Purchase key's issuer and key id. |
| `APPLE_PRIVATE_KEY` | Secret | **Secret.** The In-App Purchase key's `.p8` file, as is (PKCS #8 PEM, P-256). |
| `APPLE_SUBSCRIPTION_PRODUCT_IDS` | `appStore.subscriptionProductIds` | The auto-renewable subscription product ids that entitle, comma-separated. |
| `APPLE_ACCEPT_SANDBOX` | `appStore.acceptSandbox` | Optional. `true` in prod and staging (the chart default): App Review and TestFlight purchases are sandbox transactions in the release build. The API's own default is `false`. Sandbox rows are stored with `environment = sandbox`. |
| `APPLE_STOREKIT_XCODE` | none | Never set outside development: it accepts unsigned Xcode StoreKit testing purchases, and the API refuses it unless `DEV_UI=true` (docs/DEVELOPMENT.md). |
| `PLAY_PACKAGE_NAME` | `play.packageName` | `tech.futo.notes` |
| `PLAY_SERVICE_ACCOUNT_JSON` | Secret | **Secret.** The Play API service account's JSON key file. |
| `PLAY_SUBSCRIPTION_PRODUCT_IDS` | `play.subscriptionProductIds` | The subscription product ids that entitle, comma-separated. |
| `PLAY_RTDN_AUDIENCE` | `play.rtdnAudience` | The audience of the push subscription's OIDC token. |
| `PLAY_RTDN_SERVICE_ACCOUNT` | `play.rtdnServiceAccount` | The service account email the push subscription authenticates as. |

### Webhooks and network paths

| From | To | Why |
|---|---|---|
| Apple | `<publicUrl>/api/billing/webhooks/app_store` (POST) | App Store Server Notifications V2. Authenticated by the JWS inside the body. |
| Pub/Sub | `<publicUrl>/api/billing/webhooks/play` (POST) | Play real-time developer notifications (RTDN). Authenticated by a Google-signed OIDC bearer token. |
| API pods | `api.storekit.itunes.apple.com`, `api.storekit-sandbox.itunes.apple.com` (HTTPS) | App Store Server API: a subscription's status when the app submits a purchase, and at reconciliation. |
| API pods | `androidpublisher.googleapis.com` (HTTPS) | Play Developer API: `subscriptionsv2.get`, acknowledge and cancel. |
| API pods | `oauth2.googleapis.com` (HTTPS) | Access tokens for the Play service account (its key file's `token_uri`). |
| API pods | `www.googleapis.com` (HTTPS, `/oauth2/v3/certs`) | Google's keys for the push tokens. Cached, and fetched again only for an unknown `kid`. |

The ingress must let the two webhook routes through without any other
authentication. A delivery that fails verification gets `400`.

Apple's signatures are checked offline: the `x5c` chain must lead to Apple Root
CA - G3 (embedded in the binary), carry Apple's App Store marker extensions, and
be valid at the payload's `signedDate`. No OCSP request is made.

### App Store Connect

1. **Subscription.** Create the auto-renewable subscription in a subscription
   group, with **Family Sharing off**. A family-shared transaction never
   entitles. Put its product id in `appStore.subscriptionProductIds`.
2. **In-App Purchase key.** Users and Access, Integrations, In-App Purchase:
   generate a key. Download the `.p8`, which can be downloaded only once, into
   the Secret as `APPLE_PRIVATE_KEY`. The key id goes in `appStore.keyId`, and
   the issuer id shown on that page in `appStore.issuerId`.
3. **Server notifications.** App Information, App Store Server Notifications.
   Choose **Version 2** for both URLs:

   | Field | URL |
   |---|---|
   | Production Server URL | `https://notes.futo.cloud/api/billing/webhooks/app_store` |
   | Sandbox Server URL | `https://notes.futo.cloud/api/billing/webhooks/app_store` |

   Sandbox points at prod too, because TestFlight and App Review use the
   release build, which talks to prod. One app record has one URL of each
   kind, so staging receives no notifications. It still learns of purchases
   when the app submits them, and from reconciliation.

Apple retries a notification that does not get a `200`. `TEST` notifications
are answered `200` and change nothing.

### Google Play

1. **Subscription.** Play Console, Monetize, Subscriptions. Put the
   subscription's product id in `play.subscriptionProductIds`.
2. **API service account.** In the Google Cloud project, enable the *Google
   Play Android Developer API* and create a service account, for example
   `play-api@<project>.iam.gserviceaccount.com`. Create a JSON key for it, into
   the Secret as `PLAY_SERVICE_ACCOUNT_JSON`. In Play Console, Users and
   permissions, invite the service account's email with these permissions on
   the app: **View financial data** and **Manage orders and subscriptions**.
3. **RTDN topic.** Create a Pub/Sub topic, for example `play-rtdn`. Grant
   `google-play-developer-notifications@system.gserviceaccount.com` the
   **Pub/Sub Publisher** role on it. In Play Console, Monetization setup,
   Real-time developer notifications, enter
   `projects/<project>/topics/play-rtdn` and choose to receive subscriptions
   and voided purchases. **Send test notification** should get a `200`.
4. **Push subscription.** Create a push subscription on the topic:
   - endpoint `https://notes.futo.cloud/api/billing/webhooks/play`;
   - **Enable authentication**, with a service account of its own, for example
     `rtdn-push@<project>.iam.gserviceaccount.com`. That email goes in
     `play.rtdnServiceAccount`;
   - an explicit **audience**, for example the endpoint URL. That goes in
     `play.rtdnAudience`;
   - exponential-backoff retry.

   If the project predates April 2021, also grant the Pub/Sub service agent
   (`service-<project-number>@gcp-sa-pubsub.iam.gserviceaccount.com`) the
   **Service Account Token Creator** role on the push service account. Staging
   gets a second push subscription on the same topic, pointing at its own
   origin and with its own audience.

Notifications only name a purchase token. The API always fetches its state
with `subscriptionsv2.get`, and never trusts the message. A purchase the app
submits, or one a notification reports, is acknowledged at once while it
entitles and carries the account id the app sets, because Play refunds one
left unacknowledged for three days. License testers' purchases are stored with `environment = sandbox`.

## Key rotation

Every verifier accepts a **set** of public keys by `kid`, and every signer uses
**one** private key. So rotation is always the same three steps: trust the new
key everywhere, switch the signer, then distrust the old key once nothing it
signed can still be valid. Nothing goes down at any point.

### API signing key (access and admin tokens)

Access tokens live 24 h; admin tokens live 5 min.

1. `mise run keygen -kid api-<date> -out api-<date>`.
2. In **every** region's `notes-storage-<region>` Secret, set `API_PUBLIC_KEYS`
   to the old **and** new public blocks (concatenated). Restart all three
   storage Deployments and wait for the rollouts to finish. Storage now accepts
   both keys.
3. In `notes-api`, set `API_SIGNING_KEY` to the new private block. Restart the
   API Deployment. New tokens carry the new kid.
4. Wait at least 24 h plus clock skew, so every access token signed with the old key has
   expired.
5. Remove the old public block from every region's `API_PUBLIC_KEYS` and
   restart the storage Deployments.

Doing step 3 before step 2 has finished in every region makes that region
reject new tokens with 401.

### Storage refresh rotation key

No overlap is needed. Set `STORAGE_REFRESH_ROTATION_KEY` in `notes-api` to a
new `openssl rand -base64 32` and restart the API Deployment. Every device's
newest refresh token keeps working, because the API stores each successor when
it first issues it. The only casualty is a device that presents a token it
already exchanged under the old key (its response was lost across the
restart): it gets `401` `refresh_token_invalid` and signs in again. Its
session is not revoked.

### Region receipt key

Receipts live 24 h.

1. `mise run keygen -kid <region>-<date> -out <region>-<date>`.
2. In the `api` release, append the new public block to that region's
   `receipt_public_keys` and upgrade. The pods roll by themselves, and the API
   accepts both.
3. In `notes-storage-<region>`, set `RECEIPT_SIGNING_KEY` to the new private
   block and restart that region's Deployment.
4. Wait at least 24 h plus clock skew, so every receipt signed with the old
   key has expired.
5. Remove the old public block from `receipt_public_keys` and upgrade `api`.

Doing step 3 before step 2 makes object writes in that region fail with
`400 invalid receipt` until the API trusts the new key.

## Migrations

`api migrate` applies the shared Postgres migrations and then the hosted
track (bookkeeping table `hosted_migrations`). The chart runs it as a Helm
**pre-install/pre-upgrade** hook Job (`notes-api-migrate`):

- It runs before any of the release's objects change, with only `POSTGRES_*`
  from `notes-db-app`. `backoffLimit: 10` rides out a database that is still
  starting.
- If it fails, the release fails and the previous pods keep serving. A failed
  Job is kept for its logs and replaced on the next attempt.
- API pods **refuse to start** while either track has migrations pending. So
  with `migration.enabled: false` you must run `api migrate` yourself first.
- Old pods keep serving against the migrated schema during the rollout, and
  `helm rollback` does not undo migrations. **Every migration must stay
  backward compatible with the previous release**. Make destructive
  changes (drop, rename, `NOT NULL` on existing data) in two releases: first stop
  using the column, then drop it.

## Network paths

| From | To | Why |
|---|---|---|
| clients | API `publicUrl` (HTTPS) | sync, SSE (long-lived; disable proxy buffering and idle timeouts under a few minutes), sign-in, dashboard |
| clients | each region's `endpoint` (HTTPS) | uploads up to 100 MiB and downloads. Allow request bodies of 100 MiB, and transfers that take many minutes. |
| browsers | Zitadel | the sign-in page |
| API pods | each region's `endpoint`, path `/internal/*` | blob GC, orphan reconciliation and account purge. The API calls the same `endpoint` clients use. If the ingress restricts `/internal/*`, it must still admit the API's egress. These routes require an admin JWT either way. |
| API pods | Zitadel issuer | discovery, code exchange and JWKS |
| API pods | `resendApiUrl` (HTTPS, `POST /emails`) | lapse and purge emails, when `RESEND_API_KEY` is set |
| API pods | `api.stripe.com` (HTTPS) | Stripe: customers, checkout and portal sessions, subscriptions |
| browsers | `js.stripe.com`, `*.js.stripe.com`, `api.stripe.com`, `hooks.stripe.com` | Stripe.js and the checkout's card fields, on the pages that host them |
| browsers | `checkout.stripe.com` | Stripe's hosted Checkout, where buyers outside the self-payment countries pay (Managed Payments) |
| payment providers | API `publicUrl`, path `/api/billing/webhooks/*` | subscription notifications; authenticated by the provider's signature, not a session. Stripe posts to `/api/billing/webhooks/web` from [its webhook IPs](https://docs.stripe.com/ips) |
| API pods | `notes-db-rw` | Postgres, including `LISTEN/NOTIFY` for SSE across replicas |
| storage pods | the region's S3 | blob bytes |
| all pods | OTLP endpoint | metrics |

Storage never calls the API. Replicas of the API never call each other.

## Observability

- **Logs**: zerolog JSON on stderr, one access line per request, with
  `request_id` (returned to clients as `X-Request-Id`). `LOG_LEVEL` is
  `trace`, `debug`, `info` (default), `warn`, `error`, `fatal`, `panic` or
  `disabled`, and `LOG_FORMAT` is `json` (default) or `pretty`, the same in
  api, storage and mock-oidc. Any other value refuses to start.
- **Metrics**: OTLP over HTTP, pushed every `OTLP_METRICS_INTERVAL_MS` (15 s)
  to `OTLP_METRICS_ENDPOINT` (host:port, plain HTTP) at `OTLP_METRICS_URL_PATH`
  (`/opentelemetry/v1/metrics` for VictoriaMetrics). Unset endpoint turns
  them off. `service.name` is `notes-api` or `notes-storage` (with `region`),
  and `service.instance.id` is the pod name. HTTP series carry `method` (or
  `other`), the chi `route` pattern (`unmatched` when none matched) and
  `status`, never a path or user.

Metrics worth alerting on:

| Metric | Service | Watch for |
|---|---|---|
| `http.server.request.errors`, `.duration` (by `route`) | both | 5xx rate and p99 latency |
| `jobs.runs` (`job`, `outcome`) and `jobs.items` | api | any failed run, and no successful `account purge`, `blob GC`, `entitlement reconciliation` or `subscription lapses` run for more than 12 h |
| `storage_client.errors` (`region`, `operation`) | api | a region's internal endpoint failing |
| `tokens.refreshes` (`outcome=reuse_detected`) | api | a spike means stolen refresh tokens or a client bug |
| `receipts.verification_failures` (`reason`) | api | `invalid` after a receipt key change means step 2 was missed |
| `auth.logins` (`outcome`) | api | `provider_error` means Zitadel is down or misconfigured; `signup_not_allowed` counts new accounts `ALLOWED_EMAIL_DOMAINS` refused |
| `db.pool.waits`, `db.pool.connections` | api | pool exhaustion (`DB_POOL_MAX`, default 10 per pod) |
| `sse.streams` | api | open sync streams per pod |
| `auth.failures` (`kind`, `reason`) | storage | a rise in `invalid` 401s, e.g. tokens for another region or a kid the region does not trust after a rotation |
| `storage.s3.errors`, `storage.s3.duration` (`operation`) | storage | S3 trouble |
| `storage.uploaded_bytes`, `storage.downloaded_bytes` | storage | traffic |

**Readiness and drain.** `/readyz` answers 200 until the pod gets SIGTERM, then
503. The pod keeps serving for `DRAIN_DELAY_MS` (15 s) so the readiness probe
(every 2 s, 2 failures) takes it out of the Service. Only then does it close the
listener and wait up to `SHUTDOWN_TIMEOUT_MS` for in-flight requests: 60 s on
the API, which ends SSE streams at that point, and 120 s on storage for slow
blob transfers. `terminationGracePeriodSeconds` (90 and 150) covers both.
Readiness never checks Postgres or S3: an outage would fail every replica at
once and empty the Service. Startup is a TCP probe with up to 180 s.

**Jobs.** Every API replica runs the scheduler, first one minute after start,
then hourly (sessions, abandoned logins, expired pairings) and every 6 h (mutation results, blob
GC, orphan reconciliation, account purge). A run takes a Postgres advisory lock
and is skipped if the job started on any replica within about its interval
(`hosted_job_runs`). So each job runs once per interval across the deployment.

## Runbook

### An account purge is stuck

The `account purge` run reports `failed` in `jobs.runs`, and its log line names
the user. The purge first deletes the user's storage prefix, then their rows.
Both steps are idempotent, and the account stays due until both succeed. So the
next run retries by itself.

1. Find the cause in the API log (`purging user <id>`): almost always the
   region's storage service or S3. Check `storage_client.errors{region}` and
   that region's storage logs.
2. Fix the region (see the next section). The next 6-hourly run finishes the
   purge. To run it sooner, `DELETE FROM hosted_job_runs WHERE name =
   'account purge';` and restart one API pod: it runs the maintenance jobs a
   minute after it starts. There is no production trigger.
3. Due accounts: `SELECT id, region_id, purge_at FROM users WHERE purge_at <=
   now();`. A user whose `region_id` no longer exists in the regions file
   cannot be purged until it is restored there.
4. A purge also stops the account's web and Play subscriptions renewing first,
   and stays due while a provider refuses (`cancelling <source> subscription`
   in the log). A subscription whose provider is no longer configured blocks
   it the same way: restore the provider's configuration, or cancel the
   subscription at the provider and set its row's `will_renew` to false.
   A lapsed account's purge also waits for the purged email unless the user
   has no address; check `RESEND_API_KEY` and Resend's status.

### A storage region is down

Users of that region cannot upload or download. Sync, sign-in and the other
regions are unaffected. Object writes never call storage, so a receipt issued
before the outage can still be claimed until it expires (24 h).

- Check the region's pods (`/readyz`), its S3 credentials and endpoint,
  `storage.s3.errors`, and `auth.failures` (a key rotated out of order shows as
  `invalid` 401s, logged with the reason).
- The API's jobs fail for that region and retry each run: GC stops at its
  first failure, and reconciliation and purge leave the region's work for
  later. Nothing leaks: GC removes ledger rows before their keys, so a key it
  could not delete is an orphan that reconciliation removes once the region is
  back, and a purge keeps the account due until storage confirms the prefix is
  empty.

### A key is compromised

Follow the rotation above, but **skip the waits**. Remove the compromised key
as soon as the new one is in place.

- **API signing key**: anyone holding it can mint access tokens for any user
  and region, and admin tokens. Do steps 1–3, then step 5 at once. Every
  access token signed with the old key stops working, and clients get 401 from
  storage and fetch a new token from the API. Then review storage logs for
  `/internal/*` calls that did not come from the API.
- **Region receipt key**: anyone holding it can forge receipts and make the
  API record blobs that were never uploaded. Do steps 1–3, then step 5 at
  once. Unclaimed receipts signed with the old key are refused, so those
  clients re-upload. Then review ledger rows claimed in that region since the
  suspected compromise.
- **Storage refresh rotation key**: rotate it as above. Holding it alone
  gives nothing: deriving a successor needs the refresh token too.
- **OIDC client secret**: regenerate it in Zitadel, update `notes-api`
  `OIDC_CLIENT_SECRET`, and restart the API. Existing sessions are unaffected.
- **A user's session or refresh token**: revoke the device from the dashboard or
  with `DELETE /api/sessions/{id}`. A refresh token presented again after
  its successor was exchanged revokes its session by itself. Access tokens already
  issued stay valid until they expire (at most 24 h). That is the accepted cost
  of keeping storage stateless.
