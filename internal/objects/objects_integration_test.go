package objects_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/objects"
	"uuid"

	"github.com/jackc/pgx/v5"
)

func TestObjectMutationLifecycle(t *testing.T) {
	databaseURL := dbtest.PostgresURL(t, "OBJECTS_TEST_DATABASE_URL")
	database := dbtest.Postgres(t, "OBJECTS_TEST_DATABASE_URL")
	ctx := t.Context()
	objectStore := objects.New(database, events.PostgresPublisher{})
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
	dbtest.CleanupExec(t, database, `DELETE FROM users WHERE id = $1`, userID)
	if _, err := database.ExecContext(ctx, `INSERT INTO collections (id, user_id) VALUES ($1, $2)`, collectionID, userID); err != nil {
		t.Fatal(err)
	}

	store := &blobs.Store{Dir: t.TempDir()}
	firstKey, err := blobs.Stage(ctx, database, store, userID, []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	mutation := func(objectID, mutationID string) objects.Mutation {
		return objects.Mutation{UserID: userID, CollectionID: collectionID, ObjectID: objectID, MutationID: mutationID}
	}
	created, err := objectStore.Create(ctx, mutation("", "create-1"), firstKey)
	if err != nil {
		t.Fatal(err)
	}
	if created.Object.Version != 1 || created.CollectionVersion != 1 {
		t.Fatalf("unexpected create: %#v", created)
	}
	objectID := created.Object.ID
	waitForObjectNotification(t, listener, userID, collectionID, 1)
	replayed, err := objectStore.Create(ctx, mutation("", "create-1"), firstKey)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Replayed == nil || !*replayed.Replayed || replayed.Object.ID != objectID {
		t.Fatalf("unexpected replay: %#v", replayed)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)

	secondKey, err := blobs.Stage(ctx, database, store, userID, []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := objectStore.Update(ctx, mutation(objectID, "update-1"), secondKey, 2)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Object.Version != 2 || updated.CollectionVersion != 2 {
		t.Fatalf("unexpected update: %#v", updated)
	}
	waitForObjectNotification(t, listener, userID, collectionID, 2)
	_, err = objectStore.Update(ctx, mutation(objectID, "update-stale"), firstKey, 2)
	if conflict, ok := errors.AsType[*objects.ConflictError](err); !ok || conflict.Conflict.CurrentVersion != 2 {
		t.Fatalf("unexpected conflict: %v", err)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)

	deleted, err := objectStore.Delete(ctx, mutation(objectID, "delete-1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Object.Version != 3 || deleted.CollectionVersion != 3 {
		t.Fatalf("unexpected delete: %#v", deleted)
	}
	waitForObjectNotification(t, listener, userID, collectionID, 3)
	rows, _, err := objectStore.List(ctx, userID, collectionID, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Deleted {
		t.Fatalf("unexpected delta: %#v", rows)
	}
	_, err = objectStore.Delete(ctx, mutation(objectID, "delete-stale"), new(int64(1)))
	if conflict, ok := errors.AsType[*objects.ConflictError](err); !ok || conflict.Conflict.CurrentVersion != 3 {
		t.Fatalf("unexpected stale tombstone re-delete: %v", err)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)
	redeleted, err := objectStore.Delete(ctx, mutation(objectID, "delete-2"), new(int64(3)))
	if err != nil {
		t.Fatal(err)
	}
	if redeleted.Object.Version != 3 || redeleted.CollectionVersion != 3 || !redeleted.Object.Deleted {
		t.Fatalf("unexpected tombstone re-delete: %#v", redeleted)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)
	unconditionalRedelete, err := objectStore.Delete(ctx, mutation(objectID, "delete-3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if unconditionalRedelete.Object.Version != 3 || unconditionalRedelete.CollectionVersion != 3 ||
		!unconditionalRedelete.Object.Deleted {
		t.Fatalf("unexpected unconditional tombstone re-delete: %#v", unconditionalRedelete)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)
	thirdKey, err := blobs.Stage(ctx, database, store, userID, []byte("third"))
	if err != nil {
		t.Fatal(err)
	}
	tombstoneUpdate, err := objectStore.Update(ctx, mutation(objectID, "update-2"), thirdKey, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !tombstoneUpdate.Object.Deleted {
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
	recovered, err := objectStore.RecoverCreate(ctx, userID, collectionID, "create-1")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Object.ID != objectID {
		t.Fatalf("unexpected recovery: %#v", recovered)
	}
}

func waitForObjectNotification(t *testing.T, listener *pgx.Conn, userID, collectionID string, version int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
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
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
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
	database := dbtest.Postgres(t, "OBJECTS_TEST_DATABASE_URL")
	ctx := t.Context()
	objectStore := objects.New(database, events.PostgresPublisher{})

	userID, collectionID := uuid.NewV7().String(), uuid.NewV7().String()
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'delete blob test', $3)`, userID, "delete-blob-"+userID, userID+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	dbtest.CleanupExec(t, database, `DELETE FROM users WHERE id = $1`, userID)
	if _, err := database.ExecContext(ctx, `INSERT INTO collections (id, user_id) VALUES ($1, $2)`, collectionID, userID); err != nil {
		t.Fatal(err)
	}

	store := &blobs.Store{Dir: t.TempDir()}
	key, err := blobs.Stage(ctx, database, store, userID, []byte("folder ciphertext"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := objectStore.Create(ctx, objects.Mutation{UserID: userID, CollectionID: collectionID, MutationID: "create-folder"}, key)
	if err != nil {
		t.Fatalf("unexpected create: %v", err)
	}
	if _, err := objectStore.Delete(ctx, objects.Mutation{UserID: userID, CollectionID: collectionID,
		ObjectID: created.Object.ID, MutationID: "delete-folder"}, nil); err != nil {
		t.Fatalf("unexpected delete: %v", err)
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
