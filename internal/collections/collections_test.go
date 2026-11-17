package collections_test

import (
	"errors"
	"testing"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/collections"
)

// Absent, foreign and malformed ids are all ErrNotFound, so callers answer 404
// without learning whether another account owns the collection.
func TestNotFound(t *testing.T) {
	ctx, database, _, id := keyTestCollection(t)
	other, err := auth.UpsertUserByEmail(ctx, database, "other@example.invalid", "Other")
	if err != nil {
		t.Fatal(err)
	}

	for name, target := range map[string]string{
		"foreign":   id,
		"absent":    "0195e2a1-0000-7000-8000-00000000ffff",
		"malformed": "not-a-uuid",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := collections.Get(ctx, database, other.ID, target); !errors.Is(err, collections.ErrNotFound) {
				t.Errorf("Get() error = %v, want ErrNotFound", err)
			}
			if _, _, err := collections.GetKey(ctx, database, other.ID, target); !errors.Is(err, collections.ErrNotFound) {
				t.Errorf("GetKey() error = %v, want ErrNotFound", err)
			}
			if _, err := collections.PutKey(ctx, database, other.ID, target, keyInput("x"), nil); !errors.Is(err, collections.ErrNotFound) {
				t.Errorf("PutKey() error = %v, want ErrNotFound", err)
			}
			if err := collections.Delete(ctx, database, other.ID, target); !errors.Is(err, collections.ErrNotFound) {
				t.Errorf("Delete() error = %v, want ErrNotFound", err)
			}
		})
	}
}

// Before any claim the collection has no key: GetKey reports it unclaimed and a
// rotation has nothing to rotate.
func TestUnclaimedKey(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	if _, claimed, err := collections.GetKey(ctx, database, userID, id); err != nil || claimed {
		t.Fatalf("GetKey() claimed = %t, error = %v, want false, nil", claimed, err)
	}
	stale := "2026-01-01T00:00:00Z"
	if _, err := collections.PutKey(ctx, database, userID, id, keyInput("x"), &stale); !errors.Is(err, collections.ErrNoCurrentKey) {
		t.Fatalf("PutKey() error = %v, want ErrNoCurrentKey", err)
	}
}
