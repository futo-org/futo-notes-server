package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
	"time"
)

func TestGenerateToken(t *testing.T) {
	tok := GenerateToken()
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(tok))
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}
	if GenerateToken() == tok {
		t.Fatal("two tokens are identical")
	}
}

func TestHashToken(t *testing.T) {
	// The stored hash must be SHA-256 of the hex string itself, not of the
	// decoded bytes — sessions created by the TS server depend on this.
	raw := "00ff"
	want := sha256.Sum256([]byte{'0', '0', 'f', 'f'})
	if !slices.Equal(HashToken(raw), want[:]) {
		t.Fatal("hash is not SHA-256 of the hex string")
	}
}

func TestDueForRenewal(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	// A sliding row's expiry is its last request plus SlidingSessionTTL, so
	// "used ago" is the only input that matters.
	expiryAfter := func(ago time.Duration) time.Time {
		return now.Add(-ago).Add(SlidingSessionTTL)
	}
	for _, c := range []struct {
		ago  time.Duration
		want bool
	}{
		{0, false},
		{SlidingRenewInterval - time.Second, false},
		{SlidingRenewInterval, true},
		{SlidingRenewInterval + time.Second, true},
		{SlidingSessionTTL, true},
	} {
		if got := dueForRenewal(expiryAfter(c.ago), now); got != c.want {
			t.Errorf("dueForRenewal(used %v ago) = %v, want %v", c.ago, got, c.want)
		}
	}
}
