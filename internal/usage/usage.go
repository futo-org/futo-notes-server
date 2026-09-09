// Package usage owns the Plan an account is on and its Storage Usage: the
// account's claimed and retained blob bytes.
//
// It is the one seam between the blob ledger and everything that cares how
// much room an account has left. Quota checks and the billing route ask this
// package; they never read objects or blob_ledger themselves. The counter is
// moved inside the same transactions that move blob-ledger states, so a
// mutation and the bytes it accounts for commit or roll back together, and
// nothing sums the ledger on the upload path.
package usage

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"futo-notes-server/internal/db"
)

// Plan is the named set of limits an account is entitled to. The JSON tags
// are the wire names the billing route serves.
type Plan struct {
	StorageQuotaBytes int64 `json:"storage_quota_bytes"`
	BlobMaxBytes      int64 `json:"blob_max_bytes"`
}

// Account is one account's plan limits and its current Storage Usage.
type Account struct {
	Plan      Plan
	BytesUsed int64
}

// Exceeds reports whether storing incoming more bytes would put the account
// past its Storage Quota. Filling the quota exactly is allowed.
func (a Account) Exceeds(incoming int64) bool {
	return a.BytesUsed+incoming > a.Plan.StorageQuotaBytes
}

// selectAccount reads one account's plan limits beside its counter. An account
// that has never stored anything has no counter row yet and reads as zero.
const selectAccount = `SELECT plans.storage_quota_bytes, plans.blob_max_bytes,
	COALESCE(user_usage.bytes_used, 0)
	FROM users
	JOIN plans ON plans.id = users.plan_id
	LEFT JOIN user_usage ON user_usage.user_id = users.id
	WHERE users.id = $1`

type accountRow interface{ Scan(dest ...any) error }

func scanAccount(row accountRow) (Account, error) {
	var account Account
	err := row.Scan(&account.Plan.StorageQuotaBytes, &account.Plan.BlobMaxBytes, &account.BytesUsed)
	return account, err
}

// Of returns the account's plan limits and Storage Usage.
func Of(ctx context.Context, database *db.DB, userID string) (Account, error) {
	return scanAccount(database.QueryRowContext(ctx, selectAccount, userID))
}

// Fits reports whether the account has room for incoming more bytes, reading
// the counter inside the caller's transaction — the same transaction that
// claims the blob and calls Add. That is what makes the quota enforceable: a
// check taken before the transaction can be walked past by staging bytes
// first, because staged bytes do not count until something claims them.
func Fits(ctx context.Context, tx *db.Tx, userID string, incoming int64) (Account, bool, error) {
	account, err := scanAccount(tx.QueryRowContext(ctx, selectAccount, userID))
	if err != nil {
		return Account{}, false, err
	}
	return account, !account.Exceeds(incoming), nil
}

// Add moves the account's counter by delta inside the caller's transaction,
// which is the transaction that moved the blob-ledger row. It creates the
// counter row on first use, so account creation has nothing to remember.
func Add(ctx context.Context, tx *db.Tx, userID string, delta int64) error {
	if delta == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO user_usage (user_id, bytes_used) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET bytes_used = user_usage.bytes_used + EXCLUDED.bytes_used`,
		userID, delta)
	return err
}

// ReconcileResult reports how many accounts a reconciliation pass recounted
// and how many of their counters it had to correct.
type ReconcileResult struct {
	Checked   int `json:"checked"`
	Corrected int `json:"corrected"`
}

// Reconcile recomputes every account's Storage Usage from the blob ledger and
// writes back the ones that have drifted, logging each correction. Drift is a
// bug wherever it comes from, so the log line carries both numbers.
func Reconcile(ctx context.Context, database *db.DB) (ReconcileResult, error) {
	var result ReconcileResult
	ids, err := accountIDs(ctx, database)
	if err != nil {
		return result, err
	}
	for _, id := range ids {
		corrected, err := reconcileAccount(ctx, database, id)
		if err != nil {
			return result, err
		}
		result.Checked++
		if corrected {
			result.Corrected++
		}
	}
	return result, nil
}

func accountIDs(ctx context.Context, database *db.DB) ([]string, error) {
	rows, err := database.QueryContext(ctx, `SELECT id FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// reconcileAccount recounts one account and reports whether it corrected the
// counter. The counter row is locked before the ledger is summed, so a
// mutation that moves it waits for this recount instead of landing between
// the sum and the correction and being overwritten.
//
// An account that has never stored anything has no row to lock yet, so its
// very first claim can race a pass and be overwritten. That leaves the
// counter low by one blob until the next pass, which is the same drift this
// job exists to correct.
func reconcileAccount(ctx context.Context, database *db.DB, userID string) (bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var counted int64
	err = tx.QueryRowContext(ctx,
		`SELECT bytes_used FROM user_usage WHERE user_id = $1`+database.Dialect().ForUpdate(), userID).Scan(&counted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	var actual int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM blob_ledger
		WHERE user_id = $1 AND state IN ('claimed', 'retained')`, userID).Scan(&actual); err != nil {
		return false, err
	}
	if counted == actual {
		return false, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_usage (user_id, bytes_used) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET bytes_used = EXCLUDED.bytes_used`, userID, actual); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	slog.Info("usage reconciliation: corrected Storage Usage",
		"user_id", userID, "counted", counted, "actual", actual)
	return true, nil
}
