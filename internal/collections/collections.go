// Package collections owns the collections table.
package collections

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"futo-notes-server/internal/uuidv7"
)

type Collection struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	CurrentVersion string    `json:"current_version"`
	CreatedAt      time.Time `json:"created_at"`
}

const selectColumns = `id, user_id, current_version, created_at`

// oldest is the collection the claim route hands out when an account holds
// several from before the one-vault cap: first by (created_at, id).
const selectOldest = `SELECT ` + selectColumns + ` FROM collections
	WHERE user_id = $1 ORDER BY created_at, id LIMIT 1`

type scanner interface {
	Scan(dest ...any) error
}

func scanCollection(row scanner) (Collection, error) {
	var c Collection
	var version int64
	if err := row.Scan(&c.ID, &c.UserID, &version, &c.CreatedAt); err != nil {
		return Collection{}, err
	}
	c.CurrentVersion = strconv.FormatInt(version, 10)
	c.CreatedAt = c.CreatedAt.UTC()
	return c, nil
}

// Claim returns the user's oldest collection, creating one if none exists,
// and reports whether it created it. The user row is locked so two devices
// claiming concurrently converge on one collection instead of forking.
func Claim(ctx context.Context, database *sql.DB, userID string) (Collection, bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Collection{}, false, err
	}
	defer tx.Rollback()

	var lockedID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&lockedID); err != nil {
		return Collection{}, false, err
	}

	c, err := scanCollection(tx.QueryRowContext(ctx, selectOldest, userID))
	created := false
	if errors.Is(err, sql.ErrNoRows) {
		c, err = scanCollection(tx.QueryRowContext(ctx,
			`INSERT INTO collections (id, user_id) VALUES ($1, $2)
			 RETURNING `+selectColumns, uuidv7.New(), userID))
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

func List(ctx context.Context, database *sql.DB, userID string) ([]Collection, error) {
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

// Get returns nil when the collection is absent or owned by another user, so
// the caller answers 404 either way and existence doesn't leak.
func Get(ctx context.Context, database *sql.DB, userID, id string) (*Collection, error) {
	if !isUUID(id) {
		return nil, nil
	}
	c, err := scanCollection(database.QueryRowContext(ctx,
		`SELECT `+selectColumns+` FROM collections WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// isUUID reports whether s is in canonical 8-4-4-4-12 form, which is what
// Postgres will accept as a uuid parameter without a cast error.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}
