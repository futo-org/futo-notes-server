package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
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
