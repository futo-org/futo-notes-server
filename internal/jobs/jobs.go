// Package jobs owns the server's periodic database and blob-storage cleanup.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/ids"
)

const (
	// reconciliationLimit caps how many files one reconciliation pass adopts.
	reconciliationLimit = 500
	// garbageCollectionBatchSize is how many blob keys one GC query selects.
	garbageCollectionBatchSize = 500

	// pendingMutationLifetime is how long a pending (status 102) mutation
	// result is kept.
	pendingMutationLifetime = 24 * time.Hour
	// mutationResultLifetime is how long any other expirable mutation result
	// is kept.
	mutationResultLifetime = 30 * 24 * time.Hour
	// stagedBlobLifetime is how long a blob stays staged before GC removes it.
	stagedBlobLifetime = 24 * time.Hour
	// retainedBlobLifetime is how long a blob stays retained before GC
	// removes it.
	retainedBlobLifetime = 365 * 24 * time.Hour
)

// SessionReapResult reports what ReapSessions deleted.
type SessionReapResult struct {
	Reaped int64 `json:"reaped"`
}

// ReapSessions deletes expired sessions.
func ReapSessions(ctx context.Context, database *db.DB) (SessionReapResult, error) {
	result, err := database.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < $1`, db.Timestamp(time.Now()))
	if err != nil {
		return SessionReapResult{}, err
	}
	reaped, err := result.RowsAffected()
	return SessionReapResult{Reaped: reaped}, err
}

// ReconciliationResult reports what ReconcileStorage adopted and skipped.
// CapHit is set when the pass stopped at its adoption limit.
type ReconciliationResult struct {
	Adopted int  `json:"adopted"`
	Skipped int  `json:"skipped"`
	CapHit  bool `json:"cap_hit"`
}

var errReconciliationCap = errors.New("storage reconciliation cap reached")

// ReconcileStorage adopts files that have no authoritative blob_ledger row.
// A missing storage directory is equivalent to an empty one on a fresh server.
func ReconcileStorage(ctx context.Context, database *db.DB, store *blobs.Store) (ReconciliationResult, error) {
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
		if !ok || !ids.IsUUID(owner) {
			result.Skipped++
			slog.Info("storage reconciliation: skipping file", "key", key, "reason", "invalid owner")
			return nil
		}

		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Discard()
		var ownerExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, owner).Scan(&ownerExists); err != nil {
			return err
		}
		if !ownerExists {
			if err := tx.Commit(); err != nil {
				return err
			}
			result.Skipped++
			slog.Info("storage reconciliation: skipping file", "key", key, "reason", "owner does not exist")
			return nil
		}
		var inserted int
		now := db.Timestamp(time.Now())
		err = tx.QueryRowContext(ctx, `INSERT INTO blob_ledger
			(blob_key, user_id, size_bytes, state, created_at, state_changed_at)
			VALUES ($1, $2, $3, 'staged', $4, $4)
			ON CONFLICT (blob_key) DO NOTHING RETURNING 1`, key, owner, info.Size(), now).Scan(&inserted)
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit()
		}
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		result.Adopted++
		if result.Adopted == reconciliationLimit {
			result.CapHit = true
			return errReconciliationCap
		}
		return nil
	})

	if errors.Is(err, errReconciliationCap) {
		slog.Info("storage reconciliation: adoption cap reached", "limit", reconciliationLimit)
		return result, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	return result, err
}

// MutationExpiryResult reports how many unfinished and finished mutation
// results ExpireMutationResults deleted.
type MutationExpiryResult struct {
	PendingExpired int64 `json:"pending_expired"`
	OtherExpired   int64 `json:"other_expired"`
}

// ExpireMutationResults deletes pending mutation results older than
// pendingMutationLifetime and other results older than mutationResultLifetime.
// Successful and ambiguous legacy create results are never expired.
func ExpireMutationResults(ctx context.Context, database *db.DB) (MutationExpiryResult, error) {
	var result MutationExpiryResult
	now := time.Now()
	pending, err := database.ExecContext(ctx, `
		DELETE FROM mutation_results
		WHERE created_at < $1
		  AND result->>'status' IN ('102', 'pending')`, db.Timestamp(now.Add(-pendingMutationLifetime)))
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
		WHERE created_at < $1
		  AND (result->>'status' IS NULL
		       OR result->>'status' NOT IN ('102', 'pending'))
		  AND NOT (kind = 'create' AND (
			(result->>'status' IS NOT NULL AND result->>'status' LIKE '2%')
			OR (result->>'status' IS NULL
				AND (result->>'error' IS NULL OR result->>'error' NOT IN
					('not found', 'version conflict', 'blob is not staged',
					 'Mutation-Id reused for different intent')))
		  ))`, db.Timestamp(now.Add(-mutationResultLifetime)))
	if err != nil {
		return result, err
	}
	result.OtherExpired, err = other.RowsAffected()
	return result, err
}

// BlobGCResult reports how many ledger rows and files GarbageCollectBlobs
// removed.
type BlobGCResult struct {
	RowsPurged   int `json:"rows_purged"`
	FilesRemoved int `json:"files_removed"`
}

// GarbageCollectBlobs deletes the authoritative row before its file. If the
// file deletion fails, reconciliation will adopt it again for a later pass.
func GarbageCollectBlobs(ctx context.Context, database *db.DB, store *blobs.Store) (BlobGCResult, error) {
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

			existed, err := store.Exists(key)
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

// eligibleBlobKeys returns up to garbageCollectionBatchSize keys whose ledger
// rows may be purged.
func eligibleBlobKeys(ctx context.Context, database *db.DB) ([]string, error) {
	stagedBefore, retainedBefore := blobExpiryCutoffs(time.Now())
	rows, err := database.QueryContext(ctx, `
		SELECT blob_key FROM blob_ledger
		WHERE (state = 'staged' AND state_changed_at < $2)
		   OR (state = 'retained' AND state_changed_at < $3)
		   OR state = 'purgeable'
		ORDER BY blob_key
		LIMIT $1`, garbageCollectionBatchSize, stagedBefore, retainedBefore)
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

// blobExpiryCutoffs returns the timestamps before which staged and retained
// blobs are eligible for garbage collection.
func blobExpiryCutoffs(now time.Time) (stagedBefore, retainedBefore string) {
	return db.Timestamp(now.Add(-stagedBlobLifetime)), db.Timestamp(now.Add(-retainedBlobLifetime))
}

// purgeBlobRow deletes key's ledger row if it is still eligible and not locked
// by another server, and reports whether it did.
func purgeBlobRow(ctx context.Context, database *db.DB, key string) (bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Discard()

	var lockedKey string
	stagedBefore, retainedBefore := blobExpiryCutoffs(time.Now())
	err = tx.QueryRowContext(ctx, `
		SELECT blob_key FROM blob_ledger
		WHERE blob_key = $1 AND (
			(state = 'staged' AND state_changed_at < $2)
			OR (state = 'retained' AND state_changed_at < $3)
			OR state = 'purgeable'
		)
		`+database.Dialect().ForUpdateSkipLocked(), key, stagedBefore, retainedBefore).Scan(&lockedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM blob_ledger WHERE blob_key = $1`, lockedKey); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
