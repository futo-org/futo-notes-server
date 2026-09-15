// Package pairing owns Device Pairings: the short-lived, single-use relay
// that carries a vault key from a device that is already unlocked to a new
// one on the same account, so the new device does not have to be told the
// Vault Password.
//
// The new device publishes a one-time public key, the unlocked device posts
// the vault key sealed to it, and the new device collects that ciphertext
// exactly once. The server never holds anything that could open it: the
// private half of the one-time key never leaves the new device, and no
// vault-key plaintext, Vault Password, or Recovery Key is in this table or
// anywhere near this package.
package pairing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"futo-notes-server/internal/db"
)

// TTL is how long a pairing stays usable, counted from creation and not
// restarted by the post. Every step is hands-on and in one sitting — show a
// QR code, scan it, confirm, collect — with no trip through an identity
// provider to cover, so this is shorter than the Login Hand-off's window.
// The vault key's ciphertext is what sits here in the meantime, which is the
// other reason to keep it short.
const TTL = 5 * time.Minute

// idBytes is how much entropy a pairing id carries. The account check is what
// actually keeps a pairing private; an unguessable id means a stranger cannot
// even ask about one.
const idBytes = 32

func newID() string {
	var b [idBytes]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Offer is what the new device publishes: its one-time public key, and what
// to call it on the confirmation the unlocked device shows. All three are
// opaque to the server.
type Offer struct {
	PublicKey  string
	DeviceName string
	Platform   string
}

// Pairing is a freshly created relay, as the new device needs to hear it.
type Pairing struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Create opens a pairing for userID and returns it.
func Create(ctx context.Context, database *db.DB, userID string, offer Offer) (Pairing, error) {
	created := Pairing{ID: newID(), ExpiresAt: time.Now().Add(TTL).UTC()}
	_, err := database.ExecContext(ctx,
		`INSERT INTO device_pairings (id, user_id, public_key, device_name, platform, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		created.ID, userID, offer.PublicKey, offer.DeviceName, offer.Platform,
		db.Timestamp(created.ExpiresAt))
	if err != nil {
		return Pairing{}, err
	}
	return created, nil
}

// DeliverCode says what posting a vault key to a pairing found.
type DeliverCode int

const (
	// DeliverGone is a pairing that is unknown, expired, already collected,
	// or owned by another account. They are one answer on purpose, so a
	// stranger cannot learn whether a pairing id exists.
	DeliverGone DeliverCode = iota
	// DeliverTaken is a pairing that already carries a key. Only the owning
	// account can reach this, so it says nothing a stranger could use.
	DeliverTaken
	// DeliverOK is the key stored, waiting to be collected.
	DeliverOK
)

// Deliver stores the sealed vault key on the pairing, which has to belong to
// userID. Reading the pairing's state and filling it happen in one
// transaction, so of two devices posting at once exactly one reaches
// DeliverOK and the other is told the pairing is taken.
func Deliver(ctx context.Context, database *db.DB, userID, id, ciphertext string) (DeliverCode, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return DeliverGone, err
	}
	defer tx.Rollback()

	stored, live, err := scanPairing(tx.QueryRowContext(ctx, selectPairing+tx.Dialect().ForUpdate(), id, userID))
	if err != nil {
		return DeliverGone, err
	}
	if !live {
		return DeliverGone, nil
	}
	if stored.Valid {
		return DeliverTaken, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE device_pairings SET ciphertext = $1 WHERE id = $2`, ciphertext, id); err != nil {
		return DeliverGone, err
	}
	return DeliverOK, tx.Commit()
}

// CollectCode says what a poll found.
type CollectCode int

const (
	// CollectGone is a pairing that is unknown, expired, already collected,
	// or owned by another account — the same single answer DeliverGone is.
	CollectGone CollectCode = iota
	// CollectPending is a pairing still waiting on the unlocked device.
	CollectPending
	// CollectOK carries the sealed vault key.
	CollectOK
)

// Collected is the outcome of one poll, and for CollectOK the sealed key.
type Collected struct {
	Code       CollectCode
	Ciphertext string
}

// Collect hands over the sealed vault key and consumes the pairing, which has
// to belong to userID. Reading the ciphertext and deleting the row happen in
// one transaction, so exactly one poll of a given pairing ever reaches
// CollectOK; every later one is CollectGone.
func Collect(ctx context.Context, database *db.DB, userID, id string) (Collected, error) {
	// Waiting is the common case: the new device polls every second or so
	// from the moment it shows its code until the key arrives. A transaction
	// takes SQLite's whole-database write lock the moment it opens, so ask
	// without one first and only open a transaction when there is something
	// to hand over. That transaction reads the row again under its own lock,
	// so this costs the single-collection guarantee nothing.
	stored, live, err := scanPairing(database.QueryRowContext(ctx, selectPairing, id, userID))
	if err != nil {
		return Collected{}, err
	}
	if !live {
		return Collected{Code: CollectGone}, nil
	}
	if !stored.Valid {
		return Collected{Code: CollectPending}, nil
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Collected{}, err
	}
	defer tx.Rollback()

	stored, live, err = scanPairing(tx.QueryRowContext(ctx, selectPairing+tx.Dialect().ForUpdate(), id, userID))
	if err != nil {
		return Collected{}, err
	}
	if !live {
		return Collected{Code: CollectGone}, nil
	}
	if !stored.Valid {
		return Collected{Code: CollectPending}, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_pairings WHERE id = $1`, id); err != nil {
		return Collected{}, err
	}
	return Collected{Code: CollectOK, Ciphertext: stored.String}, tx.Commit()
}

// selectPairing reads one account's pairing. Scoping it by user_id is what
// makes a pairing belonging to someone else indistinguishable from one that
// was never created, which is what keeps the two answers the same on the wire.
const selectPairing = `SELECT ciphertext, expires_at FROM device_pairings WHERE id = $1 AND user_id = $2`

// scanPairing reads that row and reports whether the pairing is still usable.
// Rows past their window read as gone here; the hourly sweep deletes them.
func scanPairing(row *sql.Row) (sql.NullString, bool, error) {
	var ciphertext sql.NullString
	var expiresAt db.Time
	if err := row.Scan(&ciphertext, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.NullString{}, false, nil
		}
		return sql.NullString{}, false, err
	}
	return ciphertext, expiresAt.Time.After(time.Now()), nil
}
