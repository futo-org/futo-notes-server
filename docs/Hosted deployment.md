# Hosted deployment

How the hosted FUTO Notes sync service is configured and deployed. Issue #12.
The decisions behind it are in `docs/Hosted service plan.md` and ADR 0004; this
is the operational half: every setting the hosted server reads, the one-time
setup that has to happen outside this repo, and how to tell it worked.

Status: written 2026-09-09. The environment contract and the localhost
rehearsal are done. The Manifest entry exists as an **unpushed local commit**
on branch `futo-notes-hosted` in the `manifest-inventory` checkout, not on its
`master`. **Nothing has been deployed and no Zitadel application or Polar
webhook has been created yet** — the four steps below each need a console this
repo cannot reach.

## What runs where

The internal MVP is a second FUTO Notes server on the shared Manifest staging
pair, `manifest-staging` and `manifest-staging2`. It is a different service
from the internal sync server that already runs there:

| | Internal sync server | Hosted service |
|---|---|---|
| Manifest file | `inventory/futo-notes/sync-server.yml` | `inventory/futo-notes/hosted.yml` |
| Container | `inventory_futo-notes_sync-server` | `inventory_futo-notes_hosted` |
| Reachable at | tailnet only, `100.x.x.x:3010` | `https://staging-notes-sync.futo.org` |
| Login | `AUTH_MODE=dev`, email only | `AUTH_MODE=oidc`, "Log in with FUTO" |
| Database | `futo_notes` | `futo_notes_hosted` |
| Blobs | `/futo-notes-data/` | `/futo-notes-hosted-data/` |
| Writes | ungated | gated on a Polar subscription |

They share the futo-notes Patroni cluster and nothing else. Manifest gives
every image in a project folder one pod and one network, which is why the
hosted service listens on **3011** rather than 3000: 3000 is already taken
inside that pod. It is reserved as `3011:3011` under `futo-notes` in
`config-inv.yml`.

Manifest rewrites a `loadbalancer.domain` for every environment except
`production` and `pay`, so `domain: notes-sync.futo.org` deployed to the
staging pair is served at **`staging-notes-sync.futo.org`**. That name is what
the Zitadel redirect URI and the Polar webhook URL have to match, so moving to
a dedicated host later means re-registering both. Both are dashboard edits.

## The environment contract

Hosted mode is `AUTH_MODE=oidc` plus four OIDC settings and four Polar ones.
Any of the eight missing fails the boot with a single error naming all of
them, so an operator fixes them in one pass rather than eight restarts.

**`DATABASE_URL` is not one of the eight.** It defaults to
`sqlite:./data/notes.db` and `AUTH_MODE=oidc` does not require it, so a hosted
deployment that leaves it out boots healthy on a SQLite file inside the
container and loses everything on the next restart. Nothing in the server
catches that today. Until something does, the Manifest entry setting it is the
only thing standing between this instance and that failure, and it is worth
reading first in any review of `hosted.yml`.

### Required in hosted mode

| Variable | What it is |
|---|---|
| `AUTH_MODE` | `oidc`. This is what makes the server hosted: it mounts the Login Hand-off, billing, and webhook routes, reports `billing: true` and `signup: open` in the capability document, enforces the Storage Quota, and gates writes on Entitlement. |
| `DATABASE_URL` | `postgres://futo_notes:<password>@<host>:15510/futo_notes_hosted`. Required in practice, though not by the boot check — see above. Port 15510 is HAProxy in front of the Patroni cluster, which routes to the current primary; direct 5510 is read-only whenever the local node is a replica. |
| `OIDC_ISSUER` | The Zitadel instance. Read at boot: the server fetches `<issuer>/.well-known/openid-configuration` and exits if it is unreachable or missing the authorization, token, or userinfo endpoint. A trailing slash is trimmed. |
| `OIDC_CLIENT_ID` | The Zitadel application's client id. |
| `OIDC_CLIENT_SECRET` | Its secret. The server is a confidential client and authenticates the code exchange with `client_secret_basic`. |
| `OIDC_REDIRECT_URI` | Absolute URL whose path **must end in `/api/auth/oidc/callback`**; the server refuses to boot otherwise. It is the only public origin the server is ever told, and two more URLs are derived from it: the Login Hand-off's login URL (`/api/auth/oidc/start`) and the checkout success page (`/billing/done`). Get this wrong and logins and checkouts send people to a URL that 404s. |
| `POLAR_BASE_URL` | `https://api.polar.sh` for production, `https://sandbox-api.polar.sh` for the sandbox. |
| `POLAR_ACCESS_TOKEN` | Organization access token. Used only by the checkout and portal routes; nothing on the sync path calls Polar. |
| `POLAR_PRODUCT_ID` | The $5/month product a checkout subscribes to. |
| `POLAR_WEBHOOK_SECRET` | The endpoint's Standard Webhooks signing secret, `whsec_`-prefixed base64. |

### Optional

| Variable | Default | What it is |
|---|---|---|
| `POLAR_DISCOUNT_ID` | unset | Applied to every checkout while set. The internal MVP points it at a **100% percentage discount whose duration is Forever**, so FUTO people go through the real product's checkout and pay nothing. Dropping the variable is the launch flip; there is no product swap on the day. |
| `HOSTED_ALLOWED_EMAIL_DOMAINS` | unset | Comma-separated, compared case-insensitively. Unset admits every identity the provider authenticates. Set to `futo.org` for the internal MVP: anyone else is refused at the OIDC callback with `signup_not_open` and no account is created. |
| `PORT` | `3005` | `3011` here, because the pod's 3000 belongs to the internal sync server. |
| `BLOB_DIR` | `./blobs` | `/data/blobs`, on the `/futo-notes-hosted-data/` host volume. |
| `COOKIE_SECURE` | `true` | Leave on. The instance is behind the loadbalancer's TLS. |
| `DEV_UI` | `false` | Leave off. It serves `/dev`, which can sign Polar deliveries with the configured webhook secret. |
| `STANDIN_MODE` | `false` | **Never set this.** It replaces Zitadel and Polar with in-process fakes: anyone who opens the login URL is signed in, and opening a checkout URL entitles the account for free. It refuses to boot alongside any `OIDC_*`, `POLAR_*`, or `HOSTED_ALLOWED_EMAIL_DOMAINS` value, so it cannot start on this instance's configuration at all — and if it somehow did, it binds loopback only and the loadbalancer would find nothing answering. It exists for automated test suites; see [ADR 0009](./adr/0009-stand-in-test-mode.md). |
| `BLOB_GC_ENABLED` | `true` | Off disables irreversible blob collection. |
| `DB_POOL_MAX` | `10` | Postgres only. |
| `DB_POOL_IDLE_TIMEOUT_MS` | `10000` | Postgres only. |
| `ALLOW_FRESH_DATABASE` | `false` | SQLite guard; irrelevant on Postgres. |

Settings the TypeScript server honored and this one does not — `LOG_LEVEL`,
`TRUST_PROXY`, `MAX_BLOB_BYTES`, `MAX_BATCH_BYTES`, `BLOB_RETENTION_DAYS`,
`BLOB_GC_INTERVAL_MS`, `AUTH_RATE_LIMIT`, `AUTH_RATE_LIMIT_WINDOW_MS`,
`DB_SSL*` — are logged as ignored at boot rather than silently dropped.

## One-time setup

Four of these need a console. None of them can be done from this repo.

### 1. DNS and the loadbalancer

`staging-notes-sync.futo.org` has to resolve to the staging loadbalancer,
`137.184.44.236` (`do-lax-lb-staging`). Manifest creates the certificate once
the name resolves. Nothing else works until this does: the Zitadel redirect
and the Polar webhook both need a public HTTPS URL.

### 2. A Zitadel application

The existing FUTO Notes application is a **dev** application: type Web, auth
method Code, Development Mode on, redirect `http://localhost:3005/api/auth/oidc/callback`.
It must not be reused, because Development Mode allows plain-HTTP redirects.

Register a second application on the same instance with:

- Type **Web**, auth method **Code** (confidential — the server holds a secret).
- Development Mode **off**.
- Redirect URI exactly `https://staging-notes-sync.futo.org/api/auth/oidc/callback`.
- Scopes `openid profile email`; PKCE S256.

Then put the client id and secret in 1Password (step 4).

**Open question for devops.** `inventory/futo-notes/hosted.yml` currently names
`https://external-dev-gkhk8b.us1.zitadel.cloud` — the Zitadel Cloud instance
the dev application is already on, org `external-dev`. It serves a live
discovery document, and its subjects are public rather than pairwise, so the
`sub` claim is stable across applications. But the org is named for
development, and there is also a self-hosted Zitadel at
`staging-zitadel.futoinfra.com` that futopay's staging uses. Which instance is
"Log in with FUTO" for a real internal service is the one identity decision
this repo cannot settle, and it is not reversible in practice: `sub` is the
cross-service identity key, so the instance chosen here is the one every
account is joined to. Confirm before creating the application.

### 3. Polar

All against the production organization, since the MVP runs a real checkout.

1. **Create the discount.** 100%, type **percentage**, duration **Forever**.
   Check the dashboard reads Forever before pointing `POLAR_DISCOUNT_ID` at
   it — a 100% discount with duration Once makes month one free and bills $5
   in month two, which is exactly the failure the internal MVP cannot have.
2. **Register the webhook endpoint** at
   `https://staging-notes-sync.futo.org/api/billing/polar/webhook`.
   Subscribe it to the subscription lifecycle events; the server acts on
   `subscription.created`, `.active`, `.updated`, `.past_due`, `.canceled`,
   and `.revoked`, and acknowledges everything else with 200 rather than
   failing, so subscribing to more types produces no retries.
3. **Take the endpoint's signing secret.** It must be a Standard Webhooks
   secret — `whsec_`-prefixed base64. Polar generated Polar-specific HMAC
   secrets before 8 September 2026 and Standard Webhooks ones after; the
   server implements the current scheme only, so an older secret has to be
   regenerated.
4. **Note the product id** for the $5/month product, and mint an organization
   access token.

Two things nobody has watched happen yet, both claimed by Polar's
[discounts documentation](https://polar.sh/docs/features/discounts) and
neither verified against a live checkout. Confirm both on the first real run:

- The checkout asks for **no card**. Polar's wording is "Free products and
  100% forever subscriptions never ask for a card."
- The discount applies to the **renewal**, not only the first invoice — check
  the resulting subscription in the dashboard, not just the checkout.

### 4. 1Password

Create item `Manifest/futo-notes-hosted` with these fields, which are what
`hosted.yml` references:

```
oidc_client_id
oidc_client_secret
polar_access_token
polar_product_id
polar_webhook_secret
polar_discount_id
```

There is deliberately no `postgres_password` field. `hosted.yml` reads the
existing `Manifest/futo-notes-internal/postgres_password` directly, because it
is the same Patroni cluster and the same `futo_notes` role — a copy would
silently stop matching the day the password is rotated.

`OIDC_REDIRECT_URI` and `POLAR_BASE_URL` sit in the `secrets:` block too but
are plain values, not `op://` references — Manifest passes non-reference
values through unchanged, and keeping them there is what lets them differ per
environment. It is the same thing the crashlog entry does with its OAuth
redirect URI.

### 5. Create the database

The server applies its own migrations but does not create its own database.
Once, on the Patroni primary:

```sql
CREATE DATABASE futo_notes_hosted OWNER futo_notes;
```

It is deliberately not the internal sync server's `futo_notes`. That one
holds `AUTH_MODE=dev` accounts created by anyone on the tailnet with an email
address; hosted accounts are OIDC identities keyed on `sub`, and the two must
never mix in one `users` table.

### 6. Pin the image

`hosted.yml` ships with `version: staging: candidate-0000000`, which is a
placeholder that will fail to pull. Replace it with the
`candidate-<short sha>` tag from the pipeline that built the commit being
deployed. The moving `candidate` tag is deliberately not used: it is shared
with `go-rewrite` builds, so it would silently swap this instance's code.

The candidate image job now also runs on the `hosted-server` branch, so the
branch produces a deployable tag before it merges. Drop that rule from
`.gitlab-ci.yml` once it does.

## Rehearse on localhost first

`cmd/hostedrehearsal` walks the whole hosted path against a real server
process, with local stand-ins for Zitadel and Polar, so it needs no
credentials and no network:

```bash
docker compose up -d postgres
docker exec futo-notes-postgres createdb -U futo_notes hosted_rehearsal
HOSTED_REHEARSAL_DATABASE_URL='postgres://futo_notes:futo_notes@localhost:5433/hosted_rehearsal' \
  GOTOOLCHAIN=auto go run ./cmd/hostedrehearsal
```

It empties the scratch database, builds the server from the working tree and
boots it from exactly the contract above, and checks sixteen things in the
order a person meets them:
the capability document, a Login Hand-off login, a spent ticket answering 404,
a refused off-domain login, billing before paying, an ungated Vault claim, a
402 on a write, a checkout carrying the product and the discount and the
success page, the success page without a session, a signed Polar delivery, the
same write accepted, billing reporting the bytes stored, a portal that reuses
the one registered customer, a checkout that answers with status instead of a
second URL, a 507 past the Storage Quota, and a revoked subscription stopping
writes while reads keep working.

What it cannot prove is anything on the far side of the two fakes: that the
Zitadel application exists and is registered for the right redirect URI, that
the Polar webhook endpoint exists, or that the discount reads 100%-forever in
Polar's dashboard. Those are the dashboard checks above.

## Verifying the deployment

**Migrations.** The server applies them at boot and logs what it applied. From
empty, Postgres should reach `015_polar_customer_before_subscription`:

```sql
select name from kysely_migration order by name;
```

**Soak check.** `scripts/staging-soak-check.sh` checks the capability
document, database health, container health, zero restarts, and zero
application errors since a given epoch. The hosted instance is a second
service on the same hosts, so it has to be named:

```bash
FUTO_SOAK_URLS='http://100.76.177.70:3011 http://100.127.246.105:3011' \
FUTO_SOAK_SSH_HOSTS='100.76.177.70 100.127.246.105' \
FUTO_SOAK_ENGINES='postgres postgres' \
FUTO_SOAK_CONTAINER='inventory_futo-notes_hosted' \
FUTO_SOAK_AUTH_MODE='oidc' \
./scripts/staging-soak-check.sh candidate-<sha> 2026-09-10T00:00:00Z
```

The URLs are the two containers on the tailnet rather than the one public
name, because the script pairs each URL with the SSH host whose container it
then inspects, and the loadbalancer would answer from whichever host it
picked. Confirm the public leg separately — it is the certificate and the
routing that are being checked there, not the application:

```bash
curl -sSf https://staging-notes-sync.futo.org/ | jq '.auth_mode, .billing'
```

`FUTO_SOAK_AUTH_MODE` is what stops a forgotten `FUTO_SOAK_CONTAINER` from
quietly reporting on the internal sync server instead: it requires `oidc` both
in the capability document each URL serves and in the container's environment.

**One person, the whole path.** From the app, against this instance:

1. Sign in — the app opens a browser, "Log in with FUTO" completes, the
   browser says to go back to the app, and the app has a session.
2. `GET /api/billing` reads `entitled: false`, `state: "none"`.
3. A write is refused with 402 and `subscription_required`.
4. Checkout — the browser opens Polar, **asks for no card**, and ends on the
   "you're subscribed" page.
5. `GET /api/billing` reads `entitled: true`, `state: "active"`.
6. A write is accepted, and `GET /api/billing` shows `usage.bytes_used` moving.
7. In the Polar dashboard, the subscription shows the discount applying to the
   renewal.

Step 1 has a client-side prerequisite nobody has answered yet: with OIDC login
no password reaches the client, so the vault key has no PBKDF2 input. It is
flagged in `docs/Hosted service plan.md` and owned by the client team. It
becomes visible the first time an internal person signs up.

## Still not done before anyone outside FUTO signs up

The list lives in one place, "Not in the MVP" in
`docs/Hosted service plan.md`, and is deliberately not copied here — two
copies drift and then neither can be trusted. Read against this deployment,
nothing on it has moved: account deletion, the S3 blob store, a dedicated host
(this deployment *is* the second instance on the shared staging box the list
wants replaced), dropping `HOSTED_ALLOWED_EMAIL_DOMAINS` and
`POLAR_DISCOUNT_ID`, answering the Login Hand-off phishing risk, and
recovering a lost Polar customer id.

Re-read it once the instance is actually up: whether deploying changed
anything on it is a question this document cannot answer before there is a
deployment.
