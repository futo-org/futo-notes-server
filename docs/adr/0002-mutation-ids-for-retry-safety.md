---
status: accepted
date: 2026-07-24
---

# Use Mutation IDs for safe object-mutation retries

Clients may identify an intended create, update, or delete with an opaque `Mutation-Id` header. The server remembers the original outcome for a fixed 30 days so a retry cannot duplicate an object, advance a version or collection cursor twice, bind another blob to an object version, or publish another notification.

## Considered options

- Infer retry safety from object versions and blob keys. Rejected because it cannot make one-call object creation safe after a response is lost.
- Require Mutation IDs immediately. Rejected because the write routes are already published and existing clients do not send them.

## Consequences

- Mutation IDs are initially optional and advertised through capability discovery as supported but not required.
- Reusing a Mutation ID for a different mutation kind, object, or version is rejected; a retry of the same intent returns its recorded outcome without comparing opaque ciphertext.
- A retried one-call mutation stages its ciphertext before the Mutation ID is checked, because staging deliberately happens outside the mutation transaction (see [0001](./0001-authoritative-blob-ledger.md)). That blob is never claimed and expires on the 24-hour staging window, so the object keeps the original ciphertext and retries are cheap but not free.
- New client behavior, the 30-day guarantee, and any future transition to required Mutation IDs must be documented in the client protocol.
