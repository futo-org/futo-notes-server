package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
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

// Session is a validated client session attached to authenticated requests.
type Session struct {
	ID   string
	User User
}

// ValidateSession looks up a session by hash(rawToken) and returns the
// attached user. Returns nil for missing or expired tokens; an expired row is
// deleted on presentation.
func ValidateSession(ctx context.Context, database *sql.DB, rawToken string) (*Session, error) {
	var s Session
	var expiresAt time.Time
	err := database.QueryRowContext(ctx,
		`SELECT s.id, s.expires_at, u.id, u.email, u.name
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.access_token_hash = $1`, HashToken(rawToken)).
		Scan(&s.ID, &expiresAt, &s.User.ID, &s.User.Email, &s.User.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !expiresAt.After(time.Now()) {
		if _, err := database.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, s.ID); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return &s, nil
}

// DeleteSession removes a session row by id. Deleting an already-gone
// session is not an error.
func DeleteSession(ctx context.Context, database *sql.DB, sessionID string) error {
	_, err := database.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID)
	return err
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
