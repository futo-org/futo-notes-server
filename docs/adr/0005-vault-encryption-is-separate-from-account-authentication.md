---
status: accepted
date: 2026-09-10
---

# Vault encryption is separate from account authentication

Account authentication on the hosted service stays "Log in with FUTO" through Zitadel, driven by the Login Hand-off. Vault encryption is a separate secret: the person chooses a **vault password** the first time they enable sync, and it never leaves the client. Neither the Notes server nor Zitadel ever receives a plaintext vault key or vault password.

Around that one rule:

- The vault key is **remembered per device** in the platform keychain. There is no vault-password prompt on app restart, during everyday use, or when the FUTO session is renewed. It is a setup-time secret, not a login-time one.
- A **new device** authenticates with FUTO, then unlocks the vault either by the person typing the vault password or by **device-to-device key transfer** from an already-unlocked device. QR pairing ships at launch; it is what makes a separate secret tolerable, so it is not a follow-up.
- A **generated recovery key** is the third door. An unlocked device or the recovery key restores access and can set a new vault password.
- Changing or resetting the FUTO password does not touch vault encryption. An account-password reset on its own cannot recover a vault once every device and every recovery secret is gone.

This closes the question open since June in `docs/MANAGED-LAUNCH.md`: how the vault key is unlocked when login is OIDC and no password reaches the client.

## Considered options

- **Derive the vault key from the account password**, which is what self-hosted does today — `connect(password)` in the client both authenticates and unwraps, so one string does both jobs. Rejected for the hosted service. If email and password alone are enough to decrypt, and the server is what verifies the password, then at the moment of login the server holds everything needed to decrypt. Reaching that UX also means the app collects credentials and the server checks them against Zitadel's Session API (`PATCH /v2/sessions/{id}` with `checks.password`), which puts the raw password through our infrastructure. A client-side split — vault key on one side, an unreversible login proof on the other, as Bitwarden does — restores the property, but only if we verify passwords ourselves, which means giving up "Log in with FUTO" and owning reset, verification, and rate limiting. Not worth it for a shared FUTO identity we want.
- **Obsidian's model unmodified**: a vault password typed on each new device, no pairing, no recovery key. Rejected as the launch bar rather than as a design. Obsidian's own guidance for a forgotten encryption password is to create a new remote vault and reconnect every device, and their password is cached per device after first entry. Pairing and a recovery key are the two additions that make the separate secret survive contact with real people.
- **A generated recovery code as the only secret**, which is Yucca's shape for backups: `randomBytes(32).toString('hex')` at first boot, HKDF-SHA256 per repository, displayed once with print, save-to-file, copy, and browser-password-manager affordances. Rejected as the primary path, because it makes every new device a thirty-character transcription. Kept as the recovery envelope, and its save-it-once presentation is the model to copy.
- **Packing both envelopes into the existing `key_kdf` and `encrypted_vault_key` fields.** Tempting because the server treats `key_kdf` as `json.RawMessage` and never reads inside `encrypted_vault_key`, so it needs no migration at all. Rejected: it leaves one column holding a bare hex string when a v1 client wrote it and a JSON document when a v2 client did, told apart by a value in a different column. The migration is the cheaper thing to own.
- **A `collection_key_wraps` table, one row per envelope.** Deferred rather than rejected. It is the right shape if envelopes ever multiply — an envelope per device, so a lost phone can be revoked without re-keying the vault. QR pairing hands the key to the receiving device's keychain and creates no server-side envelope, so two envelopes is expected to be stable. A third envelope kind is the trigger to revisit this.

## Consequences

- **The vault key is wrapped twice**, under the vault password and under the recovery key. The stored key material gains a second envelope in **new nullable columns** rather than reusing the first. Nullable because a vault created by a v1 client has no recovery envelope, and a client that sends only the password envelope keeps working.
- `GET` and `PUT /api/collections/{id}/key` grow the recovery fields, and `docs/openapi.yaml` has to say that the recovery envelope is optional and what its absence means.
- **Two unlock models coexist.** Password-mode self-hosted keeps deriving the vault key from its login password, with the recovery columns left NULL. Hosted uses the separate vault password. The server stores both shapes and interprets neither — it has never been able to read key material, and that does not change.
- **The client work is an epic, not an edit**, and it belongs in the client repo with its own ADR: a keychain owner per shell, a second KDF discriminator in `KeyMaterial` (today `unwrap_vault_key` hard-rejects anything but `pbkdf2-sha256`), recovery-key generation and its save screen, QR pairing on iOS, Android and Tauri with a fallback for a desktop with no camera, and a vault-password change flow that re-wraps. Every new string is a `languages/en.json` entry.
- The client's sync harness pins server release v0.7.0 (`scripts/sync-server-pin.json`), so nothing on the client can exercise the recovery envelope until a release carrying these columns is cut and that pin is bumped.
- **Losing every device and every recovery secret means the ciphertext is unrecoverable.** That is the deliberate price of a server that cannot read it, and it has to be said plainly in the product, not just here.
