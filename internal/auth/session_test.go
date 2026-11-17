package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"testing"
	"time"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
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

func TestValidateSession(t *testing.T) {
	database := dbtest.SQLite(t)
	user, err := UpsertUserByEmail(t.Context(), database, "session@example.invalid", "Session")
	if err != nil {
		t.Fatal(err)
	}
	token, err := CreateSession(t.Context(), database, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	session, err := ValidateSession(t.Context(), database, token)
	if err != nil {
		t.Fatal(err)
	}
	if session.User != user {
		t.Fatalf("session user = %#v, want %#v", session.User, user)
	}

	if _, err := ValidateSession(t.Context(), database, "unknown"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("unknown token: err = %v, want ErrNoSession", err)
	}

	if _, err := database.ExecContext(t.Context(), `UPDATE sessions SET expires_at = $1`,
		db.Timestamp(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateSession(t.Context(), database, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired token: err = %v, want ErrNoSession", err)
	}
	var remaining int
	if err := database.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("expired session rows = %d, want 0 after presentation", remaining)
	}
}
