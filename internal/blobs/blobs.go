// Package blobs owns the blob_ledger table and on-disk blob storage.
package blobs

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uuid"

	"futo-notes-server/internal/db"
)

// Store writes blobs under Dir using the storage key as a relative path,
// so a key like {userId}/{blobId} becomes a subdirectory plus file. This
// matches the layout the TypeScript server's FsBlobStore left on disk.
type Store struct {
	Dir string
}

func (s *Store) Put(key string, data []byte) error {
	full := filepath.Join(s.Dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0o644)
}

// Stage mints a storage key scoped to the user, records it in blob_ledger as
// staged, and writes the bytes. The ledger row goes first: if the file write
// fails, the row is a staged entry with no bytes behind it, and garbage
// collection removes it after 24 hours.
func Stage(ctx context.Context, database *db.DB, store *Store, userID string, data []byte) (string, error) {
	key := userID + "/" + uuid.NewV7().String()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO blob_ledger (blob_key, user_id, size_bytes, state, created_at, state_changed_at)
		 VALUES ($1, $2, $3, 'staged', $4, $4)`, key, userID, len(data), db.Timestamp(time.Now())); err != nil {
		return "", err
	}
	if err := store.Put(key, data); err != nil {
		return "", err
	}
	return key, nil
}

func (s *Store) Open(key string) (*os.File, error) {
	return os.Open(filepath.Join(s.Dir, filepath.FromSlash(key)))
}

// Remove reports success when the file is already gone, so callers can treat
// deletion as idempotent.
func (s *Store) Remove(key string) error {
	err := os.Remove(filepath.Join(s.Dir, filepath.FromSlash(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Delete removes a blob that is still staged, along with its ledger row. It
// reports false when the ledger holds the key in any other state, whoever the
// row names as owner: those are owned by object and collection lifetime, not
// by the client. A key with no
// ledger row is a legacy or reconciled file and is deleted outright. A key
// outside the caller's namespace is refused before any file is touched.
//
// The row goes first. If the file removal then fails, storage reconciliation
// re-adopts the file as staged and garbage collection purges it later.
func Delete(ctx context.Context, database *db.DB, store *Store, userID, key string) (bool, error) {
	if !strings.HasPrefix(key, userID+"/") {
		return false, nil
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Keyed on blob_key alone. The namespace prefix already establishes who
	// owns the key, and a row recorded against a different user_id — which
	// migration 010 can produce for a legacy_shared key — has to block the
	// delete rather than read as absent.
	var state string
	err = tx.QueryRowContext(ctx,
		`SELECT state FROM blob_ledger WHERE blob_key = $1`+database.Dialect().ForUpdate(), key).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	case state != "staged":
		return false, nil
	default:
		if _, err := tx.ExecContext(ctx, `DELETE FROM blob_ledger WHERE blob_key = $1`, key); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, store.Remove(key)
}
