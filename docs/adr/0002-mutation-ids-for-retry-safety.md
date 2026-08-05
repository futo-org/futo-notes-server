---
status: accepted
date: 2026-07-24
---

# Use Mutation IDs for safe object-mutation retries

Clients may identify an intended create, update, or delete with an opaque, non-empty `Mutation-Id` of at most 128 characters. Framed batch creates use the transport-safe subset `A-Z`, `a-z`, `0-9`, `.`, `_`, `~`, and `-`; classic routes keep accepting the broader published syntax so retained outcomes remain replayable after upgrade. The server remembers successful create outcomes for the collection lifetime and other outcomes for 30 days so a retry cannot duplicate an object, advance a version or collection cursor twice, bind another blob to an object version, or publish another notification. Durable create ownership also makes a create-outcome lookup `404` definitive: the request never committed.

## Considered options

- Infer retry safety from object versions and blob keys. Rejected because it cannot make one-call object creation safe after a response is lost.
- Require Mutation IDs immediately. Rejected because the write routes are already published and existing clients do not send them.

## Consequences

- Mutation IDs are initially optional and advertised through capability discovery as supported but not required.
- Successful create outcomes are durable; failed creates, updates, and deletes retain the 30-day window.
- Creates claim their Mutation ID before blob staging; recovery returns a retryable in-progress result until the claim is finalized, and abandoned claims expire with staged blobs after 24 hours.
- New clients use the batch-safe syntax on every route so fallback cannot normalize an ID into a different value; classic retries and percent-encoded lookups preserve previously accepted IDs.
- Requests without a Mutation ID remain independent mutations for older clients. Each successful create keeps its submitted ciphertext, but the legacy duplicate-object risk remains after an ambiguous response.
- Reusing a Mutation ID for a different mutation kind, object, or version is rejected; a retry of the same intent returns its recorded outcome without comparing opaque ciphertext.
- Completed Mutation IDs are checked before one-call creates stage ciphertext, so ordinary replays do not create an extra blob. Concurrent requests may both stage after observing the same in-progress claim, but the final transaction permits only one to commit; the losing staged blob expires after 24 hours (see [0001](./0001-authoritative-blob-ledger.md)).
- New client behavior, both retention guarantees, and any future transition to required Mutation IDs must be documented in the client protocol.
