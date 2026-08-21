// Package jobs owns the server's periodic database and blob-storage cleanup.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"futo-notes-server/internal/blobs"
)

const reconciliationLimit = 500
const garbageCollectionBatchSize = 500

type SessionReapResult struct {
	Reaped int64 `json:"reaped"`
}

func ReapSessions(ctx context.Context, database *sql.DB) (SessionReapResult, error) {
	result, err := database.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	if err != nil {
		return SessionReapResult{}, err
	}
	reaped, err := result.RowsAffected()
	return SessionReapResult{Reaped: reaped}, err
}

type ReconciliationResult struct {
	Adopted int  `json:"adopted"`
	Skipped int  `json:"skipped"`
	CapHit  bool `json:"cap_hit"`
}

var errReconciliationCap = errors.New("storage reconciliation cap reached")

// ReconcileStorage adopts files that have no authoritative blob_ledger row.
// A missing storage directory is equivalent to an empty one on a fresh server.
func ReconcileStorage(ctx context.Context, database *sql.DB, store *blobs.Store) (ReconciliationResult, error) {
	var result ReconciliationResult
	err := filepath.WalkDir(store.Dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		relative, err := filepath.Rel(store.Dir, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		owner, _, ok := strings.Cut(key, "/")
		if !ok || !validUUID(owner) {
			result.Skipped++
			log.Printf("storage reconciliation: skipping %q: invalid owner", key)
			return nil
		}

		// The CTE distinguishes a missing owner from an existing ledger row in
		// one statement. It also closes the race between checking the user and
		// inserting the FK-backed row.
		var ownerExists, inserted bool
		err = database.QueryRowContext(ctx, `
			WITH owner AS (
				SELECT id FROM users WHERE id = $2
			), inserted AS (
				INSERT INTO blob_ledger (blob_key, user_id, size_bytes, state)
				SELECT $1, id, $3, 'staged' FROM owner
				ON CONFLICT (blob_key) DO NOTHING
				RETURNING 1
			)
			SELECT EXISTS (SELECT 1 FROM owner), EXISTS (SELECT 1 FROM inserted)`,
			key, owner, info.Size()).Scan(&ownerExists, &inserted)
		if err != nil {
			return err
		}
		if !ownerExists {
			result.Skipped++
			log.Printf("storage reconciliation: skipping %q: owner does not exist", key)
			return nil
		}
		if !inserted {
			return nil
		}
		result.Adopted++
		if result.Adopted == reconciliationLimit {
			result.CapHit = true
			return errReconciliationCap
		}
		return nil
	})

	if errors.Is(err, errReconciliationCap) {
		log.Printf("storage reconciliation: reached %d-file adoption cap", reconciliationLimit)
		return result, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	return result, err
}

type MutationExpiryResult struct {
	PendingExpired int64 `json:"pending_expired"`
	OtherExpired   int64 `json:"other_expired"`
}

func ExpireMutationResults(ctx context.Context, database *sql.DB) (MutationExpiryResult, error) {
	var result MutationExpiryResult
	pending, err := database.ExecContext(ctx, `
		DELETE FROM mutation_results
		WHERE created_at < now() - interval '24 hours'
		  AND result->>'status' IN ('102', 'pending')`)
	if err != nil {
		return result, err
	}
	if result.PendingExpired, err = pending.RowsAffected(); err != nil {
		return result, err
	}

	// Keep this legacy error mapping in sync with decodeStoredResult in
	// internal/objects/objects.go. Ambiguous legacy creates are retained.
	other, err := database.ExecContext(ctx, `
		DELETE FROM mutation_results
		WHERE created_at < now() - interval '30 days'
		  AND (result->>'status' IS NULL
		       OR result->>'status' NOT IN ('102', 'pending'))
		  AND NOT (kind = 'create' AND (
			(result->>'status' IS NOT NULL AND result->>'status' ~ '^2')
			OR (result->>'status' IS NULL
				AND (result->>'error' IS NULL OR result->>'error' NOT IN
					('not found', 'version conflict', 'blob is not staged',
					 'Mutation-Id reused for different intent')))
		  ))`)
	if err != nil {
		return result, err
	}
	result.OtherExpired, err = other.RowsAffected()
	return result, err
}

type BlobGCResult struct {
	RowsPurged   int `json:"rows_purged"`
	FilesRemoved int `json:"files_removed"`
}

// GarbageCollectBlobs deletes the authoritative row before its file. If the
// file deletion fails, reconciliation will adopt it again for a later pass.
func GarbageCollectBlobs(ctx context.Context, database *sql.DB, store *blobs.Store) (BlobGCResult, error) {
	var result BlobGCResult
	var removalErrors []error

	for {
		keys, err := eligibleBlobKeys(ctx, database)
		if err != nil {
			return result, errors.Join(append(removalErrors, err)...)
		}
		if len(keys) == 0 {
			return result, errors.Join(removalErrors...)
		}

		purgedThisPass := 0
		for _, key := range keys {
			purged, err := purgeBlobRow(ctx, database, key)
			if err != nil {
				return result, errors.Join(append(removalErrors, err)...)
			}
			if !purged {
				continue
			}
			purgedThisPass++
			result.RowsPurged++

			existed, err := blobFileExists(store, key)
			if err != nil {
				removalErrors = append(removalErrors, fmt.Errorf("stating blob %q: %w", key, err))
				continue
			}
			if err := store.Remove(key); err != nil {
				removalErrors = append(removalErrors, fmt.Errorf("removing blob %q: %w", key, err))
				continue
			}
			if existed {
				result.FilesRemoved++
			}
		}

		// Every selected key may have been claimed or handled by another
		// server before its short transaction acquired the lock. Let that
		// server finish the pass instead of spinning on the same locked keys.
		if purgedThisPass == 0 {
			return result, errors.Join(removalErrors...)
		}
	}
}

func eligibleBlobKeys(ctx context.Context, database *sql.DB) ([]string, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT blob_key FROM blob_ledger
		WHERE (state = 'staged' AND state_changed_at < now() - interval '24 hours')
		   OR (state = 'retained' AND state_changed_at < now() - interval '365 days')
		   OR state = 'purgeable'
		ORDER BY blob_key
		LIMIT $1`, garbageCollectionBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func purgeBlobRow(ctx context.Context, database *sql.DB, key string) (bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var lockedKey string
	err = tx.QueryRowContext(ctx, `
		SELECT blob_key FROM blob_ledger
		WHERE blob_key = $1 AND (
			(state = 'staged' AND state_changed_at < now() - interval '24 hours')
			OR (state = 'retained' AND state_changed_at < now() - interval '365 days')
			OR state = 'purgeable'
		)
		FOR UPDATE SKIP LOCKED`, key).Scan(&lockedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM blob_ledger WHERE blob_key = $1`, lockedKey); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func blobFileExists(store *blobs.Store, key string) (bool, error) {
	_, err := os.Stat(filepath.Join(store.Dir, filepath.FromSlash(key)))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range []byte(value) {
		switch i {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
				return false
			}
		}
	}
	return true
}
