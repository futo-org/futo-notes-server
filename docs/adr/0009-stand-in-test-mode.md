---
status: accepted
date: 2026-09-15
---

# Stand-in test mode: how fake login and fake billing get into the shipped binary safely

Client ADR 0003 (`futo-notes/futo-notes#172`, decision 12) asks for the
client's cross-platform suite to drive Log in with FUTO, checkout, and billing
against a localhost server with no network and no credentials.
`docs/Hosted service plan.md` carries it as the fourth client-driven follow-up:
"the `cmd/hostedrehearsal` Zitadel and Polar stand-ins reachable from a flag".

That is the whole ask, and it is a sharp one. It puts an identity provider
that signs anyone in, and a payment provider that entitles for free, inside
the binary people deploy. **This document is the source of truth for how it is
switched on and what stops it being switched on by accident.** The routes it
adds are deliberately absent from `docs/openapi.yaml` and `docs/API.md`,
because they exist in no deployment a client contract describes; the developer-
facing description lives in `docs/DEVELOPMENT.md`.

## One variable, read in one place, failing closed

`STANDIN_MODE=true` — the exact string, anything else is off — is read once, in
`config.Load`, into `Config.StandinMode`. Every behaviour below hangs off that
one boolean. A server started without it has no stand-in listeners, no
replaced settings, and no `/standin` routes in its mux at all.

It is not merely undocumented-but-present. `registerStandinRoutes` is called
only under the flag, so on an ordinary binary the two account controls are not
registered handlers that refuse — they are 404s from a mux that has never heard
of them. A test asserts that in `dev`, `password`, and `oidc` mode, with the
dev UI on.

## It refuses to combine with anything that names something real

Stand-in mode replaces what the hosted settings name with fakes. So finding
any of them already set means `STANDIN_MODE` has reached a server somebody
configured to be real, and the boot **fails with an error naming both the
variable and the setting** rather than overriding it. The refused list is the
four `OIDC_*`, the five `POLAR_*`, and `HOSTED_ALLOWED_EMAIL_DOMAINS`.

Eight of those ten get a stand-in value. `POLAR_DISCOUNT_ID` and
`HOSTED_ALLOWED_EMAIL_DOMAINS` get none — a stand-in checkout carries no
discount and a stand-in server admits its one identity — and they are refused
all the same, because what they name is just as real.

`AUTH_MODE` is refused the same way unless it is unset or `oidc`. Quietly
turning a self-hoster's password login into one that signs anyone in is the
worst version of this accident, so it is the one most worth refusing.

`COOKIE_SECURE` is refused unless it is unset or `false`, and forced to
`false` once it is. Stand-in mode serves plain http on loopback, where a
Secure cookie never comes back — but a server told to mark its cookies Secure
is a server somebody put behind TLS, so turning the flag off quietly would be
the same silent override as the rest. Refusing it costs a suite nothing: it
simply does not set the variable.

## It binds loopback and nothing else

In stand-in mode the listen address is `127.0.0.1:<PORT>` instead of
`:<PORT>`. This is the strictest thing that still satisfies the ticket, which
asks for a localhost server: a server that reached this mode by accident
cannot be talked to from the network **at all**, so the failure is a health
check that stops answering rather than an open door. Nothing is reachable from
off the machine — not the fake login, not the free checkout, and not the two
account controls.

The two stand-in upstreams listen on their own ephemeral loopback ports for the
same reason. Nothing reaches them except this server and anything already on
the machine.

The cost is that a stand-in server inside a container is not reachable from its
host without the container's own port forwarding, and that is accepted: the
ticket's case is a suite on localhost.

## It says so, loudly, on every start

One `WARN` at boot naming what is fake — the login, the checkout, the two
account controls — and that this must never be a real server. A server that
got here by accident has one line in its log that explains every strange thing
it is about to do.

The capability document is deliberately **not** changed. A `standin` field
would be a contract addition nobody asked for, and it would buy nothing:
loopback-only binding already means no remote reader exists to warn.

## The two account controls

```
POST /standin/lapse   → the calling account stops being entitled  (exercises 402)
POST /standin/quota   → sets the Storage Quota; empty body fills it  (exercises 507)
```

Both sit behind the same middleware `/api/` uses, so each acts for the account
that called it and neither takes an account name. Both were **chosen, not
specified**: no document names them.

`lapse` delivers the `subscription.revoked` Polar would send, signed with the
configured webhook secret, through the real webhook route. It is not a shortcut
around signature verification; it is the same path a genuine lapse takes.

`quota` rewrites the one Plan's ceiling, which is shared by every account. A
stand-in server has one identity and therefore one account, which is what makes
that the same thing here. An empty body drops the ceiling to what the account
already stores, so the next byte is refused; a value puts it back, so a suite
that has tested `507` is not stuck at the ceiling.

## One identity, no way to change it

Every stand-in login signs in as `person@standin.test`. A suite that needs a
second account starts a second server on a fresh database, which is what a
suite that wants isolation does anyway. A route to choose the identity was
considered and left out as unasked-for; it is a small addition over the
`SignInAs` the stand-in issuer already has, if a suite turns out to need one.

The login URL and the checkout URL are both fetched by a browser-shaped
client — one that follows redirects and keeps cookies — because the login
chain hands back the `oidc_flow` cookie at the callback. That is a fact a
suite needs, so `docs/DEVELOPMENT.md` states it beside the two calls.

## The stand-ins are the rehearsal's, moved rather than copied

`cmd/hostedrehearsal`'s fakes became `internal/standins`, used by both. They
stay honest: the issuer checks the client secret and recomputes the S256 PKCE
challenge, and Polar checks its access token and refuses a second customer for
an external id it has seen. A run that passes here has been through the real
exchange.

One behaviour is new, because the ticket needs it: fetching a checkout URL
delivers a signed `subscription.created` to the server's webhook **and only
then** redirects to the success page. Firing before redirecting is what lets a
suite read the checkout URL and find the account entitled on the very next
request. A delivery the server refuses answers `502` at the checkout URL rather
than being swallowed, because an account that silently failed to become
entitled is the hardest possible failure to read from the far end.

## Considered and rejected

- **Mounting the stand-ins on the server's own port** under a path prefix,
  rather than separate loopback listeners. Rejected: it puts fake-provider
  routes on the mux that serves the real API, and buys nothing, because a test
  only ever reaches them by following a URL the server handed it.
- **Overriding a real `OIDC_ISSUER` instead of refusing it.** Rejected — a boot
  that silently ignores the issuer an operator configured is exactly the
  accident this is guarding against.
- **Letting `STANDIN_MODE` bind every interface**, so a suite can run the
  server in a container. Rejected for now; the ticket asks for localhost, and
  the loopback bind is the single strongest thing stopping an accident from
  mattering.
- **A `standin: true` field in the capability document.** Rejected: see above.
