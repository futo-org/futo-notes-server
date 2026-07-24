---
status: accepted
date: 2026-07-24
---

# Track every blob in one authoritative ledger

Every stored blob is recorded in one authoritative ledger as staged, claimed, retained, or purgeable, and each claimed blob belongs to exactly one object version. The Collection Contents module owns these state transitions because sharing blobs or tracking their lifetime across unrelated tables makes safe cleanup and atomic claiming substantially harder.

## Considered options

- Allow blobs to be shared and add reference counting. Rejected because it complicates every replacement, collection deletion, and cleanup race for little expected storage benefit.
- Add only a staged-blob table beside `objects` and `orphaned_blobs`. Rejected because one blob's lifetime would remain spread across three sources of truth.

## Consequences

- Separate uploads remain staged for a fixed 24 hours unless an object mutation claims them.
- Staging is the only step that writes blob bytes, and it always runs outside the mutation transaction — otherwise storing ciphertext would hold the collection's row lock, and a pooled connection, across the upload. Claiming a staged blob inside the transaction is a row lock and nothing more.
- Clients may directly delete only staged blobs; claimed and retained blobs are controlled by the Collection Contents module.
- Deleting a collection makes all of its blobs immediately purgeable.
- Migration backfills known blobs, stages previously untracked storage files for 24 hours, and preserves legacy shared blobs as non-deletable exceptions instead of risking data loss.
- The old `orphaned_blobs` table remains untouched for safe downgrade and migration auditing, but runtime code no longer reads or writes it; `blob_ledger` is the sole authority.
