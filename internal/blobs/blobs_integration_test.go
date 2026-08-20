package blobs_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"futo-notes-server/internal/blobs"
	databasepkg "futo-notes-server/internal/db"
	"uuid"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestBlobDeleteLifecycle(t *testing.T) {
	databaseURL := os.Getenv("BLOBS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BLOBS_TEST_DATABASE_URL is not set")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if _, err := databasepkg.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}

	userID := uuid.NewV7().String()
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'blobs test', $3)`, userID, "blobs-test-"+userID, userID+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	defer database.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)

	store := &blobs.Store{Dir: t.TempDir()}
	onDisk := func(key string) bool {
		_, err := os.Stat(filepath.Join(store.Dir, filepath.FromSlash(key)))
		return err == nil
	}

	staged, err := blobs.Stage(ctx, database, store, userID, []byte("staged"))
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := blobs.Delete(ctx, database, store, userID, staged)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted || onDisk(staged) {
		t.Fatalf("staged blob survived: deleted=%v onDisk=%v", deleted, onDisk(staged))
	}
	var rows int
	if err := database.QueryRowContext(ctx,
		`SELECT count(*) FROM blob_ledger WHERE blob_key = $1`, staged).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("ledger row survived: %d", rows)
	}

	// Deleting the same key again, and a key that was never staged, are both
	// no-ops rather than errors.
	for _, key := range []string{staged, userID + "/" + uuid.NewV7().String()} {
		deleted, err := blobs.Delete(ctx, database, store, userID, key)
		if err != nil || !deleted {
			t.Fatalf("absent delete: deleted=%v err=%v", deleted, err)
		}
	}

	// A file with no ledger row is a legacy or reconciled upload; it goes.
	legacy := userID + "/" + uuid.NewV7().String()
	if err := store.Put(legacy, []byte("legacy")); err != nil {
		t.Fatal(err)
	}
	if deleted, err := blobs.Delete(ctx, database, store, userID, legacy); err != nil || !deleted {
		t.Fatalf("legacy delete: deleted=%v err=%v", deleted, err)
	}
	if onDisk(legacy) {
		t.Fatal("legacy file survived")
	}

	for _, state := range []string{"claimed", "retained", "purgeable", "legacy_shared"} {
		key, err := blobs.Stage(ctx, database, store, userID, []byte(state))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.ExecContext(ctx,
			`UPDATE blob_ledger SET state = $2 WHERE blob_key = $1`, key, state); err != nil {
			t.Fatal(err)
		}
		deleted, err := blobs.Delete(ctx, database, store, userID, key)
		if err != nil {
			t.Fatal(err)
		}
		if deleted || !onDisk(key) {
			t.Fatalf("%s blob was deletable", state)
		}
	}

	// Migration 010 records a key shared by several object rows under the
	// lowest user id it saw, which need not be the user whose namespace the
	// key lives in. The row still has to block the delete.
	shared := userID + "/" + uuid.NewV7().String()
	if err := store.Put(shared, []byte("shared")); err != nil {
		t.Fatal(err)
	}
	foreignOwner := uuid.NewV7().String()
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'blobs test shared', $3)`, foreignOwner, "blobs-test-"+foreignOwner,
		foreignOwner+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	defer database.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, foreignOwner)
	if _, err := database.ExecContext(ctx, `INSERT INTO blob_ledger
		(blob_key, user_id, size_bytes, state) VALUES ($1, $2, 6, 'legacy_shared')`,
		shared, foreignOwner); err != nil {
		t.Fatal(err)
	}
	deleted, err = blobs.Delete(ctx, database, store, userID, shared)
	if err != nil {
		t.Fatal(err)
	}
	if deleted || !onDisk(shared) {
		t.Fatal("legacy_shared blob recorded against another user was deletable")
	}

	// Another user's ledger row is not reachable, even by exact key.
	otherID := uuid.NewV7().String()
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'blobs test other', $3)`, otherID, "blobs-test-"+otherID, otherID+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	defer database.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, otherID)
	otherKey, err := blobs.Stage(ctx, database, store, otherID, []byte("theirs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Delete(ctx, database, store, userID, otherKey); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx,
		`SELECT count(*) FROM blob_ledger WHERE blob_key = $1`, otherKey).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatal("another user's ledger row was removed")
	}
	if !onDisk(otherKey) {
		t.Fatal("another user's file was removed")
	}
}
