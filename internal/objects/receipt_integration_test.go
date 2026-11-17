package objects_test

import (
	"errors"
	"sync"
	"testing"

	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/objects"
	"uuid"
)

func TestReceiptBlobsAreClaimedOnceAndDirectly(t *testing.T) {
	database := dbtest.Postgres(t, "OBJECTS_TEST_DATABASE_URL")
	ctx := t.Context()
	store := objects.New(database, events.PostgresPublisher{})

	userID, collectionID := uuid.NewV7().String(), uuid.NewV7().String()
	if _, err := database.ExecContext(ctx, `INSERT INTO users (id, sub, name, email)
		VALUES ($1, $2, 'receipt test', $3)`, userID, "receipt-test-"+userID, userID+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	dbtest.CleanupExec(t, database, `DELETE FROM users WHERE id = $1`, userID)
	if _, err := database.ExecContext(ctx, `INSERT INTO collections (id, user_id) VALUES ($1, $2)`, collectionID, userID); err != nil {
		t.Fatal(err)
	}
	ledger := func(key string) (state string, size int64, objectID *string) {
		t.Helper()
		if err := database.QueryRowContext(ctx, `SELECT state, size_bytes, object_id FROM blob_ledger WHERE blob_key = $1`, key).
			Scan(&state, &size, &objectID); err != nil {
			t.Fatalf("ledger row for %s: %v", key, err)
		}
		return state, size, objectID
	}
	mutation := func(objectID, mutationID string) objects.Mutation {
		return objects.Mutation{UserID: userID, CollectionID: collectionID, ObjectID: objectID, MutationID: mutationID}
	}

	firstKey := userID + "/" + uuid.NewV7().String()
	created, err := store.Create(ctx, mutation("", "receipt-create"), objects.ReceiptBlob(firstKey, 1234))
	if err != nil {
		t.Fatal(err)
	}
	if created.Object.SizeBytes == nil || *created.Object.SizeBytes != 1234 || *created.Object.BlobKey != firstKey {
		t.Fatalf("created = %#v", created.Object)
	}
	if state, size, objectID := ledger(firstKey); state != "claimed" || size != 1234 || objectID == nil || *objectID != created.Object.ID {
		t.Fatalf("ledger = %s %d %v", state, size, objectID)
	}

	replayed, err := store.Create(ctx, mutation("", "receipt-create"), objects.ReceiptBlob(firstKey, 1234))
	if err != nil || replayed.Replayed == nil || !*replayed.Replayed || replayed.Object.ID != created.Object.ID {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}

	_, err = store.Create(ctx, mutation("", "receipt-reuse"), objects.ReceiptBlob(firstKey, 1234))
	if !errors.Is(err, objects.ErrReceiptUsed) {
		t.Fatalf("reused receipt err = %v, want ErrReceiptUsed", err)
	}
	_, err = store.Create(ctx, mutation("", "receipt-reuse"), objects.ReceiptBlob(userID+"/"+uuid.NewV7().String(), 1))
	if !errors.Is(err, objects.ErrReceiptUsed) {
		t.Fatalf("retry of a failed create = %v, want the stored ErrReceiptUsed", err)
	}

	secondKey := userID + "/" + uuid.NewV7().String()
	updated, err := store.Update(ctx, mutation(created.Object.ID, "receipt-update"), objects.ReceiptBlob(secondKey, 99), 2)
	if err != nil {
		t.Fatal(err)
	}
	if *updated.Object.SizeBytes != 99 || *updated.Object.BlobKey != secondKey {
		t.Fatalf("updated = %#v", updated.Object)
	}
	if state, _, _ := ledger(firstKey); state != "retained" {
		t.Fatalf("superseded blob state = %s, want retained", state)
	}

	// Two creates racing on one receipt: exactly one claims it.
	racedKey := userID + "/" + uuid.NewV7().String()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = store.Create(ctx, mutation("", "receipt-race-"+string(rune('a'+i))), objects.ReceiptBlob(racedKey, 5))
		})
	}
	wg.Wait()
	used := 0
	for _, err := range errs {
		switch {
		case errors.Is(err, objects.ErrReceiptUsed):
			used++
		case err != nil:
			t.Fatalf("raced create: %v", err)
		}
	}
	if used != 1 {
		t.Fatalf("raced creates = %v, want exactly one ErrReceiptUsed", errs)
	}
}
