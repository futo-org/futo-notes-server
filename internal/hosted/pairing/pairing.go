// Package pairing is the relay that carries a vault key from an unlocked
// device to a new device of the same account: the new device opens a pairing
// with a one-time public key, the unlocked device posts the vault key sealed
// to it, and the new device collects it once. The server never reads the
// sealed key, and never logs it.
package pairing

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"uuid"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/ids"
)

// Window is how long a pairing lives from creation. Posting the key does not
// extend it.
const Window = 5 * time.Minute

var (
	// ErrNotFound reports a pairing that is unknown, expired, already
	// collected or another account's, which callers must not tell apart.
	ErrNotFound = errors.New("pairing not found")
	// ErrAlreadyKeyed reports a second key posted to the caller's pairing.
	ErrAlreadyKeyed = errors.New("pairing already has a key")
)

// Offer is what the new device publishes when it opens a pairing. It is
// stored and never served: the unlocked device reads it from the QR code.
type Offer struct {
	PublicKey  string
	DeviceName string
	Platform   string
}

// Create opens a pairing for userID and returns its id and expiry.
func Create(ctx context.Context, database *db.DB, userID string, offer Offer) (string, time.Time, error) {
	id := uuid.NewV7().String()
	var expiresAt time.Time
	err := database.QueryRowContext(ctx, `INSERT INTO pairings (id, user_id, public_key, device_name, platform, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6)) RETURNING expires_at`,
		id, userID, offer.PublicKey, offer.DeviceName, offer.Platform, Window.Seconds()).Scan(&expiresAt)
	return id, expiresAt.UTC(), err
}

// Deliver stores ciphertext as the key of userID's live pairing id. It
// returns ErrAlreadyKeyed when one was already posted.
func Deliver(ctx context.Context, database *db.DB, userID, id, ciphertext string) error {
	if !ids.IsUUID(id) {
		return ErrNotFound
	}
	result, err := database.ExecContext(ctx, `UPDATE pairings SET ciphertext = $1
		WHERE id = $2 AND user_id = $3 AND expires_at > now() AND ciphertext IS NULL`, ciphertext, id, userID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n == 1 {
		return err
	}
	live, err := isLive(ctx, database, userID, id)
	if err == nil && live {
		return ErrAlreadyKeyed
	}
	if err == nil {
		return ErrNotFound
	}
	return err
}

// Collect returns the key posted to userID's live pairing id and deletes the
// pairing in the same statement, so of two collectors only one gets it.
// delivered is false while no key has been posted.
func Collect(ctx context.Context, database *db.DB, userID, id string) (ciphertext string, delivered bool, err error) {
	if !ids.IsUUID(id) {
		return "", false, ErrNotFound
	}
	err = database.QueryRowContext(ctx, `DELETE FROM pairings
		WHERE id = $1 AND user_id = $2 AND expires_at > now() AND ciphertext IS NOT NULL
		RETURNING ciphertext`, id, userID).Scan(&ciphertext)
	if err == nil {
		return ciphertext, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	live, err := isLive(ctx, database, userID, id)
	if err == nil && !live {
		err = ErrNotFound
	}
	return "", false, err
}

// isLive reports whether userID has an unexpired pairing id.
func isLive(ctx context.Context, database *db.DB, userID, id string) (bool, error) {
	var live bool
	err := database.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pairings
		WHERE id = $1 AND user_id = $2 AND expires_at > now())`, id, userID).Scan(&live)
	return live, err
}

// DeleteExpired removes the pairings past their window, keyed or not,
// returning how many.
func DeleteExpired(ctx context.Context, database *db.DB) (int64, error) {
	result, err := database.ExecContext(ctx, `DELETE FROM pairings WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
