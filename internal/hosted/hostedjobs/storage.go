package hostedjobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/hosted/storageclient"
	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/ids"
	"futo-notes-server/internal/jobs"
	"futo-notes-server/internal/storagesvc/adminapi"
)

// OrphanMinAge is how old a stored key must be before reconciliation may
// delete it for being unknown to the ledger. Until its receipt expires
// (hostedauth.ReceiptTTL, 24h) a client may still claim a fresh upload, and
// the ledger learns the key only then; twice that leaves a wide margin for a
// slow claim and for clock skew between the API and S3.
const OrphanMinAge = 2 * hostedauth.ReceiptTTL

// gcBatchSize is how many ledger rows one GC transaction removes; one
// storage delete call takes them all.
const gcBatchSize = adminapi.MaxDeleteKeys

// GCResult reports what CollectBlobs did.
type GCResult struct {
	RowsPurged  int
	KeysDeleted int
	KeysFailed  int
}

// CollectBlobs deletes the ledger rows the self-hosted GC would (retained
// longer than jobs.RetainedBlobLifetime, or purgeable; hosted never stages),
// in short transactions, then deletes their keys in each owner's region. The
// row goes first: when the storage delete fails the key is unknown to the
// ledger, and ReconcileOrphans deletes it once it is OrphanMinAge old.
//
// A purgeable row is kept until it was claimed more than
// hostedauth.ReceiptTTL ago. The ledger row is what makes a receipt
// single-use: dropped while the receipt is still valid, the same receipt
// could claim the key again and point a new object at a deleted blob.
func CollectBlobs(ctx context.Context, database *db.DB, storage *storageclient.Client) (GCResult, error) {
	var result GCResult
	for {
		batch, err := purgeLedgerRows(ctx, database)
		if err != nil {
			return result, err
		}
		result.RowsPurged += len(batch)
		byRegion := map[string][]string{}
		var errs []error
		for _, row := range batch {
			if row.region == "" {
				result.KeysFailed++
				errs = append(errs, fmt.Errorf("blob %s belongs to a user with no region", row.key))
				continue
			}
			byRegion[row.region] = append(byRegion[row.region], row.key)
		}
		for region, keys := range byRegion {
			failed, err := storage.DeleteBlobs(ctx, region, keys)
			if err != nil {
				result.KeysFailed += len(keys)
				errs = append(errs, err)
				continue
			}
			result.KeysDeleted += len(keys) - len(failed)
			result.KeysFailed += len(failed)
			if len(failed) > 0 {
				errs = append(errs, fmt.Errorf("storage in %s could not delete %d blobs, first %s: %s",
					region, len(failed), failed[0].Key, failed[0].Message))
			}
		}
		// Stop at the first failure rather than hand the whole backlog to
		// reconciliation while a region is down.
		if len(errs) > 0 || len(batch) < gcBatchSize {
			return result, errors.Join(errs...)
		}
	}
}

type ledgerRow struct {
	key    string
	region string
}

// purgeLedgerRows deletes up to gcBatchSize eligible ledger rows that no
// other transaction holds, and returns their keys with the owner's region.
func purgeLedgerRows(ctx context.Context, database *db.DB) ([]ledgerRow, error) {
	rows, err := database.QueryContext(ctx, `
		WITH doomed AS (
			SELECT blob_key FROM blob_ledger
			WHERE (state = 'retained' AND state_changed_at < $2)
			   OR (state = 'purgeable' AND created_at < $3)
			ORDER BY blob_key
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		), deleted AS (
			DELETE FROM blob_ledger b USING doomed WHERE b.blob_key = doomed.blob_key
			RETURNING b.blob_key, b.user_id
		)
		SELECT deleted.blob_key, coalesce(u.region_id, '') FROM deleted LEFT JOIN users u ON u.id = deleted.user_id`,
		gcBatchSize, time.Now().Add(-jobs.RetainedBlobLifetime), time.Now().Add(-hostedauth.ReceiptTTL))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch []ledgerRow
	for rows.Next() {
		var row ledgerRow
		if err := rows.Scan(&row.key, &row.region); err != nil {
			return nil, err
		}
		batch = append(batch, row)
	}
	return batch, rows.Err()
}

// ReconcileResult reports what ReconcileOrphans did in one region.
type ReconcileResult struct {
	// Listed counts keys older than OrphanMinAge.
	Listed int
	// Skipped counts listed keys that are not blob keys, which storage
	// never writes and refuses to delete.
	Skipped int
	Deleted int
	Failed  int
}

// ReconcileOrphans deletes region's keys under prefix ("" for the whole
// region, or "<userId>/") that are at least OrphanMinAge old at now and
// unknown to the ledger: uploads never claimed, and keys whose storage delete
// failed after GC or account purge forgot them. A key the ledger knows is
// never deleted, whatever its state.
func ReconcileOrphans(ctx context.Context, database *db.DB, storage *storageclient.Client, region, prefix string, now time.Time) (ReconcileResult, error) {
	var result ReconcileResult
	err := storage.ListBlobs(ctx, region, prefix, now.Add(-OrphanMinAge), func(blobs []adminapi.Blob) error {
		result.Listed += len(blobs)
		keys := make([]string, 0, len(blobs))
		for _, b := range blobs {
			if !ids.IsBlobKey(b.Key) {
				result.Skipped++
				log.Warn().Str("region", region).Str("key", b.Key).Msg("orphan reconciliation: skipping a key that is not a blob key")
				continue
			}
			keys = append(keys, b.Key)
		}
		known, err := ledgerKeys(ctx, database, keys)
		if err != nil {
			return err
		}
		orphans := make([]string, 0, len(keys))
		for _, key := range keys {
			if !known[key] {
				orphans = append(orphans, key)
			}
		}
		if len(orphans) == 0 {
			return nil
		}
		failed, err := storage.DeleteBlobs(ctx, region, orphans)
		if err != nil {
			result.Failed += len(orphans)
			return err
		}
		result.Deleted += len(orphans) - len(failed)
		result.Failed += len(failed)
		if len(failed) > 0 {
			return fmt.Errorf("storage in %s could not delete %d orphans, first %s: %s",
				region, len(failed), failed[0].Key, failed[0].Message)
		}
		return nil
	})
	return result, err
}

// ledgerKeys returns which of keys the ledger has a row for.
func ledgerKeys(ctx context.Context, database *db.DB, keys []string) (map[string]bool, error) {
	known := make(map[string]bool, len(keys))
	if len(keys) == 0 {
		return known, nil
	}
	rows, err := database.QueryContext(ctx, `SELECT blob_key FROM blob_ledger WHERE blob_key = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		known[key] = true
	}
	return known, rows.Err()
}

// PurgeResult reports what PurgeAccounts did.
type PurgeResult struct {
	// Due counts accounts whose grace period was over.
	Due          int
	Purged       int
	Failed       int
	BlobsDeleted int
}

// PurgeAccounts purges every account whose deletion grace period is over:
// first the user's prefix in their region (skipped when they never chose
// one), then every row of theirs in one transaction. Both steps are
// idempotent, so an account that fails stays due and the next run finishes
// it.
func PurgeAccounts(ctx context.Context, database *db.DB, storage *storageclient.Client) (PurgeResult, error) {
	var result PurgeResult
	due, err := dueAccounts(ctx, database)
	if err != nil {
		return result, err
	}
	result.Due = len(due)
	var errs []error
	for _, userID := range due {
		blobs, purged, err := purgeAccount(ctx, database, storage, userID)
		result.BlobsDeleted += blobs
		switch {
		case err != nil:
			result.Failed++
			errs = append(errs, fmt.Errorf("purging user %s: %w", userID, err))
		case purged:
			result.Purged++
		}
	}
	return result, errors.Join(errs...)
}

// dueAccounts returns the users whose grace period is over, by the database
// clock that account.Store.Restore also checks.
func dueAccounts(ctx context.Context, database *db.DB) ([]string, error) {
	rows, err := database.QueryContext(ctx, `SELECT id FROM users WHERE purge_at <= clock_timestamp() ORDER BY purge_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var due []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		due = append(due, id)
	}
	return due, rows.Err()
}

// purgeAccount purges one due account and reports how many blobs storage
// deleted and whether the account's rows are gone.
func purgeAccount(ctx context.Context, database *db.DB, storage *storageclient.Client, userID string) (int, bool, error) {
	region, due, err := claimPurge(ctx, database, userID)
	if err != nil || !due {
		return 0, false, err
	}
	blobs := 0
	if region != "" {
		if blobs, err = storage.PurgeUser(ctx, region, userID); err != nil {
			return blobs, false, err
		}
	}
	// Everything the user owns references users with ON DELETE CASCADE:
	// sessions and their storage refresh tokens, collections and their
	// objects, ledger rows, mutation results and one-time codes. This one
	// statement removes them all atomically.
	result, err := database.ExecContext(ctx, `DELETE FROM users WHERE id = $1 AND purge_at IS NOT NULL`, userID)
	if err != nil {
		return blobs, false, err
	}
	n, err := result.RowsAffected()
	return blobs, n == 1, err
}

// claimPurge locks the user's row and reports, by the database clock, whether
// the purge is due, and the user's region ("" if none). Restore updates the
// same row under the same clock, so the two serialize: a restore that won
// leaves purge_at NULL here, and one that comes after this finds the grace
// period over and refuses. Once claimed, an account stays due until purged.
func claimPurge(ctx context.Context, database *db.DB, userID string) (string, bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Discard()
	var region string
	err = tx.QueryRowContext(ctx, `SELECT coalesce(region_id, '') FROM users
		WHERE id = $1 AND purge_at <= clock_timestamp() FOR UPDATE`, userID).Scan(&region)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return region, true, tx.Commit()
}

func blobGC(database *db.DB, storage *storageclient.Client, metrics *Metrics) jobs.Job {
	const name = "blob GC"
	return jobs.Job{Name: name, Run: func(ctx context.Context) (string, error) {
		result, err := CollectBlobs(ctx, database, storage)
		metrics.add(ctx, name, map[string]int{
			itemsProcessed: result.RowsPurged, itemsDeleted: result.KeysDeleted, itemsFailed: result.KeysFailed,
		})
		return fmt.Sprintf("purged %d ledger rows, deleted %d blobs, %d failed",
			result.RowsPurged, result.KeysDeleted, result.KeysFailed), err
	}}
}

func orphanReconciliation(database *db.DB, storage *storageclient.Client, regions []string, metrics *Metrics) jobs.Job {
	const name = "orphan reconciliation"
	return jobs.Job{Name: name, Run: func(ctx context.Context) (string, error) {
		var total ReconcileResult
		var errs []error
		for _, region := range regions {
			result, err := ReconcileOrphans(ctx, database, storage, region, "", time.Now())
			total.Listed += result.Listed
			total.Skipped += result.Skipped
			total.Deleted += result.Deleted
			total.Failed += result.Failed
			if err != nil {
				errs = append(errs, err)
			}
		}
		metrics.add(ctx, name, map[string]int{
			itemsProcessed: total.Listed, itemsDeleted: total.Deleted, itemsFailed: total.Failed,
		})
		return fmt.Sprintf("listed %d old blobs, deleted %d orphans, %d failed, %d skipped",
			total.Listed, total.Deleted, total.Failed, total.Skipped), errors.Join(errs...)
	}}
}

func accountPurge(database *db.DB, storage *storageclient.Client, metrics *Metrics) jobs.Job {
	const name = "account purge"
	return jobs.Job{Name: name, Run: func(ctx context.Context) (string, error) {
		result, err := PurgeAccounts(ctx, database, storage)
		metrics.add(ctx, name, map[string]int{
			itemsProcessed: result.Due, itemsDeleted: result.Purged, itemsFailed: result.Failed,
		})
		return fmt.Sprintf("purged %d of %d due accounts (%d blobs), %d failed",
			result.Purged, result.Due, result.BlobsDeleted, result.Failed), err
	}}
}
