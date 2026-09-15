package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"futo-notes-server/internal/db"
)

const (
	// SessionTTL is how long a Fixed Session lives from the moment it is
	// opened, whatever the client does with it.
	SessionTTL = 7 * 24 * time.Hour

	// SlidingSessionTTL is how long a Sliding Session lives from its last
	// authenticated request. A device that syncs at least this often never
	// has to log in again.
	SlidingSessionTTL = 90 * 24 * time.Hour

	// SlidingRenewInterval is how stale a Sliding Session's expiry may get
	// before a request writes it forward. Requests inside the interval leave
	// the row alone, so a device polling every few seconds writes its session
	// 24 times a day instead of thousands; the price is that the session can
	// die up to an hour early, which is an hour off ninety days.
	SlidingRenewInterval = time.Hour
)

// SessionKind says how a session's expiry is decided. Every login path picks
// one deliberately: the two lifetimes are a product decision, not a default.
type SessionKind int

const (
	// FixedSession expires SessionTTL after it was opened and is never
	// renewed. Browser cookie logins get this, as do password- and dev-mode
	// logins, where logging in again is typing a password the person already
	// has rather than a round trip through an identity provider.
	FixedSession SessionKind = iota

	// SlidingSession expires SlidingSessionTTL after its last authenticated
	// request. Only redeeming a Login Hand-off ticket opens one, because that
	// is the only login an app cannot perform for itself.
	SlidingSession
)

// TTL is the lifetime a session of this kind is opened with, and for a
// Sliding Session the lifetime every renewal restores.
func (k SessionKind) TTL() time.Duration {
	if k == SlidingSession {
		return SlidingSessionTTL
	}
	return SessionTTL
}

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
	ID        string
	User      User
	Kind      SessionKind
	ExpiresAt time.Time
}

// dueForRenewal reports whether a Sliding Session's row has fallen far enough
// behind to be worth writing. A sliding row's expires_at is its last renewal
// plus SlidingSessionTTL, so the row is due exactly once that renewal is
// SlidingRenewInterval old.
func dueForRenewal(expiresAt, now time.Time) bool {
	return !expiresAt.After(now.Add(SlidingSessionTTL - SlidingRenewInterval))
}

// ValidateSession looks up a session by hash(rawToken) and returns the
// attached user. Returns nil for missing or expired tokens; an expired row is
// deleted on presentation. A live Sliding Session has its expiry written
// forward here, at most once per SlidingRenewInterval.
func ValidateSession(ctx context.Context, database *db.DB, rawToken string) (*Session, error) {
	var s Session
	var expiresAt db.Time
	var sliding bool
	err := database.QueryRowContext(ctx,
		`SELECT s.id, s.expires_at, s.sliding, u.id, u.email, u.name
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.access_token_hash = $1`, HashToken(rawToken)).
		Scan(&s.ID, &expiresAt, &sliding, &s.User.ID, &s.User.Email, &s.User.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if !expiresAt.Time.After(now) {
		if _, err := database.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, s.ID); err != nil {
			return nil, err
		}
		return nil, nil
	}
	s.ExpiresAt = expiresAt.Time
	if sliding {
		s.Kind = SlidingSession
	}
	if sliding && dueForRenewal(expiresAt.Time, now) {
		renewed := now.Add(SlidingSessionTTL)
		if _, err := database.ExecContext(ctx, `UPDATE sessions SET expires_at = $1 WHERE id = $2`,
			db.Timestamp(renewed), s.ID); err != nil {
			// The session is already known good, and the next request an hour
			// from now retries the write. Failing the request instead would
			// turn a busy database into a 401-looking outage on a read that
			// needed no write at all before sessions slid.
			slog.Error("extending a sliding session", "session", s.ID, "err", err)
		} else {
			s.ExpiresAt = renewed
		}
	}
	return &s, nil
}

// DeleteSession removes a session row by id. Deleting an already-gone
// session is not an error.
func DeleteSession(ctx context.Context, database *db.DB, sessionID string) error {
	_, err := database.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID)
	return err
}

// CreateSession opens a session of the given kind for the user and returns
// the raw token the client authenticates with. Only the token's SHA-256 hash
// is stored.
func CreateSession(ctx context.Context, database *db.DB, userID string, kind SessionKind) (string, error) {
	rawToken := GenerateToken()
	_, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, access_token_hash, expires_at, sliding) VALUES ($1, $2, $3, $4, $5)`,
		uuid.NewV7().String(), userID, HashToken(rawToken),
		db.Timestamp(time.Now().Add(kind.TTL())), kind == SlidingSession)
	if err != nil {
		return "", err
	}
	return rawToken, nil
}
