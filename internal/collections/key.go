package collections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/ids"
)

// KeyMaterial is the client-side vault key material. Every field is opaque to
// the server. KeyUpdatedAt doubles as the rotation revision token: it goes out
// as RFC 3339 with nanoseconds and comes back verbatim in
// previous_key_updated_at, so rotation checks compare that exact rendering.
type KeyMaterial struct {
	KeySalt           string          `json:"key_salt"`
	KeyKDF            json.RawMessage `json:"key_kdf"`
	EncryptedVaultKey string          `json:"encrypted_vault_key"`
	KeyUpdatedAt      time.Time       `json:"key_updated_at"`
	// Only Keys with a RecoveryStore set it. Nil marshals no field at all,
	// so key material without one keeps the self-hosted bytes.
	*RecoveryEnvelope
}

// RecoveryEnvelope is the vault key wrapped a second time, under a recovery
// key instead of the vault password. Its fields are as opaque as the password
// envelope's.
type RecoveryEnvelope struct {
	Salt              string          `json:"recovery_key_salt"`
	KDF               json.RawMessage `json:"recovery_key_kdf"`
	EncryptedVaultKey string          `json:"recovery_encrypted_vault_key"`
}

func (m KeyMaterial) revisionToken() string {
	return m.KeyUpdatedAt.UTC().Format(time.RFC3339Nano)
}

// KeyInput is the key material a client submits to claim or rotate the key.
type KeyInput struct {
	KeySalt           string
	KeyKDF            json.RawMessage
	EncryptedVaultKey string
	// Recovery is the recovery envelope to store with the key, or nil to
	// store none. Keys without a RecoveryStore ignore it.
	Recovery *RecoveryEnvelope
}

// RecoveryStore keeps recovery envelopes, which the shared schema has no
// columns for, inside the key's own transaction.
type RecoveryStore interface {
	// Load returns the collection's envelope and whether it has one.
	Load(ctx context.Context, tx *db.Tx, collectionID string) (RecoveryEnvelope, bool, error)
	// Replace stores envelope as the collection's, deleting the stored one
	// when envelope is nil.
	Replace(ctx context.Context, tx *db.Tx, collectionID string, envelope *RecoveryEnvelope) error
}

// Keys reads and writes collections' key material in DB. With Recovery set
// (the hosted API) a recovery envelope travels with the key and shares its
// revision; without it (self-hosted) key material never carries one.
type Keys struct {
	DB       *db.DB
	Recovery RecoveryStore
}

// ErrNoCurrentKey reports a rotation token supplied when no key is stored,
// which no legitimate client can produce.
var ErrNoCurrentKey = errors.New("no existing key to rotate")

// KeyConflictError reports a rotation from a stale revision token. Current is
// the authoritative key material the client should resync from.
type KeyConflictError struct {
	Current KeyMaterial
}

// Error implements error.
func (e *KeyConflictError) Error() string {
	return "key conflict: rotation token is stale"
}

const keyColumns = `key_salt, key_kdf, encrypted_vault_key, key_updated_at`

// scanKeyMaterial reads the four key columns and reports whether the
// collection has a claimed key (the columns are set together, so
// key_updated_at stands in for all four).
func scanKeyMaterial(row db.Scanner) (KeyMaterial, bool, error) {
	var salt, evk sql.NullString
	var kdf []byte
	var updatedAt sql.Null[db.Time]
	if err := row.Scan(&salt, &kdf, &evk, &updatedAt); err != nil {
		return KeyMaterial{}, false, err
	}
	if !updatedAt.Valid {
		return KeyMaterial{}, false, nil
	}
	return KeyMaterial{
		KeySalt:           salt.String,
		KeyKDF:            json.RawMessage(kdf),
		EncryptedVaultKey: evk.String,
		KeyUpdatedAt:      updatedAt.V.UTC(),
	}, true, nil
}

// Get returns the collection's key material and reports whether a key has
// been claimed yet. It returns ErrNotFound when the collection is absent or
// owned by another user.
func (k Keys) Get(ctx context.Context, userID, id string) (KeyMaterial, bool, error) {
	if !ids.IsUUID(id) {
		return KeyMaterial{}, false, ErrNotFound
	}
	if k.Recovery == nil {
		key, ok, err := scanKeyMaterial(k.DB.QueryRowContext(ctx,
			`SELECT `+keyColumns+` FROM collections WHERE id = $1 AND user_id = $2`, id, userID))
		if errors.Is(err, sql.ErrNoRows) {
			return KeyMaterial{}, false, ErrNotFound
		}
		return key, ok, err
	}

	tx, err := k.DB.BeginTx(ctx, nil)
	if err != nil {
		return KeyMaterial{}, false, err
	}
	defer tx.Discard()
	// The share lock waits out a Put in flight, so the envelope read next
	// belongs to the same revision as the key.
	key, ok, err := k.lockedKey(ctx, tx, userID, id, k.DB.Dialect().ForShare())
	if err != nil {
		return KeyMaterial{}, false, err
	}
	return key, ok, tx.Commit()
}

// lockedKey reads the collection's key material under lock (a row-locking
// clause), with its recovery envelope when k keeps them.
func (k Keys) lockedKey(ctx context.Context, tx *db.Tx, userID, id, lock string) (KeyMaterial, bool, error) {
	key, claimed, err := scanKeyMaterial(tx.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM collections WHERE id = $1 AND user_id = $2`+lock, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return KeyMaterial{}, false, ErrNotFound
	}
	if err != nil || !claimed || k.Recovery == nil {
		return key, claimed, err
	}
	envelope, ok, err := k.Recovery.Load(ctx, tx, id)
	if ok {
		key.RecoveryEnvelope = &envelope
	}
	return key, claimed, err
}

// nextRevision picks the key_updated_at to store. Timestamps are kept to the
// millisecond, so wall-clock time alone lets a rotation reuse the revision
// token it was meant to replace — two clients holding that token would then
// both pass the staleness check and the second would overwrite the first.
// Stepping one millisecond past the stored revision keeps the token strictly
// increasing without changing its shape.
func nextRevision(current KeyMaterial, claimed bool) time.Time {
	updatedAt := time.Now().UTC().Truncate(time.Millisecond)
	if !claimed {
		return updatedAt
	}
	if earliest := current.KeyUpdatedAt.Truncate(time.Millisecond).Add(time.Millisecond); updatedAt.Before(earliest) {
		return earliest
	}
	return updatedAt
}

// Put claims or rotates the collection's key material and returns what is
// stored afterwards. It returns ErrNotFound when the collection is absent or
// owned by another user.
//
// Claim (prevToken nil): stores the material if none exists; otherwise the
// stored material wins and is returned unchanged, which also makes identical
// resubmissions idempotent.
//
// Rotation (prevToken set): replaces the material only when the token matches
// the current revision. A stale token yields a *KeyConflictError carrying the
// current material; a token with nothing stored yields ErrNoCurrentKey.
//
// With a RecoveryStore, a write replaces both envelopes: in.Recovery nil
// deletes the stored recovery envelope.
func (k Keys) Put(ctx context.Context, userID, id string, in KeyInput, prevToken *string) (KeyMaterial, error) {
	if !ids.IsUUID(id) {
		return KeyMaterial{}, ErrNotFound
	}

	tx, err := k.DB.BeginTx(ctx, nil)
	if err != nil {
		return KeyMaterial{}, err
	}
	defer tx.Discard()

	current, claimed, err := k.lockedKey(ctx, tx, userID, id, k.DB.Dialect().ForUpdate())
	if err != nil {
		return KeyMaterial{}, err
	}

	if prevToken == nil {
		if claimed {
			return current, tx.Commit()
		}
	} else {
		if !claimed {
			return KeyMaterial{}, ErrNoCurrentKey
		}
		if current.revisionToken() != *prevToken {
			if err := tx.Commit(); err != nil {
				return KeyMaterial{}, err
			}
			return KeyMaterial{}, &KeyConflictError{Current: current}
		}
	}

	stored, _, err := scanKeyMaterial(tx.QueryRowContext(ctx,
		`UPDATE collections
		 SET key_salt = $1, key_kdf = $2`+k.DB.Dialect().JSONCast()+`, encrypted_vault_key = $3, key_updated_at = $5
		 WHERE id = $4 RETURNING `+keyColumns,
		in.KeySalt, string(in.KeyKDF), in.EncryptedVaultKey, id, db.Timestamp(nextRevision(current, claimed))))
	if err != nil {
		return KeyMaterial{}, err
	}
	if k.Recovery != nil {
		if err := k.Recovery.Replace(ctx, tx, id, in.Recovery); err != nil {
			return KeyMaterial{}, err
		}
		stored.RecoveryEnvelope = in.Recovery
	}
	return stored, tx.Commit()
}
