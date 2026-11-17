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
}

func (m KeyMaterial) revisionToken() string {
	return m.KeyUpdatedAt.UTC().Format(time.RFC3339Nano)
}

// KeyInput is the key material a client submits to claim or rotate the key.
type KeyInput struct {
	KeySalt           string
	KeyKDF            json.RawMessage
	EncryptedVaultKey string
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

// GetKey returns the collection's key material and reports whether a key has
// been claimed yet. It returns ErrNotFound when the collection is absent or
// owned by another user.
func GetKey(ctx context.Context, database *db.DB, userID, id string) (KeyMaterial, bool, error) {
	if !ids.IsUUID(id) {
		return KeyMaterial{}, false, ErrNotFound
	}
	key, ok, err := scanKeyMaterial(database.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM collections WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return KeyMaterial{}, false, ErrNotFound
	}
	return key, ok, err
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

// PutKey claims or rotates the collection's key material and returns what is
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
func PutKey(ctx context.Context, database *db.DB, userID, id string, in KeyInput, prevToken *string) (KeyMaterial, error) {
	if !ids.IsUUID(id) {
		return KeyMaterial{}, ErrNotFound
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return KeyMaterial{}, err
	}
	defer tx.Discard()

	current, claimed, err := scanKeyMaterial(tx.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM collections WHERE id = $1 AND user_id = $2`+database.Dialect().ForUpdate(), id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return KeyMaterial{}, ErrNotFound
	}
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
		 SET key_salt = $1, key_kdf = $2`+database.Dialect().JSONCast()+`, encrypted_vault_key = $3, key_updated_at = $5
		 WHERE id = $4 RETURNING `+keyColumns,
		in.KeySalt, string(in.KeyKDF), in.EncryptedVaultKey, id, db.Timestamp(nextRevision(current, claimed))))
	if err != nil {
		return KeyMaterial{}, err
	}
	return stored, tx.Commit()
}
