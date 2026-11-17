package collections_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/collections"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
)

func keyTestCollection(t *testing.T) (context.Context, *db.DB, string, string) {
	t.Helper()
	ctx := t.Context()
	database := dbtest.SQLite(t)
	user, err := auth.UpsertUserByEmail(ctx, database, "key@example.invalid", "Key")
	if err != nil {
		t.Fatal(err)
	}
	collection, created, err := collections.Claim(ctx, database, user.ID)
	if err != nil || !created {
		t.Fatalf("Claim() = %#v, %t, %v", collection, created, err)
	}
	return ctx, database, user.ID, collection.ID
}

func keyInput(suffix string) collections.KeyInput {
	return collections.KeyInput{
		KeySalt:           "salt-" + suffix,
		KeyKDF:            []byte(`{"name":"scrypt"}`),
		EncryptedVaultKey: "wrapped-" + suffix,
	}
}

// token renders the revision token the way the wire does, so the test compares
// what a client would actually send back in previous_key_updated_at.
func token(material collections.KeyMaterial) string {
	return material.KeyUpdatedAt.UTC().Format(time.RFC3339Nano)
}

// getKey reads back the stored key material, failing the test when no key has
// been claimed.
func getKey(ctx context.Context, t *testing.T, database *db.DB, userID, id string) collections.KeyMaterial {
	t.Helper()
	key, claimed, err := collections.GetKey(ctx, database, userID, id)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("no key material")
	}
	return key
}

// wantKeyConflict fails the test unless err is a key conflict, and returns the
// current material it carries.
func wantKeyConflict(t *testing.T, what string, err error) collections.KeyMaterial {
	t.Helper()
	conflict, ok := errors.AsType[*collections.KeyConflictError](err)
	if !ok {
		t.Fatalf("%s = %v, want a key conflict", what, err)
	}
	return conflict.Current
}

// A rotation that lands in the same millisecond as the write it replaces still
// has to hand back a new revision token, or the client cannot tell the two
// revisions apart.
func TestPutKeyRotationAdvancesRevisionToken(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	claimed, err := collections.PutKey(ctx, database, userID, id, keyInput("one"), nil)
	if err != nil {
		t.Fatalf("claim = %v", err)
	}
	previous := token(claimed)

	for i := range 8 {
		rotated, err := collections.PutKey(ctx, database, userID, id, keyInput("two"), &previous)
		if err != nil {
			t.Fatalf("rotation %d = %v", i, err)
		}
		next := token(rotated)
		if next == previous {
			t.Fatalf("rotation %d returned the previous revision token %q", i, next)
		}
		if !rotated.KeyUpdatedAt.After(claimed.KeyUpdatedAt) {
			t.Fatalf("rotation %d key_updated_at %v did not advance past %v",
				i, rotated.KeyUpdatedAt, claimed.KeyUpdatedAt)
		}
		claimed, previous = rotated, next
	}

	// The bumped timestamp still has to be the fixed-width millisecond form the
	// column and the date-time contract expect.
	var raw string
	if err := database.QueryRowContext(ctx,
		`SELECT key_updated_at FROM collections WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(db.TimestampFormat, raw); err != nil {
		t.Fatalf("stored key_updated_at %q is not %s: %v", raw, db.TimestampFormat, err)
	}
}

// A stored revision ahead of the wall clock — a clock step or a burst of
// rotations that already borrowed from the future — must still be replaced by a
// strictly later token rather than one that goes backwards or stands still.
func TestPutKeyRotationAdvancesPastAFutureRevision(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	if _, err := collections.PutKey(ctx, database, userID, id, keyInput("one"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE collections SET key_updated_at = $1 WHERE id = $2`,
		db.Timestamp(time.Now().Add(time.Hour)), id); err != nil {
		t.Fatal(err)
	}
	ahead := getKey(ctx, t, database, userID, id)
	previous := token(ahead)

	rotated, err := collections.PutKey(ctx, database, userID, id, keyInput("two"), &previous)
	if err != nil {
		t.Fatalf("rotation = %v", err)
	}
	if !rotated.KeyUpdatedAt.After(ahead.KeyUpdatedAt) {
		t.Fatalf("rotation key_updated_at %v did not advance past %v",
			rotated.KeyUpdatedAt, ahead.KeyUpdatedAt)
	}

	_, err = collections.PutKey(ctx, database, userID, id, keyInput("three"), &previous)
	wantKeyConflict(t, "rotation from the superseded token", err)
}

// Two clients holding the same revision token must not both rotate: the second
// one is working from a revision that no longer exists and has to get a 409, or
// it silently overwrites the first client's key material.
func TestPutKeyRejectsSecondRotationFromSameToken(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	claimed, err := collections.PutKey(ctx, database, userID, id, keyInput("one"), nil)
	if err != nil {
		t.Fatal(err)
	}
	stale := token(claimed)

	winner, err := collections.PutKey(ctx, database, userID, id, keyInput("two"), &stale)
	if err != nil {
		t.Fatalf("first rotation = %v", err)
	}
	_, err = collections.PutKey(ctx, database, userID, id, keyInput("three"), &stale)
	current := wantKeyConflict(t, "second rotation from the same token", err)
	if current.EncryptedVaultKey != winner.EncryptedVaultKey {
		t.Fatalf("conflict returned %q, want the first rotation's %q",
			current.EncryptedVaultKey, winner.EncryptedVaultKey)
	}

	if stored := getKey(ctx, t, database, userID, id); stored.EncryptedVaultKey != winner.EncryptedVaultKey {
		t.Fatalf("stored key = %q, want the first rotation's %q",
			stored.EncryptedVaultKey, winner.EncryptedVaultKey)
	}
}

// The same guarantee under real concurrency: whichever rotation gets there
// first wins, and the loser is told so instead of clobbering it.
func TestPutKeyConcurrentRotationsFromSameToken(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	material, err := collections.PutKey(ctx, database, userID, id, keyInput("claim"), nil)
	if err != nil {
		t.Fatal(err)
	}

	for round := range 20 {
		stale := token(material)
		results := make([]collections.KeyMaterial, 2)
		errs := make([]error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range results {
			wg.Go(func() {
				<-start
				results[i], errs[i] = collections.PutKey(ctx, database, userID, id,
					keyInput(string(rune('a'+i))), &stale)
			})
		}
		close(start)
		wg.Wait()

		var winners int
		for i := range results {
			if errs[i] == nil {
				winners++
				material = results[i]
				continue
			}
			if _, ok := errors.AsType[*collections.KeyConflictError](errs[i]); !ok {
				t.Fatalf("round %d rotation %d: %v", round, i, errs[i])
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d of 2 rotations from revision %q succeeded, want 1",
				round, winners, stale)
		}
		if stored := getKey(ctx, t, database, userID, id); stored.EncryptedVaultKey != material.EncryptedVaultKey {
			t.Fatalf("round %d: stored key %q is not the winner's %q",
				round, stored.EncryptedVaultKey, material.EncryptedVaultKey)
		}
	}
}
