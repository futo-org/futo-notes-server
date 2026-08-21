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
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
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
	created, err := objects.Create(ctx, database, userID, collectionID, "create-1", firstKey)
	if err != nil {
		t.Fatal(err)
	}
	if created.Code != objects.OK || created.Response.Object.Version != "1" || created.Response.CollectionVersion != 1 {
		t.Fatalf("unexpected create: %#v", created)
	}
	waitForObjectNotification(t, listener, userID, collectionID, 1)
	replayed, err := objects.Create(ctx, database, userID, collectionID, "create-1", firstKey)
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
	updated, err := objects.Update(ctx, database, userID, collectionID, created.Response.Object.ID, "update-1", secondKey, 2)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Code != objects.OK || updated.Response.Object.Version != "2" || updated.Response.CollectionVersion != 2 {
		t.Fatalf("unexpected update: %#v", updated)
	}
	waitForObjectNotification(t, listener, userID, collectionID, 2)
	conflict, err := objects.Update(ctx, database, userID, collectionID, created.Response.Object.ID, "update-stale", firstKey, 2)
	if err != nil {
		t.Fatal(err)
	}
	if conflict.Code != objects.VersionConflict || conflict.Conflict.CurrentVersion != 2 {
		t.Fatalf("unexpected conflict: %#v", conflict)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)

	deleted, err := objects.Delete(ctx, database, userID, collectionID, created.Response.Object.ID, "delete-1", nil)
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
	redeleted, err := objects.Delete(ctx, database, userID, collectionID, created.Response.Object.ID, "delete-2", &staleVersion)
	if err != nil {
		t.Fatal(err)
	}
	if redeleted.Code != objects.OK || redeleted.Response.Object.Version != "3" ||
		redeleted.Response.CollectionVersion != 3 || !redeleted.Response.Object.Deleted {
		t.Fatalf("unexpected tombstone re-delete: %#v", redeleted)
	}
	assertNoObjectNotification(t, listener, userID, collectionID)
	unconditionalRedelete, err := objects.Delete(ctx, database, userID, collectionID,
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
	tombstoneUpdate, err := objects.Update(ctx, database, userID, collectionID, created.Response.Object.ID, "update-2", thirdKey, 4)
	if err != nil {
		t.Fatal(err)
	}
	if tombstoneUpdate.Code != objects.OK || !tombstoneUpdate.Response.Object.Deleted {
		t.Fatalf("update should not revive tombstone: %#v", tombstoneUpdate)
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
