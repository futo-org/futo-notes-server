# OIDC login plan ("Log in with FUTO")

Status: design written 2026-09-08. Zitadel application created the same day
(project "FUTO Notes Dev", org `external-dev`, type Web, auth method Code,
Development Mode on, redirect `http://localhost:3005/api/auth/oidc/callback`).

Goal for this iteration: a third `AUTH_MODE=oidc` where a browser can complete
Authorization Code + PKCE against Zitadel and end up with the same session
cookie and `{user, token}` shape the other two login modes produce. Demo lives
at `/dev`. Nothing here changes the client contract for `dev` or `password`.

## Facts from the live Zitadel discovery document (2026-09-08)

- `subject_types_supported = [public]` — `sub` is the instance-wide user ID, so
  `users.sub UNIQUE` remains the cross-service join key. Open question closed.
- `code_challenge_methods_supported = [S256]`.
- `token_endpoint_auth_methods_supported` includes `client_secret_basic`.
- `scopes_supported` includes `openid profile email`; `claims_supported`
  includes `sub`, `email`, `email_verified`, `name`, `preferred_username`.
- Endpoints: `/oauth/v2/authorize`, `/oauth/v2/token`, `/oidc/v1/userinfo`,
  `/oidc/v1/end_session`. Always read them from discovery, never hardcode.

## Design decisions

1. **Standard library only.** No JWT/JOSE dependency. After the code exchange
   the server calls the userinfo endpoint with the access token and reads
   `sub`, `email`, `name`, `preferred_username` from it. The ID token is not
   parsed. The userinfo response arrives over TLS straight from the issuer, and
   `state` + PKCE protect the redirect leg, so this is sound for the code flow.
2. **Confidential client.** Token request authenticates with
   `client_secret_basic` (HTTP Basic, client ID and secret) and also sends the
   PKCE `code_verifier`.
3. **Flow state lives in a short-lived cookie**, not the database. `start`
   generates a random `state` and PKCE `verifier`, stores both in one cookie
   named `oidc_flow` (HttpOnly, `Secure: cfg.CookieSecure`, SameSite Lax,
   Path `/api/auth/oidc/`, MaxAge 600 s), and redirects. `callback` requires
   the cookie, requires `state` query param == cookie state, then clears the
   cookie. Lax is correct: the return from Zitadel is a top-level GET.
4. **User identity keyed on `sub`.** New `auth.UpsertUserBySub(ctx, db, sub,
   email, name)`: `INSERT ... ON CONFLICT (sub) DO UPDATE SET email, name
   RETURNING id, email, name`. Store the raw issuer `sub`, no prefix. If the
   email collides with a different `sub`, let the unique violation surface as a
   500 for now; note it in the doc, don't design around it yet.
5. **Required claims.** `sub` and `email` must be non-empty (users.email is
   NOT NULL UNIQUE). Missing → 502 with a clear message naming the claim. Name
   falls back to `preferred_username`, then to email.
6. **Callback response.** Sets the `session` cookie exactly like the other
   logins. Then: if `cfg.DevUI` is true, 302 to `/dev`; otherwise return the
   standard LoginSuccess JSON `{user, token}`. How a native client receives the
   token is a separate, still-open decision; this interim behavior keeps the
   demo usable and the contract honest.
7. **Capability.** `auth_mode: "oidc"`, and `signup: "open"` in oidc mode
   because any FUTO account can log in. Check `docs/openapi.yaml` for the
   `signup` enum before choosing the literal; if `open` is not in the enum,
   add it there too.
8. **Discovery at boot.** `main.go` discovers the provider once in oidc mode
   and exits with a clear error if the issuer is unreachable or the document
   lacks the three endpoints. `routes()` receives the provider as a parameter
   (nil in other modes). Tests build a fake issuer with `httptest` and pass a
   provider discovered from it.
9. **Rate limiting.** Wrap `callback` with `rateLimited(auth.NewRateLimiter(), …)`
   like password login. `start` is not limited.
10. **Logout** stays local-only. No `end_session` call this iteration.
11. **Outbound HTTP client**: one `http.Client{Timeout: 10 * time.Second}` in
    the oidc package, requests carry the request context.

## Config

`internal/config/config.go`:

| Env | Field | Rule |
|---|---|---|
| `AUTH_MODE=oidc` | `AuthMode` | now one of `dev`, `password`, `oidc` |
| `OIDC_ISSUER` | `OIDCIssuer` | required in oidc mode; trailing slash trimmed |
| `OIDC_CLIENT_ID` | `OIDCClientID` | required in oidc mode |
| `OIDC_CLIENT_SECRET` | `OIDCClientSecret` | required in oidc mode |
| `OIDC_REDIRECT_URI` | `OIDCRedirectURI` | required in oidc mode; must be absolute |

Scopes are fixed at `openid profile email`. No env knob.
Missing values produce one error naming every missing variable.

## Package layout

`internal/oidc/` (new):

- `provider.go`: `type Provider struct { Issuer, AuthorizationEndpoint,
  TokenEndpoint, UserinfoEndpoint string }`; `Discover(ctx, issuer) (*Provider,
  error)` fetches `<issuer>/.well-known/openid-configuration`, checks the
  `issuer` field equals the configured issuer, and requires the three endpoints.
- `flow.go`: `NewFlow() (state, verifier string)`; `(p *Provider)
  AuthorizeURL(clientID, redirectURI, state, verifier string) string` (S256
  challenge, `response_type=code`, `scope=openid profile email`);
  `(p *Provider) Exchange(ctx, clientID, clientSecret, redirectURI, code,
  verifier) (accessToken string, err error)`; `(p *Provider) Userinfo(ctx,
  accessToken) (Claims, error)` where `Claims{Sub, Email, Name,
  PreferredUsername string}`.
- Tests with an `httptest` fake issuer covering: discovery ok / issuer
  mismatch / missing endpoint; authorize URL parameters incl. correct S256
  challenge; exchange sends Basic auth + verifier and handles a non-200;
  userinfo parses claims.

`cmd/server/`:

- `auth.go`: `handleOIDCStart(cfg, provider)`, `handleOIDCCallback(cfg,
  provider, database)`, `requireAuth` carve-outs for
  `GET /api/auth/oidc/start` and `GET /api/auth/oidc/callback` when mode is oidc.
- `main.go`: register the two routes under `if cfg.AuthMode == "oidc"`, pass
  `signup` into `handleCapability`, discover at boot.
- `devui.html`: a bespoke card "Log in with FUTO" with a plain link to
  `/api/auth/oidc/start` and a note that it needs `AUTH_MODE=oidc`. On page
  load, the card reads the capability `auth_mode` and greys itself out (with
  the reason) when the mode is not oidc.
- End-to-end SQLite test: fake issuer → `routes()` in oidc mode → GET start,
  assert 302 to the fake authorize endpoint with `state`, `code_challenge`,
  `code_challenge_method=S256`, and the `oidc_flow` cookie → GET callback with
  the fake's code and the same state, carrying the cookie → assert `session`
  cookie set, `oidc_flow` cleared, 302 to `/dev` (DevUI true) → GET /api/auth
  with the cookie returns the user with the fake's email → a second login with
  the same `sub` but new name updates the row and does not create a second
  user. Also: callback with wrong state → 400; callback without cookie → 400.

## Verification gate

- `GOTOOLCHAIN=auto go test ./...` green.
- `GOTOOLCHAIN=auto go vet ./...` clean.
- Real run: `AUTH_MODE=oidc DEV_UI=true` with the `.env` values on port 3005,
  `curl -i localhost:3005/api/auth/oidc/start` returns 302 to the Zitadel
  authorize endpoint; browser completes login and lands on `/dev` with the
  session pill showing the Zitadel email.

## Deferred (explicitly not in this iteration)

- Native-client token hand-off (loopback / custom scheme / device flow).
- `end_session` on logout.
- Email collision between two different `sub`s.
- Vault key derivation under SSO (client-side; see MANAGED-LAUNCH.md).
