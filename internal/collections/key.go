package collections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"futo-notes-server/internal/db"
)

// KeyMaterial is the client-side vault key material. Every field is opaque to
// the server. KeyUpdatedAt doubles as the re-wrap revision token: it goes out
// as RFC 3339 with nanoseconds and comes back verbatim in
// previous_key_updated_at, so staleness checks compare that exact rendering.
//
// The three Recovery fields are the vault key wrapped under the Recovery Key
// instead of the Vault Password. They are optional and absent together: a
// vault whose client knows only the Vault Password has no recovery door, and
// password-mode self-hosted never writes one.
type KeyMaterial struct {
	KeySalt           string          `json:"key_salt"`
	KeyKDF            json.RawMessage `json:"key_kdf"`
	EncryptedVaultKey string          `json:"encrypted_vault_key"`
	KeyUpdatedAt      time.Time       `json:"key_updated_at"`

	RecoveryKeySalt           *string         `json:"recovery_key_salt"`
	RecoveryKeyKDF            json.RawMessage `json:"recovery_key_kdf"`
	RecoveryEncryptedVaultKey *string         `json:"recovery_encrypted_vault_key"`
}

func (m KeyMaterial) revisionToken() string {
	return m.KeyUpdatedAt.UTC().Format(time.RFC3339Nano)
}

// KeyInput is one complete set of key material to store. The Recovery fields
// are all set or all nil; the handler rejects a partial set before it gets
// here.
type KeyInput struct {
	KeySalt           string
	KeyKDF            json.RawMessage
	EncryptedVaultKey string

	RecoveryKeySalt           *string
	RecoveryKeyKDF            json.RawMessage
	RecoveryEncryptedVaultKey *string
}

type PutKeyOutcome int

const (
	PutKeyOK PutKeyOutcome = iota
	PutKeyNotFound
	// PutKeyConflict is a stale revision token; the returned material is the
	// authoritative current key.
	PutKeyConflict
	// PutKeyNoCurrentKey is a revision token supplied when nothing is stored,
	// which no legitimate client can produce.
	PutKeyNoCurrentKey
)

const keyColumns = `key_salt, key_kdf, encrypted_vault_key, key_updated_at, ` +
	`recovery_key_salt, recovery_key_kdf, recovery_encrypted_vault_key`

// scanKeyMaterial reads the key columns, returning nil when the collection has
// no claimed key (the password envelope's columns are set together, so
// key_updated_at stands in for all of them). The recovery columns are their own
// all-or-nothing group, absent on a vault with no recovery door.
func scanKeyMaterial(row scanner) (*KeyMaterial, error) {
	var salt, evk, recoverySalt, recoveryEVK sql.NullString
	var kdf, recoveryKDF []byte
	var updatedAt db.NullTime
	if err := row.Scan(&salt, &kdf, &evk, &updatedAt, &recoverySalt, &recoveryKDF, &recoveryEVK); err != nil {
		return nil, err
	}
	if !updatedAt.Valid {
		return nil, nil
	}
	material := &KeyMaterial{
		KeySalt:           salt.String,
		KeyKDF:            json.RawMessage(kdf),
		EncryptedVaultKey: evk.String,
		KeyUpdatedAt:      updatedAt.Time.UTC(),
	}
	// All three or none: half an envelope is a recovery door that cannot be
	// opened, so it reads back the same as no door at all rather than as a
	// recovery envelope the client would then try to use.
	if recoverySalt.Valid && recoveryKDF != nil && recoveryEVK.Valid {
		material.RecoveryKeySalt = &recoverySalt.String
		material.RecoveryKeyKDF = json.RawMessage(recoveryKDF)
		material.RecoveryEncryptedVaultKey = &recoveryEVK.String
	}
	return material, nil
}

// nullableText and nullableJSON render an absent recovery envelope as SQL NULL
// rather than as an empty string, which jsonb would reject and which a later
// read could not tell from a real value.
func nullableText(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableJSON(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}

// GetKey reports whether the collection exists for this user and, if so,
// returns its key material — nil when no key has been claimed yet.
func GetKey(ctx context.Context, database *db.DB, userID, id string) (bool, *KeyMaterial, error) {
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

// nextRevision picks the key_updated_at to store. Timestamps are kept to the
// millisecond, so wall-clock time alone lets a re-wrap reuse the revision
// token it was meant to replace — two clients holding that token would then
// both pass the staleness check and the second would overwrite the first.
// Stepping one millisecond past the stored revision keeps the token strictly
// increasing without changing its shape.
func nextRevision(current *KeyMaterial) time.Time {
	updatedAt := time.Now().UTC().Truncate(time.Millisecond)
	if current == nil {
		return updatedAt
	}
	if earliest := current.KeyUpdatedAt.Truncate(time.Millisecond).Add(time.Millisecond); updatedAt.Before(earliest) {
		return earliest
	}
	return updatedAt
}

// PutKey claims or re-wraps the collection's key material.
//
// Claim (prevToken nil): stores the material if none exists; otherwise the
// stored material wins and is returned unchanged, which also makes identical
// resubmissions idempotent.
//
// Re-wrap (prevToken set): replaces the material only when the token matches
// the current revision. Both envelopes are replaced together, so a client that
// re-wraps only one still sends both; an input with no recovery envelope
// clears the stored one.
func PutKey(ctx context.Context, database *db.DB, userID, id string, in KeyInput, prevToken *string) (PutKeyOutcome, *KeyMaterial, error) {
	if !isUUID(id) {
		return PutKeyNotFound, nil, nil
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()

	current, err := scanKeyMaterial(tx.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM collections WHERE id = $1 AND user_id = $2`+database.Dialect().ForUpdate(), id, userID))
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

	jsonCast := database.Dialect().JSONCast()
	stored, err := scanKeyMaterial(tx.QueryRowContext(ctx,
		`UPDATE collections
		 SET key_salt = $1, key_kdf = $2`+jsonCast+`, encrypted_vault_key = $3, key_updated_at = $5,
		     recovery_key_salt = $6, recovery_key_kdf = $7`+jsonCast+`, recovery_encrypted_vault_key = $8
		 WHERE id = $4 RETURNING `+keyColumns,
		in.KeySalt, string(in.KeyKDF), in.EncryptedVaultKey, id, db.Timestamp(nextRevision(current)),
		nullableText(in.RecoveryKeySalt), nullableJSON(in.RecoveryKeyKDF), nullableText(in.RecoveryEncryptedVaultKey)))
	if err != nil {
		return 0, nil, err
	}
	return PutKeyOK, stored, tx.Commit()
}
