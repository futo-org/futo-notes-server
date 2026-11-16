// Package collections owns the collections table.
package collections

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"uuid"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/ids"
)

// ErrNotFound reports that the collection does not exist or belongs to another
// user. The two cases are deliberately indistinguishable so existence doesn't
// leak across accounts.
var ErrNotFound = errors.New("collection not found")

// Collection is one user's sync namespace. CurrentVersion is the highest
// object version issued in it; it goes out as a JSON string.
type Collection struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	CurrentVersion int64     `json:"current_version,string"`
	CreatedAt      time.Time `json:"created_at"`
}

const selectColumns = `id, user_id, current_version, created_at`

// oldest is the collection the claim route hands out when an account holds
// several from before the one-vault cap: first by (created_at, id).
const selectOldest = `SELECT ` + selectColumns + ` FROM collections
	WHERE user_id = $1 ORDER BY created_at, id LIMIT 1`

func scanCollection(row db.Scanner) (Collection, error) {
	var c Collection
	var createdAt db.Time
	if err := row.Scan(&c.ID, &c.UserID, &c.CurrentVersion, &createdAt); err != nil {
		return Collection{}, err
	}
	c.CreatedAt = createdAt.Time
	return c, nil
}

// Claim returns the user's oldest collection, creating one if none exists,
// and reports whether it created it. The user row is locked so two devices
// claiming concurrently converge on one collection instead of forking.
func Claim(ctx context.Context, database *db.DB, userID string) (Collection, bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Collection{}, false, err
	}
	defer tx.Discard()

	var lockedID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1`+database.Dialect().ForUpdate(), userID).Scan(&lockedID); err != nil {
		return Collection{}, false, err
	}

	c, err := scanCollection(tx.QueryRowContext(ctx, selectOldest, userID))
	created := false
	if errors.Is(err, sql.ErrNoRows) {
		c, err = scanCollection(tx.QueryRowContext(ctx,
			`INSERT INTO collections (id, user_id, created_at) VALUES ($1, $2, $3)
			 RETURNING `+selectColumns, uuid.NewV7().String(), userID, db.Timestamp(time.Now())))
		created = true
	}
	if err != nil {
		return Collection{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return Collection{}, false, err
	}
	return c, created, nil
}

// List returns the user's collections ordered by (created_at, id). An empty
// result is a non-nil slice so it serializes as [].
func List(ctx context.Context, database *db.DB, userID string) ([]Collection, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT `+selectColumns+` FROM collections WHERE user_id = $1 ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cs := []Collection{}
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	return cs, rows.Err()
}

// Get returns the user's collection with the given id, or ErrNotFound when it
// is absent or owned by another user.
func Get(ctx context.Context, database *db.DB, userID, id string) (Collection, error) {
	if !ids.IsUUID(id) {
		return Collection{}, ErrNotFound
	}
	c, err := scanCollection(database.QueryRowContext(ctx,
		`SELECT `+selectColumns+` FROM collections WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Collection{}, ErrNotFound
	}
	return c, err
}

// Delete removes the user's collection, or returns ErrNotFound when it is
// absent or owned by another user. Objects cascade via FK. Claimed and retained blobs become purgeable for
// asynchronous maintenance to remove — staged blobs age out on their own and
// legacy_shared is never cleaned. Mutation results scoped to the collection
// are deleted, because that sync namespace no longer exists.
func Delete(ctx context.Context, database *db.DB, userID, id string) error {
	if !ids.IsUUID(id) {
		return ErrNotFound
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Discard()

	var lockedID string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM collections WHERE id = $1 AND user_id = $2`+database.Dialect().ForUpdate(), id, userID).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE blob_ledger SET state = 'purgeable', state_changed_at = $2
		 WHERE collection_id = $1 AND state IN ('claimed', 'retained')`, id, db.Timestamp(time.Now())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM mutation_results WHERE collection_id = $1 AND user_id = $2`, id, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM collections WHERE id = $1`, id); err != nil {
		return err
	}

	return tx.Commit()
}
