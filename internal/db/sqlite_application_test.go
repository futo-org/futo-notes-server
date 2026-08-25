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
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/jobs"
	"futo-notes-server/internal/objects"
)

func TestSQLiteApplicationLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := &blobs.Store{Dir: filepath.Join(root, "blobs")}
	database, err := db.Open(config.Config{
		DatabaseURL: "sqlite:" + filepath.Join(root, "db", "notes.db"),
		BlobDir:     store.Dir,
	})
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
	if session, err := auth.ValidateSession(ctx, database, token); err != nil || session == nil || session.User.ID != user.ID {
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
	key, _, err := collections.PutKey(ctx, database, user.ID, collection.ID, collections.KeyInput{
		KeySalt: "salt", KeyKDF: []byte(`{"name":"scrypt"}`), EncryptedVaultKey: "wrapped",
	}, nil)
	if err != nil || key != collections.PutKeyOK {
		t.Fatalf("PutKey() = %v, %v", key, err)
	}
	if found, material, err := collections.GetKey(ctx, database, user.ID, collection.ID); err != nil || !found || material == nil || material.KeySalt != "salt" {
		t.Fatalf("GetKey() = %t, %#v, %v", found, material, err)
	}

	reconciledKey := user.ID + "/00000000-0000-7000-8000-000000000099"
	reconciledPath := filepath.Join(store.Dir, filepath.FromSlash(reconciledKey))
	if err := os.MkdirAll(filepath.Dir(reconciledPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reconciledPath, []byte("reconciled"), 0o600); err != nil {
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
	createdObject, err := objects.Create(ctx, database, publisher, user.ID, collection.ID, "create-sqlite", firstBlob)
	if err != nil || createdObject.Code != objects.OK {
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
	updated, err := objects.Update(ctx, database, publisher, user.ID, collection.ID,
		createdObject.Response.Object.ID, "update-sqlite", secondBlob, 2)
	if err != nil || updated.Code != objects.OK {
		t.Fatalf("Update() = %#v, %v", updated, err)
	}
	deleted, err := objects.Delete(ctx, database, publisher, user.ID, collection.ID,
		createdObject.Response.Object.ID, "delete-sqlite", nil)
	if err != nil || deleted.Code != objects.OK {
		t.Fatalf("Delete() = %#v, %v", deleted, err)
	}

	if result, err := jobs.ExpireMutationResults(ctx, database); err != nil {
		t.Fatalf("ExpireMutationResults() = %#v, %v", result, err)
	}
	if removed, err := collections.Delete(ctx, database, user.ID, collection.ID); err != nil || !removed {
		t.Fatalf("collections.Delete() = %t, %v", removed, err)
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
	database, err := db.Open(config.Config{
		DatabaseURL: "sqlite:" + filepath.Join(root, "notes.db"),
		BlobDir:     filepath.Join(root, "blobs"),
	})
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
	if err := database.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil || journalMode != "wal" {
		t.Fatalf("journal_mode = %q, %v", journalMode, err)
	}
	var foreignKeys int
	if err := database.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, %v", foreignKeys, err)
	}
	var busyTimeout int
	if err := database.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil || busyTimeout != 10000 {
		t.Fatalf("busy_timeout = %d, %v", busyTimeout, err)
	}
	var synchronous int
	if err := database.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 1 {
		t.Fatalf("synchronous = %d, %v", synchronous, err)
	}
	var integrity string
	if err := database.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
}

func assertSQLiteImmediateTransactions(t *testing.T, database *db.DB, path string) {
	t.Helper()
	writer, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()

	competitor, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer competitor.Close()
	second, err := competitor.BeginTx(context.Background(), nil)
	if err == nil {
		second.Rollback()
		t.Fatal("second immediate transaction acquired the SQLite write lock")
	}
}
