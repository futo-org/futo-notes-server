package account

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"uuid"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/ids"
)

const (
	// SessionTTL is how long a hosted session lives past its last use. Every
	// use slides it, so an active device stays signed in.
	SessionTTL = 90 * 24 * time.Hour
	// touchInterval bounds how often use is written back: a session used
	// within this long of its last recorded use is not updated, so busy
	// clients do not write on every request.
	touchInterval = 10 * time.Minute
	// maxUserAgent caps the stored User-Agent.
	maxUserAgent = 512
)

// Principal is the caller an authenticated request acts as.
type Principal struct {
	Session auth.Session
	// RegionID is "" until the user chooses a region.
	RegionID            string
	DeletionRequestedAt *time.Time
	// Slid reports that this authentication extended the session's expiry
	// to SessionTTL from now, so a cookie carrying the token should be
	// extended too.
	Slid bool
}

// PendingDeletion reports whether the caller's account is scheduled for
// deletion.
func (p Principal) PendingDeletion() bool { return p.DeletionRequestedAt != nil }

// execer is what creating a session needs from a database or transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// createSession opens a session for userID through q and returns its raw
// token and id.
func (s *Store) createSession(ctx context.Context, q execer, userID, userAgent string) (token, sessionID string, err error) {
	if len(userAgent) > maxUserAgent {
		userAgent = userAgent[:maxUserAgent]
	}
	now := s.now()
	token, sessionID = auth.GenerateToken(), uuid.NewV7().String()
	_, err = q.ExecContext(ctx, `INSERT INTO sessions
		(id, user_id, access_token_hash, expires_at, created_at, last_used_at, user_agent)
		VALUES ($1, $2, $3, $4, $5, $5, $6)`,
		sessionID, userID, auth.HashToken(token), now.Add(SessionTTL), now, userAgent)
	if err != nil {
		return "", "", err
	}
	return token, sessionID, nil
}

// CreateSession opens a session for userID, recording userAgent for the
// device list, and returns its raw token. Only the token's hash is stored.
func (s *Store) CreateSession(ctx context.Context, userID, userAgent string) (string, error) {
	token, _, err := s.createSession(ctx, s.database, userID, userAgent)
	return token, err
}

// Authenticate returns the caller a raw session token names, sliding the
// session's expiry when it was last recorded in use more than touchInterval
// ago and reporting that in Principal.Slid. It returns auth.ErrNoSession for an unknown or expired token, deleting
// an expired session.
func (s *Store) Authenticate(ctx context.Context, token string) (Principal, error) {
	var p Principal
	var expiresAt, lastUsedAt time.Time
	var region sql.NullString
	err := s.database.QueryRowContext(ctx, `SELECT s.id, s.expires_at, s.last_used_at,
			u.id, u.email, u.name, u.region_id, u.deletion_requested_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.access_token_hash = $1`, auth.HashToken(token)).
		Scan(&p.Session.ID, &expiresAt, &lastUsedAt,
			&p.Session.User.ID, &p.Session.User.Email, &p.Session.User.Name, &region, &p.DeletionRequestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, auth.ErrNoSession
	}
	if err != nil {
		return Principal{}, err
	}
	p.RegionID = region.String
	now := s.now()
	if !expiresAt.After(now) {
		if _, err := s.database.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, p.Session.ID); err != nil {
			return Principal{}, err
		}
		return Principal{}, auth.ErrNoSession
	}
	if now.Sub(lastUsedAt) >= touchInterval {
		// The guard on last_used_at lets only one of several concurrent
		// requests write.
		result, err := s.database.ExecContext(ctx, `UPDATE sessions SET last_used_at = $2, expires_at = $3
			WHERE id = $1 AND last_used_at <= $4`, p.Session.ID, now, now.Add(SessionTTL), now.Add(-touchInterval))
		if err != nil {
			return Principal{}, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return Principal{}, err
		}
		p.Slid = n > 0
	}
	return p, nil
}

// DeviceSession is one of a user's sessions, as the device list shows it.
type DeviceSession struct {
	ID         string
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time
	UserAgent  string
}

// Sessions lists the user's live sessions, most recently used first.
func (s *Store) Sessions(ctx context.Context, userID string) ([]DeviceSession, error) {
	rows, err := s.database.QueryContext(ctx, `SELECT id, created_at, last_used_at, expires_at, user_agent
		FROM sessions WHERE user_id = $1 AND expires_at > $2
		ORDER BY last_used_at DESC, id`, userID, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := []DeviceSession{}
	for rows.Next() {
		var d DeviceSession
		if err := rows.Scan(&d.ID, &d.CreatedAt, &d.LastUsedAt, &d.ExpiresAt, &d.UserAgent); err != nil {
			return nil, err
		}
		sessions = append(sessions, d)
	}
	return sessions, rows.Err()
}

// RevokeSession signs out one of the user's sessions, and with it the storage
// refresh tokens it holds. It returns ErrNotFound unless the user owns it.
func (s *Store) RevokeSession(ctx context.Context, userID, sessionID string) error {
	if !ids.IsUUID(sessionID) {
		return ErrNotFound
	}
	result, err := s.database.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1 AND user_id = $2`, sessionID, userID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
