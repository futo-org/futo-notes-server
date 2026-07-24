# E2EE Sync

This context coordinates versioned, opaque encrypted data across a user's devices without learning the data's plaintext meaning.

## Language

**Collection**:
A user-owned container whose ordered changes are tracked by a collection-wide cursor.

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
The retained object state produced by soft deletion at a specific object version and collection change. Repeated deletion returns the existing tombstone rather than creating another change.

**Blob Lifetime**:
The progression of a blob from current object content, through optional merge-ancestor retention, to eventual deletion.
_Avoid_: Blob cleanup
