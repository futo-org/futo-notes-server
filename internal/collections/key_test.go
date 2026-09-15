package collections_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/collections"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
)

func keyTestCollection(t *testing.T) (context.Context, *db.DB, string, string) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(config.Config{DatabaseURL: "sqlite:" + filepath.Join(t.TempDir(), "notes.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
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
func token(t *testing.T, material *collections.KeyMaterial) string {
	t.Helper()
	if material == nil {
		t.Fatal("no key material")
	}
	return material.KeyUpdatedAt.UTC().Format(time.RFC3339Nano)
}

// A rotation that lands in the same millisecond as the write it replaces still
// has to hand back a new revision token, or the client cannot tell the two
// revisions apart.
func TestPutKeyRotationAdvancesRevisionToken(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	outcome, claimed, err := collections.PutKey(ctx, database, userID, id, keyInput("one"), nil)
	if err != nil || outcome != collections.PutKeyOK {
		t.Fatalf("claim = %v, %v", outcome, err)
	}
	previous := token(t, claimed)

	for i := 0; i < 8; i++ {
		outcome, rotated, err := collections.PutKey(ctx, database, userID, id, keyInput("two"), &previous)
		if err != nil || outcome != collections.PutKeyOK {
			t.Fatalf("rotation %d = %v, %v", i, outcome, err)
		}
		next := token(t, rotated)
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

	if _, _, err := collections.PutKey(ctx, database, userID, id, keyInput("one"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE collections SET key_updated_at = $1 WHERE id = $2`,
		db.Timestamp(time.Now().Add(time.Hour)), id); err != nil {
		t.Fatal(err)
	}
	_, ahead, err := collections.GetKey(ctx, database, userID, id)
	if err != nil {
		t.Fatal(err)
	}
	previous := token(t, ahead)

	outcome, rotated, err := collections.PutKey(ctx, database, userID, id, keyInput("two"), &previous)
	if err != nil || outcome != collections.PutKeyOK {
		t.Fatalf("rotation = %v, %v", outcome, err)
	}
	if !rotated.KeyUpdatedAt.After(ahead.KeyUpdatedAt) {
		t.Fatalf("rotation key_updated_at %v did not advance past %v",
			rotated.KeyUpdatedAt, ahead.KeyUpdatedAt)
	}

	outcome, _, err = collections.PutKey(ctx, database, userID, id, keyInput("three"), &previous)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != collections.PutKeyConflict {
		t.Fatalf("rotation from the superseded token = %v, want PutKeyConflict", outcome)
	}
}

// Two clients holding the same revision token must not both rotate: the second
// one is working from a revision that no longer exists and has to get a 409, or
// it silently overwrites the first client's key material.
func TestPutKeyRejectsSecondRotationFromSameToken(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	_, claimed, err := collections.PutKey(ctx, database, userID, id, keyInput("one"), nil)
	if err != nil {
		t.Fatal(err)
	}
	stale := token(t, claimed)

	outcome, winner, err := collections.PutKey(ctx, database, userID, id, keyInput("two"), &stale)
	if err != nil || outcome != collections.PutKeyOK {
		t.Fatalf("first rotation = %v, %v", outcome, err)
	}
	outcome, current, err := collections.PutKey(ctx, database, userID, id, keyInput("three"), &stale)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != collections.PutKeyConflict {
		t.Fatalf("second rotation from the same token = %v, want PutKeyConflict", outcome)
	}
	if current.EncryptedVaultKey != winner.EncryptedVaultKey {
		t.Fatalf("conflict returned %q, want the first rotation's %q",
			current.EncryptedVaultKey, winner.EncryptedVaultKey)
	}

	_, stored, err := collections.GetKey(ctx, database, userID, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedVaultKey != winner.EncryptedVaultKey {
		t.Fatalf("stored key = %q, want the first rotation's %q",
			stored.EncryptedVaultKey, winner.EncryptedVaultKey)
	}
}

// The same guarantee under real concurrency: whichever rotation gets there
// first wins, and the loser is told so instead of clobbering it.
func TestPutKeyConcurrentRotationsFromSameToken(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	_, material, err := collections.PutKey(ctx, database, userID, id, keyInput("claim"), nil)
	if err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 20; round++ {
		stale := token(t, material)
		outcomes := make([]collections.PutKeyOutcome, 2)
		results := make([]*collections.KeyMaterial, 2)
		errs := make([]error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range outcomes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				outcomes[i], results[i], errs[i] = collections.PutKey(ctx, database, userID, id,
					keyInput(string(rune('a'+i))), &stale)
			}(i)
		}
		close(start)
		wg.Wait()

		var winners int
		for i := range outcomes {
			if errs[i] != nil {
				t.Fatalf("round %d rotation %d: %v", round, i, errs[i])
			}
			switch outcomes[i] {
			case collections.PutKeyOK:
				winners++
				material = results[i]
			case collections.PutKeyConflict:
			default:
				t.Fatalf("round %d rotation %d = %v", round, i, outcomes[i])
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d of 2 rotations from revision %q succeeded, want 1",
				round, winners, stale)
		}
		_, stored, err := collections.GetKey(ctx, database, userID, id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.EncryptedVaultKey != material.EncryptedVaultKey {
			t.Fatalf("round %d: stored key %q is not the winner's %q",
				round, stored.EncryptedVaultKey, material.EncryptedVaultKey)
		}
	}
}

// ptr is the terse way to build the optional recovery fields in a table.
func ptr(s string) *string { return &s }

// recoveryInput is keyInput plus the second envelope, the shape a client that
// knows the Recovery Key sends.
func recoveryInput(suffix string) collections.KeyInput {
	in := keyInput(suffix)
	in.RecoveryKeySalt = ptr("recovery-salt-" + suffix)
	in.RecoveryKeyKDF = []byte(`{"name":"hkdf-sha256"}`)
	in.RecoveryEncryptedVaultKey = ptr("recovery-wrapped-" + suffix)
	return in
}

func assertRecovery(t *testing.T, what string, got *collections.KeyMaterial, wantSuffix string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: no key material", what)
	}
	if got.RecoveryKeySalt == nil || got.RecoveryEncryptedVaultKey == nil {
		t.Fatalf("%s: recovery envelope is absent, want the %q one", what, wantSuffix)
	}
	if *got.RecoveryKeySalt != "recovery-salt-"+wantSuffix {
		t.Fatalf("%s: recovery_key_salt = %q, want %q", what, *got.RecoveryKeySalt, "recovery-salt-"+wantSuffix)
	}
	if *got.RecoveryEncryptedVaultKey != "recovery-wrapped-"+wantSuffix {
		t.Fatalf("%s: recovery_encrypted_vault_key = %q, want %q",
			what, *got.RecoveryEncryptedVaultKey, "recovery-wrapped-"+wantSuffix)
	}
	if string(got.RecoveryKeyKDF) != `{"name":"hkdf-sha256"}` {
		t.Fatalf("%s: recovery_key_kdf = %s", what, got.RecoveryKeyKDF)
	}
}

func assertNoRecovery(t *testing.T, what string, got *collections.KeyMaterial) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: no key material", what)
	}
	if got.RecoveryKeySalt != nil || got.RecoveryEncryptedVaultKey != nil || got.RecoveryKeyKDF != nil {
		t.Fatalf("%s: recovery envelope = %v/%s/%v, want all absent",
			what, got.RecoveryKeySalt, got.RecoveryKeyKDF, got.RecoveryEncryptedVaultKey)
	}
}

// A client that knows the Recovery Key stores both envelopes at once, and both
// come back from a later read.
func TestPutKeyStoresBothEnvelopes(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	outcome, claimed, err := collections.PutKey(ctx, database, userID, id, recoveryInput("one"), nil)
	if err != nil || outcome != collections.PutKeyOK {
		t.Fatalf("claim = %v, %v", outcome, err)
	}
	assertRecovery(t, "claim", claimed, "one")

	_, stored, err := collections.GetKey(ctx, database, userID, id)
	if err != nil {
		t.Fatal(err)
	}
	assertRecovery(t, "read back", stored, "one")
	if stored.EncryptedVaultKey != "wrapped-one" {
		t.Fatalf("password envelope = %q, want %q", stored.EncryptedVaultKey, "wrapped-one")
	}
}

// A v1 client sends only the password envelope. That is not an error and not a
// half-written row: the recovery columns stay NULL and read back absent.
func TestPutKeyWithoutARecoveryEnvelopeLeavesItAbsent(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	outcome, claimed, err := collections.PutKey(ctx, database, userID, id, keyInput("one"), nil)
	if err != nil || outcome != collections.PutKeyOK {
		t.Fatalf("claim = %v, %v", outcome, err)
	}
	assertNoRecovery(t, "claim", claimed)

	_, stored, err := collections.GetKey(ctx, database, userID, id)
	if err != nil {
		t.Fatal(err)
	}
	assertNoRecovery(t, "read back", stored)
}

// The re-wrap: an unlocked client holding the current revision replaces both
// envelopes in one write, whether it changed the Vault Password, the Recovery
// Key, or both.
func TestPutKeyRewrapReplacesBothEnvelopes(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	_, claimed, err := collections.PutKey(ctx, database, userID, id, recoveryInput("one"), nil)
	if err != nil {
		t.Fatal(err)
	}
	current := token(t, claimed)

	outcome, rewrapped, err := collections.PutKey(ctx, database, userID, id, recoveryInput("two"), &current)
	if err != nil || outcome != collections.PutKeyOK {
		t.Fatalf("re-wrap = %v, %v", outcome, err)
	}
	assertRecovery(t, "re-wrap", rewrapped, "two")
	if rewrapped.EncryptedVaultKey != "wrapped-two" {
		t.Fatalf("password envelope = %q, want %q", rewrapped.EncryptedVaultKey, "wrapped-two")
	}

	_, stored, err := collections.GetKey(ctx, database, userID, id)
	if err != nil {
		t.Fatal(err)
	}
	assertRecovery(t, "read back", stored, "two")
}

// A v1 client re-wrapping mints a brand-new vault key, so the recovery
// envelope it does not send would wrap a key that no longer exists. Omitting
// the envelope clears it rather than stranding it.
func TestPutKeyRewrapWithoutRecoveryClearsIt(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	_, claimed, err := collections.PutKey(ctx, database, userID, id, recoveryInput("one"), nil)
	if err != nil {
		t.Fatal(err)
	}
	current := token(t, claimed)

	outcome, rewrapped, err := collections.PutKey(ctx, database, userID, id, keyInput("two"), &current)
	if err != nil || outcome != collections.PutKeyOK {
		t.Fatalf("re-wrap = %v, %v", outcome, err)
	}
	assertNoRecovery(t, "re-wrap", rewrapped)

	_, stored, err := collections.GetKey(ctx, database, userID, id)
	if err != nil {
		t.Fatal(err)
	}
	assertNoRecovery(t, "read back", stored)
}

// A stale revision token still loses, and the authoritative material it comes
// back with carries the winner's recovery envelope — otherwise the loser
// adopts a vault key it cannot open with its recovery key.
func TestPutKeyConflictCarriesTheRecoveryEnvelope(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	_, claimed, err := collections.PutKey(ctx, database, userID, id, recoveryInput("one"), nil)
	if err != nil {
		t.Fatal(err)
	}
	stale := token(t, claimed)

	if _, _, err := collections.PutKey(ctx, database, userID, id, recoveryInput("two"), &stale); err != nil {
		t.Fatal(err)
	}
	outcome, current, err := collections.PutKey(ctx, database, userID, id, recoveryInput("three"), &stale)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != collections.PutKeyConflict {
		t.Fatalf("re-wrap from a stale token = %v, want PutKeyConflict", outcome)
	}
	assertRecovery(t, "conflict", current, "two")
}

// First claim is unchanged: losing the race is not an error, and the winner's
// material — recovery envelope included — is what the loser adopts.
func TestPutKeyLosingFirstClaimAdoptsTheWinnersRecoveryEnvelope(t *testing.T) {
	ctx, database, userID, id := keyTestCollection(t)

	if _, _, err := collections.PutKey(ctx, database, userID, id, recoveryInput("winner"), nil); err != nil {
		t.Fatal(err)
	}
	outcome, adopted, err := collections.PutKey(ctx, database, userID, id, recoveryInput("loser"), nil)
	if err != nil || outcome != collections.PutKeyOK {
		t.Fatalf("second claim = %v, %v", outcome, err)
	}
	assertRecovery(t, "second claim", adopted, "winner")
}
