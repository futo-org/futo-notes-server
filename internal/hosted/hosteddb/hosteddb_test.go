package hosteddb_test

import (
	"testing"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/hosted/hosteddb"
	"futo-notes-server/internal/hosted/hostedtest"
)

func TestMigrateIsIdempotentAndLeavesNothingPending(t *testing.T) {
	database := hostedtest.Postgres(t)
	ctx := t.Context()

	applied, err := hosteddb.Migrate(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second Migrate applied %v, want nothing", applied)
	}
	pending, err := hosteddb.Pending(ctx, database)
	if err != nil || len(pending) != 0 {
		t.Fatalf("Pending = %v, %v; want none", pending, err)
	}
	shared, err := db.Pending(ctx, database)
	if err != nil || len(shared) != 0 {
		t.Fatalf("db.Pending = %v, %v; want none", shared, err)
	}

	var columns int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'users' AND column_name IN ('region_id', 'deletion_requested_at', 'purge_at')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 3 {
		t.Fatalf("hosted users columns = %d, want 3", columns)
	}
}

func TestMigrateRefusesSQLite(t *testing.T) {
	database, err := db.Open("sqlite:"+t.TempDir()+"/notes.db", db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := hosteddb.Migrate(t.Context(), database); err == nil {
		t.Fatal("Migrate on SQLite succeeded, want an error")
	}
}
