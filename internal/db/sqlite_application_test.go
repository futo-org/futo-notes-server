package db_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/collections"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/jobs"
	"futo-notes-server/internal/objects"
)

func TestSQLiteApplicationLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := &blobs.Store{Dir: filepath.Join(root, "blobs")}
	database, err := db.Open("sqlite:"+filepath.Join(root, "db", "notes.db"), db.Options{BlobDir: store.Dir})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	applied, err := db.Migrate(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != "001_baseline" {
		t.Fatalf("applied migrations = %v", applied)
	}
	if again, err := db.Migrate(ctx, database); err != nil || len(again) != 0 {
		t.Fatalf("second migration run = %v, %v", again, err)
	}
	assertSQLitePragmas(t, database)
	assertSQLiteImmediateTransactions(t, database, filepath.Join(root, "db", "notes.db"))

	user, err := auth.UpsertUserByEmail(ctx, database, "sqlite@example.invalid", "SQLite")
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.CreateSession(ctx, database, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session, err := auth.ValidateSession(ctx, database, token); err != nil || session.User.ID != user.ID {
		t.Fatalf("ValidateSession() = %#v, %v", session, err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE sessions SET expires_at = $1`, db.Timestamp(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if reaped, err := jobs.ReapSessions(ctx, database); err != nil || reaped.Reaped != 1 {
		t.Fatalf("ReapSessions() = %#v, %v", reaped, err)
	}

	collection, created, err := collections.Claim(ctx, database, user.ID)
	if err != nil || !created {
		t.Fatalf("Claim() = %#v, %t, %v", collection, created, err)
	}
	var storedCreatedAt string
	if err := database.QueryRowContext(ctx, `SELECT created_at FROM collections WHERE id = $1`, collection.ID).Scan(&storedCreatedAt); err != nil {
		t.Fatal(err)
	}
	if len(storedCreatedAt) != len("2006-01-02T15:04:05.000Z") || storedCreatedAt[len(storedCreatedAt)-1] != 'Z' {
		t.Fatalf("SQLite timestamp = %q, want fixed-width UTC milliseconds", storedCreatedAt)
	}
	if _, err := collections.PutKey(ctx, database, user.ID, collection.ID, collections.KeyInput{
		KeySalt: "salt", KeyKDF: []byte(`{"name":"scrypt"}`), EncryptedVaultKey: "wrapped",
	}, nil); err != nil {
		t.Fatalf("PutKey() = %v", err)
	}
	if material, claimed, err := collections.GetKey(ctx, database, user.ID, collection.ID); err != nil || !claimed || material.KeySalt != "salt" {
		t.Fatalf("GetKey() = %#v, %t, %v", material, claimed, err)
	}

	reconciledKey := user.ID + "/00000000-0000-7000-8000-000000000099"
	if err := store.Put(reconciledKey, []byte("reconciled")); err != nil {
		t.Fatal(err)
	}
	if result, err := jobs.ReconcileStorage(ctx, database, store); err != nil || result.Adopted != 1 {
		t.Fatalf("ReconcileStorage() = %#v, %v", result, err)
	}

	hub := events.NewHub()
	publisher := events.NewPublisher(database.Dialect(), hub)
	subscription := hub.Subscribe(user.ID)
	defer hub.Unsubscribe(subscription)
	rolledBack, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(ctx, rolledBack, user.ID, collection.ID, 99); err != nil {
		t.Fatal(err)
	}
	if err := rolledBack.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-subscription.Wake():
		t.Fatal("SQLite publisher delivered a rolled-back notification")
	default:
	}
	firstBlob, err := blobs.Stage(ctx, database, store, user.ID, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	objectStore := objects.New(database, publisher)
	createdObject, err := objectStore.Create(ctx,
		objects.Mutation{UserID: user.ID, CollectionID: collection.ID, MutationID: "create-sqlite"}, firstBlob)
	if err != nil {
		t.Fatalf("Create() = %#v, %v", createdObject, err)
	}
	select {
	case <-subscription.Wake():
	case <-time.After(time.Second):
		t.Fatal("SQLite publisher did not flush after commit")
	}
	changes, open := subscription.Drain()
	if !open || len(changes) != 1 || changes[0].CurrentVersion != 1 {
		t.Fatalf("published changes = %#v, open %t", changes, open)
	}

	secondBlob, err := blobs.Stage(ctx, database, store, user.ID, []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := objectStore.Update(ctx, objects.Mutation{UserID: user.ID, CollectionID: collection.ID,
		ObjectID: createdObject.Object.ID, MutationID: "update-sqlite"}, secondBlob, 2)
	if err != nil {
		t.Fatalf("Update() = %#v, %v", updated, err)
	}
	deleted, err := objectStore.Delete(ctx, objects.Mutation{UserID: user.ID, CollectionID: collection.ID,
		ObjectID: createdObject.Object.ID, MutationID: "delete-sqlite"}, nil)
	if err != nil {
		t.Fatalf("Delete() = %#v, %v", deleted, err)
	}

	if result, err := jobs.ExpireMutationResults(ctx, database); err != nil {
		t.Fatalf("ExpireMutationResults() = %#v, %v", result, err)
	}
	if err := collections.Delete(ctx, database, user.ID, collection.ID); err != nil {
		t.Fatalf("collections.Delete() = %v", err)
	}
	var objectCount int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM objects WHERE collection_id = $1`, collection.ID).Scan(&objectCount); err != nil {
		t.Fatal(err)
	}
	if objectCount != 0 {
		t.Fatalf("foreign-key cascade left %d objects", objectCount)
	}
	gc, err := jobs.GarbageCollectBlobs(ctx, database, store)
	if err != nil || gc.RowsPurged == 0 || gc.FilesRemoved == 0 {
		t.Fatalf("GarbageCollectBlobs() = %#v, %v", gc, err)
	}

	if _, err := os.Stat(filepath.Join(root, "db", "notes.db")); err != nil {
		t.Fatalf("SQLite database file: %v", err)
	}
}

func TestSQLiteConcurrentMigration(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open("sqlite:"+filepath.Join(root, "notes.db"), db.Options{BlobDir: filepath.Join(root, "blobs")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	type result struct {
		applied []string
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			applied, err := db.Migrate(context.Background(), database)
			results <- result{applied: applied, err: err}
		}()
	}
	close(start)
	totalApplied := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		totalApplied += len(result.applied)
	}
	if totalApplied != 1 {
		t.Fatalf("concurrent migration runs applied %d migrations, want 1", totalApplied)
	}
}

func assertSQLitePragmas(t *testing.T, database *db.DB) {
	t.Helper()
	var journalMode string
	if err := database.QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&journalMode); err != nil || journalMode != "wal" {
		t.Fatalf("journal_mode = %q, %v", journalMode, err)
	}
	var foreignKeys int
	if err := database.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, %v", foreignKeys, err)
	}
	var busyTimeout int
	if err := database.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil || busyTimeout != 10000 {
		t.Fatalf("busy_timeout = %d, %v", busyTimeout, err)
	}
	var synchronous int
	if err := database.QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 1 {
		t.Fatalf("synchronous = %d, %v", synchronous, err)
	}
	var integrity string
	if err := database.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
}

func assertSQLiteImmediateTransactions(t *testing.T, database *db.DB, path string) {
	t.Helper()
	writer, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Discard()

	competitor, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer competitor.Close()
	second, err := competitor.BeginTx(context.Background(), nil)
	if err == nil {
		_ = second.Rollback()
		t.Fatal("second immediate transaction acquired the SQLite write lock")
	}
}
