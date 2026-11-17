package api

import (
	"context"
	"database/sql"
	"errors"

	"futo-notes-server/internal/collections"
	"futo-notes-server/internal/db"
)

// recoveryEnvelopes keeps the hosted API's recovery envelopes in
// collection_recovery_envelopes.
type recoveryEnvelopes struct{}

// Load implements collections.RecoveryStore. A row missing any field reads as
// no envelope: half an envelope opens nothing.
func (recoveryEnvelopes) Load(ctx context.Context, tx *db.Tx, collectionID string) (collections.RecoveryEnvelope, bool, error) {
	var salt, encrypted sql.NullString
	var kdf []byte
	err := tx.QueryRowContext(ctx, `SELECT salt, kdf, encrypted_vault_key
		FROM collection_recovery_envelopes WHERE collection_id = $1`, collectionID).Scan(&salt, &kdf, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return collections.RecoveryEnvelope{}, false, nil
	}
	if err != nil || !salt.Valid || kdf == nil || !encrypted.Valid {
		return collections.RecoveryEnvelope{}, false, err
	}
	return collections.RecoveryEnvelope{Salt: salt.String, KDF: kdf, EncryptedVaultKey: encrypted.String}, true, nil
}

// Replace implements collections.RecoveryStore.
func (recoveryEnvelopes) Replace(ctx context.Context, tx *db.Tx, collectionID string, envelope *collections.RecoveryEnvelope) error {
	if envelope == nil {
		_, err := tx.ExecContext(ctx, `DELETE FROM collection_recovery_envelopes WHERE collection_id = $1`, collectionID)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO collection_recovery_envelopes (collection_id, salt, kdf, encrypted_vault_key)
		VALUES ($1, $2, $3::jsonb, $4)
		ON CONFLICT (collection_id) DO UPDATE
		SET salt = excluded.salt, kdf = excluded.kdf, encrypted_vault_key = excluded.encrypted_vault_key`,
		collectionID, envelope.Salt, string(envelope.KDF), envelope.EncryptedVaultKey)
	return err
}
