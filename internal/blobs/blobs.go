// Package blobs owns the blob_ledger table and on-disk blob storage.
package blobs

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"uuid"
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
func Stage(ctx context.Context, database *sql.DB, store *Store, userID string, data []byte) (string, error) {
	key := userID + "/" + uuid.NewV7().String()
	if _, err := database.ExecContext(ctx,
		`INSERT INTO blob_ledger (blob_key, user_id, size_bytes, state)
		 VALUES ($1, $2, $3, 'staged')`, key, userID, len(data)); err != nil {
		return "", err
	}
	if err := store.Put(key, data); err != nil {
		return "", err
	}
	return key, nil
}
