package blobs_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/blobs/blobstest"
	"uuid"
)

// TestStoreContract is what the server relies on from every backend.
func TestStoreContract(t *testing.T) {
	for _, backend := range blobstest.Backends(t) {
		t.Run(backend.Name, func(t *testing.T) {
			ctx := context.Background()
			store := backend.Store
			key := uuid.NewV7().String() + "/" + uuid.NewV7().String()

			if err := store.Put(ctx, key, []byte("ciphertext")); err != nil {
				t.Fatalf("Put: %v", err)
			}
			body, size, err := store.Open(ctx, key)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			got, err := io.ReadAll(body)
			body.Close()
			if err != nil || string(got) != "ciphertext" || size != int64(len("ciphertext")) {
				t.Fatalf("Open = %q, size %d, %v", got, size, err)
			}

			if err := store.Remove(ctx, key); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			if _, _, err := store.Open(ctx, key); !errors.Is(err, blobs.ErrNotFound) {
				t.Fatalf("Open after Remove = %v, want ErrNotFound", err)
			}
			if err := store.Remove(ctx, key); err != nil {
				t.Fatalf("Remove of a missing blob = %v, want success", err)
			}

			empty := uuid.NewV7().String() + "/" + uuid.NewV7().String()
			if err := store.Put(ctx, empty, nil); err != nil {
				t.Fatalf("Put empty: %v", err)
			}
			body, size, err = store.Open(ctx, empty)
			if err != nil || size != 0 {
				t.Fatalf("Open empty = size %d, %v", size, err)
			}
			body.Close()
			if err := store.Remove(ctx, empty); err != nil {
				t.Fatalf("Remove empty: %v", err)
			}
		})
	}
}
