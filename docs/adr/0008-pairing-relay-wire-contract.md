---
status: accepted
date: 2026-09-15
---

# The pairing relay's routes, codes, and window

ADR 0005 decided that a new device unlocks the vault either by the person typing
the Vault Password or by **device-to-device key transfer** from a device that is
already unlocked, and that QR pairing ships at launch. Client ADR 0003
(`futo-notes/futo-notes#172`) decided the client half: the new device shows a QR
code carrying a pairing id, a one-time X25519 public key, and its own name and
platform; the unlocked device scans it, seals the vault key to that public key,
and posts the result to "the server's account-scoped pairing relay"; the new
device polls, decrypts, and stores the key.

Neither document says what that relay looks like on the wire. Nothing in
`docs/Rewriting the server in Go.md`, `docs/openapi.yaml`, or
`docs/Hosted service plan.md` does either. **This document is the source of
truth for that contract**; `docs/openapi.yaml` and `docs/API.md` describe it and
must not drift from it.

## The three calls

```
POST $BASE/api/pairings            { public_key, device_name, platform } → 201 { id, expires_at }
POST $BASE/api/pairings/{id}/key   { ciphertext }                        → 204
GET  $BASE/api/pairings/{id}                                             → 202 | 200 { ciphertext } | 404
```

All three are authenticated, and all three act only on pairings belonging to the
calling account. The names were **chosen, not specified**; a client that wants
different ones is asking for a rename, not a redesign.

**A five-minute window, from creation.** Every step is hands-on and in one
sitting — show a code, scan it, confirm, collect — with none of the trip through
an identity provider's login pages that makes the Login Hand-off's ticket last
ten minutes. Posting the key does not restart the clock the way fulfilling a
hand-off ticket does, because by then the new device is already polling. Five
minutes is also how long a sealed vault key can sit on the server, which is the
other reason not to make it longer.

**Collecting spends the pairing.** The poll reads the ciphertext and deletes the
row in one transaction, so of two devices polling at the same time exactly one
gets the key and the other gets the `404`. A second poll is not a replay.

**`404` is one answer for four situations**: unknown, expired, already
collected, and belonging to another account. Byte for byte the same body, on
both the poll and the post. This follows the rule `docs/openapi.yaml` already
states for the whole API — anything the authenticated user does not own returns
`404`, never `403` — and it is what stops a pairing id being probed for
existence from another account.

**A second post to your own pairing is `409`**, not `404`. Only the owning
account can reach it: a stranger's post is refused as `404` before the pairing's
state is consulted, so the distinction tells a stranger nothing. Telling the
owner the truth is worth something — "someone already sent a key to this
pairing" is a different problem from "this pairing is gone", and the confirming
device can say which.

**Bodies are bounded** at 8 KiB, and each stored string at 1024 characters, over
which the answer is `413` or `400`. A one-time public key and a sealed 32-byte
vault key are a couple of hundred bytes. It bounds one request and nothing more:
these routes sit outside the Entitlement gate and the Storage Quota, they are not
rate limited, and how many pairings an account may open is uncapped. A signed-in
account that wanted to fill the table could; the bound is what stops one call
doing it, and the hourly sweep is what stops the result being permanent.

## Not gated on Entitlement, and mounted in every auth mode

A lapsed subscription pauses writes. It does not pause this: someone who stopped
paying has to be able to set up a device before they can read anything on it at
all, and reads never depend on Entitlement. Pairing is one step further out than
the vault claim that is already ungated for the same reason.

The routes are also mounted in every `AUTH_MODE`, not only `oidc` the way the
Login Hand-off routes are. The relay is about vault keys, not about how anyone
logged in, and the vault key routes it sits beside have never been mode-gated.
Password-mode self-hosted derives its vault key from the login password and has
no use for pairing; it is not harmed by the routes existing.

## What the server holds

`ciphertext` is sealed to `public_key`, whose private half never leaves the new
device. The server cannot open it, does not log it — the failure path logs the
error, the method, and the request path, never the body — and deletes it the
moment it is collected. A pairing nobody collects stops being usable at its
five-minute mark and is deleted by the sweep that runs beside the session and
hand-off reapers, so it is gone within the hour rather than at the six-hourly
maintenance pass; an uncollected vault key is exactly the thing not to leave
lying around. Expiry is enforced when the row is read, so the gap between the
two is a row nothing can reach, not a window in which the key still works.

**`public_key`, `device_name`, and `platform` are stored and never served — and
that holds only for as long as pairing is QR-scan-only.** Client ADR 0003 puts
all four values in the QR payload, so the unlocked device that scans the code
already has the public key it must seal to and the name it must show on the
confirmation. There is no second reader, which is why no route hands them back.

If pairing ever gains a flow where the unlocked device learns the pairing id
without the rest of the payload — a typed short code, an id read aloud, a
desktop with no camera taking a number — then it has no public key to seal to
and no name to confirm, and these three columns have to become readable. **That
is a new `GET /api/pairings/{id}/offer`, not a body on the poll's `202`.** The
poll belongs to the new device and spends the pairing, so an unlocked device
polling to read the offer would collect its own ciphertext and strand the very
pairing it was trying to complete. Writing the columns now is what makes that
change a new route over data already there, rather than a migration plus a
client release before anything can be read back.

## Considered options

- **Return the pairing's description on the `202` poll**, so the three stored
  fields have a reader and an unlocked device could confirm a scan against the
  server. Rejected for the reason under "What the server holds" — the poll
  spends the pairing, so the wrong device reading it breaks the pairing — and
  because the ticket asked for three calls and the QR-scan flow needs no fourth.
- **`409` for a cross-account post as well**, on the grounds that it is the
  honest status. Rejected: it makes a real pairing id distinguishable from one
  that was never minted, which is the one thing the account check is there to
  prevent.
- **Making the post idempotent**, so a retried post lands as `204` rather than
  `409`. Rejected as meaningless here: a sealed box is nondeterministic, so a
  retry carries different bytes than the post it repeats and there is nothing to
  match on. A client that is unsure whether its post landed can create a new
  pairing, which costs one QR code.
- **A ten-minute window to match the Login Hand-off.** Rejected: the hand-off's
  window covers a person reading a provider's login pages; this one covers a
  camera finding a QR code.
- **Reusing the `login_handoffs` table.** Rejected — a hand-off ticket names an
  account and carries no secret, and this table carries a sealed vault key for
  an account already known. One column in common is not a shape in common.
