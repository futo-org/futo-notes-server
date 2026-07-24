---
status: accepted
date: 2026-07-24
---

# Use Mutation IDs for safe object-mutation retries

Clients may identify an intended create, update, or delete with an opaque `Mutation-Id` header. The server remembers the original outcome for a fixed 30 days so a retry cannot duplicate an object, advance a version or collection cursor twice, upload another blob, or publish another notification.

## Considered options

- Infer retry safety from object versions and blob keys. Rejected because it cannot make one-call object creation safe after a response is lost.
- Require Mutation IDs immediately. Rejected because the write routes are already published and existing clients do not send them.

## Consequences

- Mutation IDs are initially optional and advertised through capability discovery as supported but not required.
- Reusing a Mutation ID for a different mutation kind, object, or version is rejected; a retry of the same intent returns its recorded outcome without comparing opaque ciphertext.
- New client behavior, the 30-day guarantee, and any future transition to required Mutation IDs must be documented in the client protocol.
