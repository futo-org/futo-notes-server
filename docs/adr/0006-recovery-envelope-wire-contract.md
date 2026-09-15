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

These names were **chosen, not specified** — mirroring the password-envelope
fields was the only thing to go on. Nothing else about the contract depends on
them, so a client that wants different names is asking for a rename, not a
redesign; say so before its pin moves and it is a column rename and a find-replace.

(There is no ADR 0003 here: ADR 0004 reserves that number for the folder-subtree
sharing ADR, which lives on the architecture-direction branch and is not yet on
`main`.)

**Rule 1: all three or none.** A `PUT /api/collections/{id}/key` carrying some but
not all three answers `400`. Half an envelope is a recovery door that cannot be
opened, and storing one would mean a vault that advertises a recovery key the
Recovery Key cannot actually unlock. A row that somehow holds a partial set reads
back as no envelope at all, for the same reason. An explicit `null` and an omitted
field mean the same thing on the way in.

**Rule 2: omitting the recovery fields deletes the stored envelope.** A `PUT`
replaces the whole of the key material, both envelopes in one write. There is no
partial update and no way to address one envelope on its own.

Rule 2 is the one that can lose a person's vault, so what it costs is stated
before what it buys.

**Changing the vault password re-wraps the *same* vault key.** The recovery
envelope is untouched by that change and would still have opened the vault, so a
client that changes the vault password without re-sending the recovery envelope
**destroys a live recovery door** — the person's way back in is gone, and neither
they nor the server can tell until they need it. The same holds in reverse when
issuing a new recovery key. Such a client **must** send both envelopes in the one
request; client ADR 0003 already says it does, and the hazard is a client that
forgets. This is why the rule is stated as a deletion everywhere rather than left
to be inferred from "optional".

Deletion is nonetheless the right default, because of the one client that cannot
send both: a **v1 client** — predating recovery keys, or self-hosted password mode
— mints a **fresh vault key** when it re-wraps rather than re-wrapping the
existing one. The recovery envelope it omits wraps a key that no longer exists, so
retaining it would leave a recovery door that opens nothing while a person holding
that recovery key believed they had a way in. Nothing in the request distinguishes
the two clients, and the server cannot read either envelope to find out, so the
rule has to be one or the other.

## Considered options

- **Treat an omitted recovery envelope as "leave unchanged", updating only the
  fields sent.** It is the forgiving reading, and the one a client author assumes
  from "optional". Rejected because it strands the v1 case: that re-wrap leaves an
  envelope wrapping a dead vault key, nothing in the request says which client
  sent it, and the server cannot read either envelope to find out. A recovery key
  that silently stops working is worse than one the person is told to re-create.
  Replace-everything also keeps the endpoint's existing shape — a `PUT` of key
  material has always been "here is the material", never a patch.
- **A separate endpoint or verb per envelope.** Rejected as more surface than the
  problem needs. The two envelopes share one revision token, so splitting the
  write splits the guard, and two writes that must both land is a worse failure
  mode than one that carries both.
- **A `CHECK` constraint enforcing all-or-nothing in the schema.** Wanted, skipped
  for SQLite: `ALTER TABLE` cannot add a table-level `CHECK`, so it would mean
  rebuilding `collections`, which other tables reference by foreign key. The
  invariant is held in Go instead — the handler rejects a partial set and `PutKey`
  writes all three columns in one statement, so no code path produces a partial row.
- **Names not derived from the existing three** (`recovery_salt`, an `envelopes`
  JSON document, and so on). Rejected: the parallel naming is what makes the two
  envelopes obviously the same shape.

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
