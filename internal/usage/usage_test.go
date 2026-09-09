package usage_test

import (
	"context"
	"path/filepath"
	"testing"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/usage"
)

// newDatabase is a migrated SQLite database in a temp directory, so these
// tests run without Postgres.
func newDatabase(t *testing.T) *db.DB {
	t.Helper()
	root := t.TempDir()
	database, err := db.Open(config.Config{
		DatabaseURL: "sqlite:" + filepath.Join(root, "notes.db"),
		BlobDir:     filepath.Join(root, "blobs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	return database
}

func newUser(t *testing.T, database *db.DB, email string) string {
	t.Helper()
	user, err := auth.UpsertUserByEmail(context.Background(), database, email, "Usage Test")
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

// stageLedgerRow writes a blob_ledger row directly, standing in for the
// transitions the sync layer drives.
func stageLedgerRow(t *testing.T, database *db.DB, userID, key, state string, size int64) {
	t.Helper()
	_, err := database.ExecContext(context.Background(),
		`INSERT INTO blob_ledger (blob_key, user_id, size_bytes, state, created_at, state_changed_at)
		 VALUES ($1, $2, $3, $4, '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z')`,
		key, userID, size, state)
	if err != nil {
		t.Fatal(err)
	}
}

func TestOfReturnsTheDefaultPlanAndZeroUsage(t *testing.T) {
	ctx := context.Background()
	database := newDatabase(t)
	userID := newUser(t, database, "fresh@example.invalid")

	account, err := usage.Of(ctx, database, userID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Plan.StorageQuotaBytes != 10000000000 {
		t.Errorf("StorageQuotaBytes = %d, want 10000000000", account.Plan.StorageQuotaBytes)
	}
	if account.Plan.BlobMaxBytes != 104857600 {
		t.Errorf("BlobMaxBytes = %d, want 104857600", account.Plan.BlobMaxBytes)
	}
	if account.BytesUsed != 0 {
		t.Errorf("BytesUsed = %d, want 0", account.BytesUsed)
	}
}

func TestAddAccumulatesFromNothingAndSubtracts(t *testing.T) {
	ctx := context.Background()
	database := newDatabase(t)
	userID := newUser(t, database, "counter@example.invalid")

	for _, delta := range []int64{500, 250, -300} {
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := usage.Add(ctx, tx, userID, delta); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	account, err := usage.Of(ctx, database, userID)
	if err != nil {
		t.Fatal(err)
	}
	if account.BytesUsed != 450 {
		t.Fatalf("BytesUsed = %d, want 450", account.BytesUsed)
	}
}

func TestExceedsComparesAgainstTheStorageQuota(t *testing.T) {
	account := usage.Account{Plan: usage.Plan{StorageQuotaBytes: 1000}, BytesUsed: 900}
	if account.Exceeds(100) {
		t.Error("Exceeds(100) = true; filling the quota exactly should be allowed")
	}
	if !account.Exceeds(101) {
		t.Error("Exceeds(101) = false; one byte past the quota should be refused")
	}
}

func TestReconcileCorrectsDriftFromTheLedger(t *testing.T) {
	ctx := context.Background()
	database := newDatabase(t)
	drifted := newUser(t, database, "drifted@example.invalid")
	accurate := newUser(t, database, "accurate@example.invalid")

	// Claimed and retained count; staged and purgeable do not.
	stageLedgerRow(t, database, drifted, drifted+"/a", "claimed", 700)
	stageLedgerRow(t, database, drifted, drifted+"/b", "retained", 300)
	stageLedgerRow(t, database, drifted, drifted+"/c", "staged", 5000)
	stageLedgerRow(t, database, drifted, drifted+"/d", "purgeable", 9000)
	if _, err := database.ExecContext(ctx,
		`INSERT INTO user_usage (user_id, bytes_used) VALUES ($1, 42)`, drifted); err != nil {
		t.Fatal(err)
	}
	stageLedgerRow(t, database, accurate, accurate+"/a", "claimed", 10)
	if _, err := database.ExecContext(ctx,
		`INSERT INTO user_usage (user_id, bytes_used) VALUES ($1, 10)`, accurate); err != nil {
		t.Fatal(err)
	}

	result, err := usage.Reconcile(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if result.Checked != 2 {
		t.Errorf("Checked = %d, want 2", result.Checked)
	}
	if result.Corrected != 1 {
		t.Errorf("Corrected = %d, want 1", result.Corrected)
	}

	account, err := usage.Of(ctx, database, drifted)
	if err != nil {
		t.Fatal(err)
	}
	if account.BytesUsed != 1000 {
		t.Fatalf("BytesUsed after reconcile = %d, want 1000", account.BytesUsed)
	}
}
