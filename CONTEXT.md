# E2EE Sync

This context coordinates versioned, opaque encrypted data across a user's devices without learning the data's plaintext meaning.

## Language

**Collection**:
A user-owned container whose ordered changes are tracked by a collection-wide cursor.

**Vault**:
The product name for an account's root collection. Not a separate concept: "share the vault" and "share a folder" are the same operation once folder subtrees exist.
_Avoid_: Workspace, notebook

**Collection Contents**:
The objects, object versions, blobs, retained merge ancestors, and ordered changes inside a collection. Collection identity and encrypted vault-key material are not collection contents.
_Avoid_: Collection data

**Object**:
A versioned item in a collection whose content is held in an opaque encrypted blob.

**Blob**:
An opaque encrypted byte sequence referenced by exactly one object version. Blobs are never shared across objects.

**Staged Blob**:
A blob uploaded through the two-call write path but not yet claimed by an object mutation.
_Avoid_: Unattached blob, temporary blob

**Claimed Blob**:
A blob bound exclusively to an object version. It cannot be deleted independently from the collection contents that claim it.

**Retained Blob**:
A formerly claimed blob kept as a merge ancestor after an object advances to a newer version.
_Avoid_: Orphaned blob

**Purgeable Blob**:
A blob with no remaining role in collection contents and therefore eligible for asynchronous deletion. Deleting a collection makes its claimed and retained blobs immediately purgeable.

**Legacy Shared Blob**:
A migration-only quarantine state for a historic blob referenced by more than one object. It is never automatically deleted.

**Object Mutation**:
An object creation, versioned update, or soft deletion, including the corresponding collection change and blob-reference transition.
_Avoid_: Object write

**Mutation ID**:
A client-generated opaque identifier for one intended object mutation. Retries reuse the same mutation ID and resolve to the original outcome.
_Avoid_: Operation key, idempotency key

**Tombstone**:
The retained object state produced by soft deletion at a specific object version and collection change.

**Blob Lifetime**:
The progression of a blob from current object content, through optional merge-ancestor retention, to eventual deletion.
_Avoid_: Blob cleanup

## Sign-in

**Login Hand-off**:
A short-lived, single-use claim ticket the app opens a browser login with, and later redeems for the resulting session. The server finishes the identity-provider exchange; the app never touches provider tokens.
_Avoid_: Device flow, deep-link login, token relay

## Subscription

**Billing Customer**:
The account as the payment provider knows it, created the first time someone opens a checkout or the customer portal and never before. The account's sign-in `sub` is the external id both systems agree on, so a subscription event names the account without a lookup table. Code and comments at the provider boundary name the provider outright — "the account's Polar customer" — because there the concrete object is the point; the neutral term is for describing the concept.
_Avoid_: Payer, billing account, customer record

**Entitlement**:
Whether an account may currently write to the service. An account is entitled while its subscription is active or within the grace period after a failed payment; reads never depend on entitlement.
_Avoid_: Paid status, plan check, billing gate

**Grace Period**:
The fixed window after a payment failure during which an account stays entitled.
_Avoid_: Dunning window

**Plan**:
The named set of limits an account is entitled to: storage quota and per-blob cap. One plan exists today.
_Avoid_: Tier, product, SKU

**Storage Quota**:
The plan's ceiling on an account's total claimed and retained blob bytes. Staged and purgeable blobs do not count.
_Avoid_: Disk limit, usage cap

**Storage Usage**:
An account's current total of claimed and retained blob bytes, measured against its Storage Quota. It is a counter moved by the blob-ledger transitions, never a sum taken while a write is in flight; a recurring job recomputes it from the ledger and corrects drift.
