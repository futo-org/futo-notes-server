package usage_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"uuid"

	databasepkg "futo-notes-server/internal/db"
	"futo-notes-server/internal/usage"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestUsageOnPostgres runs the same counter over the engine the hosted
// service deploys on, where the account key is a uuid column rather than
// SQLite's TEXT.
func TestUsageOnPostgres(t *testing.T) {
	databaseURL := os.Getenv("USAGE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("USAGE_TEST_DATABASE_URL is not set")
	}
	rawDatabase, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	dialect, err := databasepkg.ParseDialect(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	database := databasepkg.Wrap(rawDatabase, dialect)
	defer database.Close()
	ctx := context.Background()
	if _, err := databasepkg.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}

	userID := uuid.NewV7().String()
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'usage test', $3)`, userID, "usage-test-"+userID, userID+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	defer database.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)

	account, err := usage.Of(ctx, database, userID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Plan.StorageQuotaBytes != 10000000000 || account.Plan.BlobMaxBytes != 104857600 {
		t.Fatalf("plan = %+v, want the seeded default", account.Plan)
	}
	if account.BytesUsed != 0 {
		t.Fatalf("BytesUsed = %d, want 0", account.BytesUsed)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := usage.Add(ctx, tx, userID, 900); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if account, err = usage.Of(ctx, database, userID); err != nil || account.BytesUsed != 900 {
		t.Fatalf("BytesUsed after Add = %d (%v), want 900", account.BytesUsed, err)
	}

	// Two counting states and two that do not count, then a deliberate drift
	// for reconciliation to find.
	for _, row := range []struct {
		state string
		size  int64
	}{{"claimed", 700}, {"retained", 300}, {"staged", 5000}, {"purgeable", 9000}} {
		if _, err := database.ExecContext(ctx, `INSERT INTO blob_ledger
			(blob_key, user_id, size_bytes, state) VALUES ($1, $2, $3, $4)`,
			userID+"/"+row.state, userID, row.size, row.state); err != nil {
			t.Fatal(err)
		}
	}
	result, err := usage.Reconcile(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if result.Corrected < 1 {
		t.Fatalf("Corrected = %d, want at least this account", result.Corrected)
	}
	if account, err = usage.Of(ctx, database, userID); err != nil || account.BytesUsed != 1000 {
		t.Fatalf("BytesUsed after Reconcile = %d (%v), want 1000", account.BytesUsed, err)
	}
}
