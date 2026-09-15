---
status: accepted
date: 2026-09-15
---

# The recovery envelope's names, and what omitting it means

ADR 0005 decided that the vault key is wrapped twice and that the second wrapping
goes in new nullable columns. It did not name them, and neither did
`docs/Hosted service plan.md`, `docs/openapi.yaml`, or the client's hosted-sync
spec (`futo-notes/futo-notes#172`). This ADR fixes the three names and the two
rules a client has to obey, because the client has to match them exactly and
until its own ADR lands there is nothing else to match against. **This document
is the source of truth for that contract**; `docs/openapi.yaml` and `docs/API.md`
describe it and must not drift from it.

**The names**, mirroring the three password-envelope fields they parallel, used
identically as columns and as JSON:

| Password envelope | Recovery envelope |
| --- | --- |
| `key_salt` | `recovery_key_salt` |
| `key_kdf` | `recovery_key_kdf` |
| `encrypted_vault_key` | `recovery_encrypted_vault_key` |

`recovery_key_kdf` is `jsonb` on Postgres and `TEXT` on SQLite, exactly as
`key_kdf` is. All three are nullable and, per the two rules below, null together.

**Rule 1: all three or none.** A `PUT /api/collections/{id}/key` carrying some but
not all three answers `400`. Half an envelope is a recovery door that cannot be
opened, and storing one would mean a vault that advertises a recovery key the
Recovery Key cannot actually unlock. A row that somehow holds a partial set reads
back as no envelope at all, for the same reason. An explicit `null` and an omitted
field mean the same thing on the way in.

**Rule 2: omitting the recovery fields deletes the stored envelope.** A `PUT`
replaces the whole of the key material, both envelopes in one write. There is no
partial update and no way to address one envelope on its own.

Rule 2 is the one that can lose a person's vault, so the reasoning is worth
stating in full — it differs by which client is writing.

- **A v1 client** — one that predates recovery keys, or self-hosted password mode
  — mints a **fresh vault key** every time it re-wraps (`wrap_vault_key` in the
  client's `e2ee/vault_key.rs` generates a new key rather than re-wrapping the
  existing one). The recovery envelope it omits therefore wraps a key that no
  longer exists. Deleting it is the correct outcome; retaining it would leave a
  recovery door that opens nothing, and a person holding that recovery key would
  believe they had a way back in when they did not.
- **A v2 client changing only the vault password** re-wraps the **same** vault
  key. Here the recovery envelope it did not send would still have opened the
  vault. Deleting it destroys a working recovery door. This case is why the rule
  has to be stated loudly rather than left to "optional": such a client **must**
  send the stored recovery envelope back unchanged alongside the new password
  envelope, and likewise send the untouched password envelope when issuing a new
  recovery key. Client ADR 0003 already says both envelopes are written together,
  which satisfies this — the hazard is a client that forgets.

## Considered options

- **Treat an omitted recovery envelope as "leave unchanged", updating only the
  fields sent.** Rejected. It reads more forgiving, and it is the assumption a
  client author makes from the word "optional" — which is exactly why the docs
  now say the opposite in bold. But it is wrong for the v1 case, and wrong in the
  direction that matters: a v1 re-wrap would leave a recovery envelope wrapping a
  dead vault key, and nothing in the request tells the server which case it is in.
  The server cannot read either envelope, so it cannot detect the mismatch, and a
  recovery key that silently stops working is worse than one the person is told
  to re-create. Replace-everything also keeps the endpoint's existing shape: a
  `PUT` of key material has always been "here is the material", never a patch.
- **A separate endpoint or verb per envelope**, so one can be written without
  naming the other. Rejected as more surface than the problem needs. The two
  envelopes share one revision token; splitting the write splits the guard too,
  and two writes that must both land is a worse failure mode than one write that
  carries both.
- **A `CHECK` constraint enforcing all-or-nothing in the schema.** Wanted, and
  skipped for SQLite's sake: `ALTER TABLE` cannot add a table-level `CHECK`, so it
  would mean rebuilding `collections`, which `objects` and others reference by
  foreign key. The invariant is held in one place in Go instead — the handler
  rejects a partial set, and `PutKey` writes all three columns in a single
  statement, so no code path can produce a partial row.
- **Names not derived from the existing three** (`recovery_salt`, `recovery_kdf`,
  `recovery_wrapped_key`, or an `envelopes` JSON document). Rejected: the parallel
  naming is what makes the two envelopes obviously the same shape, and a client
  reading either set can guess the other.

## Consequences

- The client must use these three names exactly. Nothing on the client can
  exercise them until a server release carrying the columns is cut and
  `scripts/sync-server-pin.json` is bumped, per ADR 0005.
- `docs/openapi.yaml` and `docs/API.md` state Rule 2 as a deletion, not as an
  omission, including in the per-field schema descriptions — a client author who
  reads only the field docs still has to see it.
- A vault-password change and a new recovery key are both ordinary re-wraps
  guarded by `key_updated_at`. Neither is a first claim, and a second write from a
  current token is not a conflict.
- If envelopes ever multiply beyond two — the per-device envelope ADR 0005
  deferred — this naming stops scaling and the `collection_key_wraps` table that
  ADR 0005 describes becomes the right shape. A third envelope kind is the trigger
  to revisit, and this ADR with it.
