package collections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
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

type KeyInput struct {
	KeySalt           string
	KeyKDF            json.RawMessage
	EncryptedVaultKey string
}

type PutKeyOutcome int

const (
	PutKeyOK PutKeyOutcome = iota
	PutKeyNotFound
	// PutKeyConflict is a stale rotation token; the returned material is the
	// authoritative current key.
	PutKeyConflict
	// PutKeyNoCurrentKey is a rotation token supplied when nothing is stored,
	// which no legitimate client can produce.
	PutKeyNoCurrentKey
)

const keyColumns = `key_salt, key_kdf, encrypted_vault_key, key_updated_at`

// scanKeyMaterial reads the four key columns, returning nil when the
// collection has no claimed key (the columns are set together, so
// key_updated_at stands in for all four).
func scanKeyMaterial(row scanner) (*KeyMaterial, error) {
	var salt, evk sql.NullString
	var kdf []byte
	var updatedAt sql.NullTime
	if err := row.Scan(&salt, &kdf, &evk, &updatedAt); err != nil {
		return nil, err
	}
	if !updatedAt.Valid {
		return nil, nil
	}
	return &KeyMaterial{
		KeySalt:           salt.String,
		KeyKDF:            json.RawMessage(kdf),
		EncryptedVaultKey: evk.String,
		KeyUpdatedAt:      updatedAt.Time.UTC(),
	}, nil
}

// GetKey reports whether the collection exists for this user and, if so,
// returns its key material — nil when no key has been claimed yet.
func GetKey(ctx context.Context, database *sql.DB, userID, id string) (bool, *KeyMaterial, error) {
	if !isUUID(id) {
		return false, nil, nil
	}
	key, err := scanKeyMaterial(database.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM collections WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	return true, key, nil
}

// PutKey claims or rotates the collection's key material.
//
// Claim (prevToken nil): stores the material if none exists; otherwise the
// stored material wins and is returned unchanged, which also makes identical
// resubmissions idempotent.
//
// Rotation (prevToken set): replaces the material only when the token matches
// the current revision.
func PutKey(ctx context.Context, database *sql.DB, userID, id string, in KeyInput, prevToken *string) (PutKeyOutcome, *KeyMaterial, error) {
	if !isUUID(id) {
		return PutKeyNotFound, nil, nil
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()

	current, err := scanKeyMaterial(tx.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM collections WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return PutKeyNotFound, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}

	if prevToken == nil {
		if current != nil {
			return PutKeyOK, current, tx.Commit()
		}
	} else {
		if current == nil {
			return PutKeyNoCurrentKey, nil, nil
		}
		if current.revisionToken() != *prevToken {
			return PutKeyConflict, current, tx.Commit()
		}
	}

	stored, err := scanKeyMaterial(tx.QueryRowContext(ctx,
		`UPDATE collections
		 SET key_salt = $1, key_kdf = $2::jsonb, encrypted_vault_key = $3, key_updated_at = now()
		 WHERE id = $4 RETURNING `+keyColumns,
		in.KeySalt, string(in.KeyKDF), in.EncryptedVaultKey, id))
	if err != nil {
		return 0, nil, err
	}
	return PutKeyOK, stored, tx.Commit()
}
