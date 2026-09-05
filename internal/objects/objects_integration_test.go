package objects_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"futo-notes-server/internal/blobs"
	databasepkg "futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/objects"
	"uuid"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestObjectMutationLifecycle(t *testing.T) {
	databaseURL := os.Getenv("OBJECTS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OBJECTS_TEST_DATABASE_URL is not set")
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
	publisher := events.PostgresPublisher{}
	if _, err := databasepkg.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	listener, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(ctx)
	if _, err := listener.Exec(ctx, "LISTEN "+events.Channel); err != nil {
		t.Fatal(err)
	}

	userID, collectionID := uuid.NewV7().String(), uuid.NewV7().String()
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'objects test', $3)`, userID, "objects-test-"+userID, userID+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	defer database.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	if _, err := database.ExecContext(ctx, `INSERT INTO collections (id, user_id) VALUES ($1, $2)`, collectionID, userID); err != nil {
		t.Fatal(err)
	}

	store := &blobs.Store{Dir: t.TempDir()}
	firstKey, err := blobs.Stage(ctx, database, store, userID, []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := objects.Create(ctx, database, publisher, userID, collectionID, "create-1", firstKey)
	if err != nil {
		t.Fatal(err)
	}
	if created.Code != objects.OK || created.Response.Object.Version != "1" || created.Response.CollectionVersion != 1 {
		t.Fatalf("unexpected create: %#v", created)
	}
	waitForObjectNotification(t, listener, userID, collectionID, 1)
	replayed, err := objects.Create(ctx, database, publisher, userID, collectionID, "create-1", firstKey)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Response.Replayed == nil || !*replayed.Response.Replayed || replayed.Response.Object.ID != created.Response.Object.ID {
		t.Fatalf("unexpected replay: %#v", replayed)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)

	secondKey, err := blobs.Stage(ctx, database, store, userID, []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := objects.Update(ctx, database, publisher, userID, collectionID, created.Response.Object.ID, "update-1", secondKey, 2)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Code != objects.OK || updated.Response.Object.Version != "2" || updated.Response.CollectionVersion != 2 {
		t.Fatalf("unexpected update: %#v", updated)
	}
	waitForObjectNotification(t, listener, userID, collectionID, 2)
	conflict, err := objects.Update(ctx, database, publisher, userID, collectionID, created.Response.Object.ID, "update-stale", firstKey, 2)
	if err != nil {
		t.Fatal(err)
	}
	if conflict.Code != objects.VersionConflict || conflict.Conflict.CurrentVersion != 2 {
		t.Fatalf("unexpected conflict: %#v", conflict)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)

	deleted, err := objects.Delete(ctx, database, publisher, userID, collectionID, created.Response.Object.ID, "delete-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Code != objects.OK || deleted.Response.Object.Version != "3" || deleted.Response.CollectionVersion != 3 {
		t.Fatalf("unexpected delete: %#v", deleted)
	}
	waitForObjectNotification(t, listener, userID, collectionID, 3)
	rows, _, err := objects.List(ctx, database, userID, collectionID, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Deleted {
		t.Fatalf("unexpected delta: %#v", rows)
	}
	staleVersion := int64(1)
	staleRedelete, err := objects.Delete(ctx, database, publisher, userID, collectionID, created.Response.Object.ID, "delete-stale", &staleVersion)
	if err != nil {
		t.Fatal(err)
	}
	if staleRedelete.Code != objects.VersionConflict || staleRedelete.Conflict.CurrentVersion != 3 {
		t.Fatalf("unexpected stale tombstone re-delete: %#v", staleRedelete)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)
	tombstoneVersion := int64(3)
	redeleted, err := objects.Delete(ctx, database, publisher, userID, collectionID, created.Response.Object.ID, "delete-2", &tombstoneVersion)
	if err != nil {
		t.Fatal(err)
	}
	if redeleted.Code != objects.OK || redeleted.Response.Object.Version != "3" ||
		redeleted.Response.CollectionVersion != 3 || !redeleted.Response.Object.Deleted {
		t.Fatalf("unexpected tombstone re-delete: %#v", redeleted)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)
	unconditionalRedelete, err := objects.Delete(ctx, database, publisher, userID, collectionID,
		created.Response.Object.ID, "delete-3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if unconditionalRedelete.Code != objects.OK || unconditionalRedelete.Response.Object.Version != "3" ||
		unconditionalRedelete.Response.CollectionVersion != 3 || !unconditionalRedelete.Response.Object.Deleted {
		t.Fatalf("unexpected unconditional tombstone re-delete: %#v", unconditionalRedelete)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)
	thirdKey, err := blobs.Stage(ctx, database, store, userID, []byte("third"))
	if err != nil {
		t.Fatal(err)
	}
	tombstoneUpdate, err := objects.Update(ctx, database, publisher, userID, collectionID, created.Response.Object.ID, "update-2", thirdKey, 4)
	if err != nil {
		t.Fatal(err)
	}
	if tombstoneUpdate.Code != objects.OK || !tombstoneUpdate.Response.Object.Deleted {
		t.Fatalf("update should not revive tombstone: %#v", tombstoneUpdate)
	}
	// The replacement blob is claimed even though the object stays deleted, so
	// a tombstone can hold a claimed blob without any legacy history. The purge
	// job is what eventually releases it.
	var tombstoneBlobState string
	if err := database.QueryRowContext(ctx, `SELECT state FROM blob_ledger WHERE blob_key = $1`,
		thirdKey).Scan(&tombstoneBlobState); err != nil {
		t.Fatal(err)
	}
	if tombstoneBlobState != "claimed" {
		t.Fatalf("blob state after updating a tombstone = %q, want %q", tombstoneBlobState, "claimed")
	}
	var releasedState string
	if err := database.QueryRowContext(ctx, `SELECT state FROM blob_ledger WHERE blob_key = $1`,
		secondKey).Scan(&releasedState); err != nil {
		t.Fatal(err)
	}
	if releasedState != "retained" {
		t.Fatalf("superseded tombstone blob state = %q, want %q", releasedState, "retained")
	}
	waitForObjectNotification(t, listener, userID, collectionID, 4)
	recovered, err := objects.RecoverCreate(ctx, database, userID, collectionID, "create-1")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Code != objects.OK || recovered.Response.Object.ID != created.Response.Object.ID {
		t.Fatalf("unexpected recovery: %#v", recovered)
	}
}

func waitForObjectNotification(t *testing.T, listener *pgx.Conn, userID, collectionID string, version int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		notification, err := listener.WaitForNotification(ctx)
		if err != nil {
			t.Fatalf("waiting for object notification: %v", err)
		}
		var payload events.Notification
		if err := json.Unmarshal([]byte(notification.Payload), &payload); err != nil {
			continue
		}
		if payload.UserID != userID || payload.CollectionID != collectionID {
			continue
		}
		if payload.CurrentVersion != version {
			t.Fatalf("notification version = %d, want %d", payload.CurrentVersion, version)
		}
		return
	}
}

func assertNoObjectNotification(t *testing.T, listener *pgx.Conn, userID, collectionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	for {
		notification, err := listener.WaitForNotification(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if err != nil {
			t.Fatalf("checking for object notification: %v", err)
		}
		var payload events.Notification
		if json.Unmarshal([]byte(notification.Payload), &payload) == nil &&
			payload.UserID == userID && payload.CollectionID == collectionID {
			t.Fatalf("unexpected notification: %#v", payload)
		}
	}
}

// A tombstone's blob must stop being claimed. Claimed blobs are never eligible
// for garbage collection, so leaving one claimed keeps the deleted object's
// ciphertext on disk forever. Retained keeps it fetchable as a merge ancestor
// while putting it on the same 365-day clock an update's old blob gets.
func TestDeleteRetainsBlobForReclamation(t *testing.T) {
	databaseURL := os.Getenv("OBJECTS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("OBJECTS_TEST_DATABASE_URL is not set")
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
	publisher := events.PostgresPublisher{}
	if _, err := databasepkg.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}

	userID, collectionID := uuid.NewV7().String(), uuid.NewV7().String()
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'delete blob test', $3)`, userID, "delete-blob-"+userID, userID+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	defer database.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	if _, err := database.ExecContext(ctx, `INSERT INTO collections (id, user_id) VALUES ($1, $2)`, collectionID, userID); err != nil {
		t.Fatal(err)
	}

	store := &blobs.Store{Dir: t.TempDir()}
	key, err := blobs.Stage(ctx, database, store, userID, []byte("folder ciphertext"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := objects.Create(ctx, database, publisher, userID, collectionID, "create-folder", key)
	if err != nil {
		t.Fatal(err)
	}
	if created.Code != objects.OK {
		t.Fatalf("unexpected create: %#v", created)
	}
	deleted, err := objects.Delete(ctx, database, publisher, userID, collectionID, created.Response.Object.ID, "delete-folder", nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Code != objects.OK {
		t.Fatalf("unexpected delete: %#v", deleted)
	}

	var state string
	var objectID sql.NullString
	var objectVersion sql.NullInt64
	if err := database.QueryRowContext(ctx, `SELECT state, object_id, object_version
		FROM blob_ledger WHERE blob_key = $1`, key).Scan(&state, &objectID, &objectVersion); err != nil {
		t.Fatal(err)
	}
	if state != "retained" {
		t.Errorf("ledger state = %q, want %q: a claimed blob is never garbage collected", state, "retained")
	}
	if objectID.Valid || objectVersion.Valid {
		t.Errorf("ledger still points at object %v version %v, want both NULL", objectID, objectVersion)
	}

	// Still readable: the tombstone's blob may be a merge ancestor.
	file, err := store.Open(key)
	if err != nil {
		t.Fatalf("tombstone blob is not fetchable: %v", err)
	}
	file.Close()
}
