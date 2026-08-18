package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"

	"futo-notes-server/internal/uuidv7"
)

const SessionTTL = 7 * 24 * time.Hour

// GenerateToken returns a raw session token: 32 random bytes, hex-encoded.
func GenerateToken() string {
	var b [32]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// HashToken returns the SHA-256 of the raw (hex-encoded) token. Only this
// hash is stored server-side.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// CreateSession opens a session for the user and returns the raw token the
// client authenticates with. Only the token's SHA-256 hash is stored.
func CreateSession(ctx context.Context, database *sql.DB, userID string) (string, error) {
	rawToken := GenerateToken()
	_, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, access_token_hash, expires_at) VALUES ($1, $2, $3, $4)`,
		uuidv7.New(), userID, HashToken(rawToken), time.Now().Add(SessionTTL))
	if err != nil {
		return "", err
	}
	return rawToken, nil
}
