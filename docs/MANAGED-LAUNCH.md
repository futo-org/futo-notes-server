# Managed Service — Launch Readiness

Working notes on what it takes to offer FUTO Notes as a fully managed, paid,
multi-tenant service. Captures decisions and findings as of 2026-06-17.

This document records **current reality and decisions**. Where it contradicts
`DESIGN.md`, reality wins until the design doc is reconciled — see "Discrepancies
with DESIGN.md" below.

## Blockers to a managed launch

In rough critical-path order.

### 1. Production blob storage

**State:** Only `FsBlobStore` (local filesystem) exists. There is **no
`S3BlobStore`**, despite `DESIGN.md` describing one as if it ships. The hosted
entrypoint runs the same `buildApp()` that hardcodes `new FsBlobStore(env.BLOB_DIR)`
(`src/app.ts`, `src/server.ts`).

**Two viable paths:**

- **Launch on a host volume (fast path).** FUTO's actual deployment is single-server
  Docker + host volume mounts (see "Deployment reality" below). Immich itself is
  deployed this way in FUTO infra (`inventory/immich/server.yml` mounts `/upload`).
  Running Notes the same way means `FsBlobStore` on a mounted volume is a legitimate
  launch option. S3BlobStore becomes a *scaling* follow-up, not a launch blocker.
- **True object storage (R2 / Ceph).** Required for the horizontal-scaling story in
  `DESIGN.md`. Needs the `S3BlobStore` implementation actually written, plus an
  env-driven `BlobStore` factory so the hosted entrypoint selects S3 instead of FS.
  `manifest-inventory` does not provision object storage — it would only inject the
  R2/Ceph credentials as secrets.

**Recommendation:** launch on a host volume; treat S3BlobStore as the scaling task.

### 2. Multi-tenant auth — RESOLVED in direction (see "Auth" below)

Only `dev` (passwordless) and `password` (single-user singleton, `sub='local'`)
modes are wired today; `signup` is hardcoded `'closed'`. A managed service needs
real multi-user signup/login. **Decision: "Log in with FUTO" via Zitadel Cloud OIDC.**
Remaining work is mechanical — see Auth section.

### 3. Billing

Hosted entrypoint (`src/hosted/index.ts`) is a stub with a billing-middleware
comment. **Decision: Polar.sh** (already in `DESIGN.md`). Needs checkout, webhook
handling, and a subscription gate on writes. Note: FUTO also runs `futopay`
(in `manifest-inventory`), but Polar is the chosen path.

### 4. Per-user storage quotas

Upload *size* limits exist; per-user *storage quota* enforcement does not. Without
it, one customer can run up an unbounded storage bill or exhaust shared capacity.
Close to a blocker for a paid service — at minimum, a quota check on blob upload
tied to the user's plan.

### Need-soon, not day-one

- **Account lifecycle:** no account-deletion endpoint (GDPR), no password reset.
  Orphaned-blob GC and collection-cascade cleanup already exist, so the plumbing is
  there — just no user-facing account delete.
- **Rate limiting / abuse controls:** the password-login path is rate-limited
  in-process (per-instance, in-memory; see `src/auth/rate-limit.ts`), which
  covers the scrypt brute-force/CPU-amplification vector. No limits yet on the
  authenticated API surface (sync/blob writes), and the in-memory limiter does
  not coordinate across instances — a shared store (Redis/Valkey) is the
  multi-instance follow-up.

### Not blockers (good shape)

- Tenant isolation (`user_id`-scoping invariant) looks consistently applied.
- Sessions in Postgres + SSE over `LISTEN`/`NOTIFY` — fine at launch scale; they're
  Stage 1–2 items on the scaling ladder, not launch gates.

## Auth — "Log in with FUTO" (Zitadel Cloud)

**Decision (confirmed 2026-06):**

- Zack Pollard confirmed the product model: a single **"Log in with FUTO"** option.
- The Yucca dev confirmed the implementation: **Zitadel Cloud** is the org IdP, used
  for **both customer auth and internal FUTO auth**. This is why no IdP appears in
  `manifest-inventory` — Zitadel is external SaaS, not self-hosted.

This means one Zitadel instance is the single identity source across all FUTO
services (Notes, Immich, futopay, …). "Log in with FUTO" is the standard OIDC
Authorization Code + PKCE redirect flow with Zitadel as issuer.

### How Yucca does OIDC (our reference)

`packages/yucca-api` (NestJS) implements the flow with the public **`openid-client`**
library using dynamic issuer discovery (`client.discovery(OIDC_ISSUER, ...)`).
Fully env-configurable, not tied to a specific IdP:

```
OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, OIDC_SCOPE,
OIDC_REDIRECT_URI, OIDC_LOGOUT_REDIRECT_URI, OIDC_REQUIRE_PKCE,
OIDC_DEVICE_ISSUER, OIDC_DEVICE_CLIENT_ID   (separate device-flow issuer/client)
```

Routes: `/auth/oidc/login` (authorize + PKCE), `/auth/oidc/callback` (code exchange,
sets a 7-day httpOnly cookie), `/auth/oidc/device` (SSE device flow for headless
clients), `/auth/logout`.

**Do not copy Yucca's files** — it's NestJS, ours is Hono, and there's a licensing
question. Reimplement the same flow against `openid-client` directly. Our session
layer already matches Yucca's (opaque token, SHA-256 hash stored, 7-day expiry), so
the net-new work is just discovery → authorize → callback wired into `src/hosted/`,
mapping the `sub` claim to our existing user/session logic. ~1–2 days.

### `sub` is the cross-service identity key

Our `users.sub UNIQUE` column is the join key. As long as every service trusts the
same issuer, the same person resolves to the same `sub` everywhere — that is what
makes "one login" actually line up across services. No schema change needed.

### Integration to-do (once values are obtained from infra)

1. `OIDC_ISSUER` URL for the Zitadel instance (e.g. `https://<instance>.zitadel.cloud`
   or a custom domain). `openid-client` discovery reads `/.well-known/openid-configuration`.
2. A **Notes application registered in Zitadel** → `client_id` (+ secret for a
   confidential client, or PKCE-only for a public client) + redirect URIs.
3. Wire `openid-client` into `src/hosted/`; map `sub` → user/session.
4. Add `'oidc'` to `AUTH_MODE`; flip capability `signup` to `'open'` for hosted.

### Open questions

- **Confirm subjects are public, not pairwise.** Zitadel uses a stable global user
  ID as `sub` by default (same across apps in the instance), so this is very likely
  fine — but a pairwise/per-application config would silently break cross-service
  identity (the same user would get a different `sub` in Notes vs Immich). Worth a
  one-line confirm given Zack's intent that accounts line up across services.

## E2EE vault key under SSO (unresolved design decision)

The hard, still-open item. Today the client wraps the vault key with a
**PBKDF2-derived key from the user's vault password**; the server stores `key_salt`,
`key_kdf`, `encrypted_vault_key`. With "Log in with FUTO", **no password ever reaches
the client** — OIDC authenticates *who you are* but hands the client no key material.

So SSO forces a client-side decision about how the vault key is unlocked: a separate
vault passphrase (decoupled from login), a passkey-derived key, key escrow, or
similar. This is entirely client-side and does not block the server work, but it is
expensive to retrofit. Immich is not E2EE, so the Yucca dev may not have prior art —
raise with the Notes client team.

## Deployment reality

FUTO deploys via **Manifest** (`gitlab.futo.org/devops/manifest`), driven by the
`manifest-inventory` repo (sibling checkout). Observed model:

- Docker containers on **named bare-metal servers** (e.g. `hv-lax2`), **not Kubernetes**.
- **Host volume mounts** for persistence (e.g. Immich's `/upload`), **not object storage**.
- HAProxy loadbalancer, healthchecks, 1Password / ansible-vault secrets.

There is already an `inventory/futo-notes/crashlog.yml` for the Notes crash-log
server (described as "Hono + Bun + SQLite"). Deploying the sync server means adding a
`server.yml` alongside it. (The crashlog entry also confirms FUTO already runs **Bun**
in production for a sibling Notes service.)

## Discrepancies with DESIGN.md (to reconcile)

- `DESIGN.md` describes `S3BlobStore` as existing; it does not (only `FsBlobStore`).
- `DESIGN.md` storage/Yucca-migration sections call S3 migration "a config change,
  no new implementation needed" — untrue until `S3BlobStore` is written.
- `DESIGN.md` deployment says "Hetzner + Kubernetes"; actual FUTO deployment is
  Manifest + single-server Docker + host volumes. Both can be true over time (Manifest
  now, K8s as scaling aspiration), but the near-term launch path is Manifest.
