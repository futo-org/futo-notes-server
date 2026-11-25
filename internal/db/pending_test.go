package db

import (
	"path/filepath"
	"testing"
)

func TestPendingReportsWithoutApplying(t *testing.T) {
	database, err := Open("sqlite:"+filepath.Join(t.TempDir(), "notes.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := t.Context()

	pending, err := Pending(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	all, err := loadMigrations(database.Dialect())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != len(all) {
		t.Fatalf("Pending on a fresh database = %d migrations, want all %d", len(pending), len(all))
	}
	again, err := Pending(ctx, database)
	if err != nil || len(again) != len(all) {
		t.Fatalf("second Pending = %v, %v; want it unchanged", again, err)
	}

	if _, err := Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	if pending, err := Pending(ctx, database); err != nil || len(pending) != 0 {
		t.Fatalf("Pending after Migrate = %v, %v; want none", pending, err)
	}
}
