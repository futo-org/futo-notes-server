package account

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"futo-notes-server/internal/auth"
)

// rotationGrace is how long after a refresh token rotates that presenting it
// again counts as a concurrent exchange rather than reuse. Two requests that
// raced with the same token both arrive within it; a stolen copy replayed
// later does not.
const rotationGrace = 10 * time.Second

// Refresh token exchange failures.
var (
	// ErrInvalidRefreshToken reports a token that names no live session.
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	// ErrRefreshTokenReused reports a token that had already been exchanged.
	// Its whole session, every token it holds included, has been revoked.
	ErrRefreshTokenReused = errors.New("refresh token reused")
	// ErrRefreshTokenRotating reports a token that a concurrent exchange
	// rotated moments ago. That exchange holds the new token; the session
	// stays signed in.
	ErrRefreshTokenRotating = errors.New("refresh token already rotated by a concurrent exchange")
)

// issueRefreshToken creates a storage refresh token for sessionID through q
// and returns it raw. Only its hash is stored.
func (s *Store) issueRefreshToken(ctx context.Context, q execer, sessionID string) (string, error) {
	token := auth.GenerateToken()
	_, err := q.ExecContext(ctx, `INSERT INTO storage_refresh_tokens (token_hash, session_id, created_at)
		VALUES ($1, $2, $3)`, auth.HashToken(token), sessionID, s.now())
	if err != nil {
		return "", err
	}
	return token, nil
}

// Exchange is the outcome of exchanging a storage refresh token.
type Exchange struct {
	UserID   string
	RegionID string
	// RefreshToken replaces the exchanged token, which is now spent.
	RefreshToken string
}

// ExchangeRefreshToken spends a storage refresh token for the right to an
// access token, returning its replacement. The exchange is a use of the
// session, so it slides the session's expiry too.
//
// The token's row lock serializes concurrent exchanges across replicas:
// exactly one rotates it. The others then see it rotated, and within
// rotationGrace fail with ErrRefreshTokenRotating, leaving the session
// alone. Presenting it later fails with ErrRefreshTokenReused and revokes the
// session. A token that is not reused fails with ErrPendingDeletion or
// ErrNoRegion, unspent, when the account cannot hold storage tokens.
func (s *Store) ExchangeRefreshToken(ctx context.Context, token string) (Exchange, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return Exchange{}, err
	}
	defer tx.Discard()

	hash := auth.HashToken(token)
	var sessionID string
	var rotatedAt, deletionRequestedAt *time.Time
	var expiresAt time.Time
	var exchange Exchange
	var region sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT t.session_id, t.rotated_at, s.expires_at, u.id, u.region_id, u.deletion_requested_at
		FROM storage_refresh_tokens t
		JOIN sessions s ON s.id = t.session_id
		JOIN users u ON u.id = s.user_id
		WHERE t.token_hash = $1 FOR UPDATE OF t`, hash).
		Scan(&sessionID, &rotatedAt, &expiresAt, &exchange.UserID, &region, &deletionRequestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Exchange{}, ErrInvalidRefreshToken
	}
	if err != nil {
		return Exchange{}, err
	}
	now := s.now()
	switch {
	case !expiresAt.After(now):
		return Exchange{}, ErrInvalidRefreshToken
	case rotatedAt != nil && now.Sub(*rotatedAt) < rotationGrace:
		return Exchange{}, ErrRefreshTokenRotating
	case rotatedAt != nil:
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID); err != nil {
			return Exchange{}, err
		}
		if err := tx.Commit(); err != nil {
			return Exchange{}, err
		}
		return Exchange{}, ErrRefreshTokenReused
	case deletionRequestedAt != nil:
		return Exchange{}, ErrPendingDeletion
	case !region.Valid:
		return Exchange{}, ErrNoRegion
	}
	exchange.RegionID = region.String

	if _, err := tx.ExecContext(ctx, `UPDATE storage_refresh_tokens SET rotated_at = $2 WHERE token_hash = $1`, hash, now); err != nil {
		return Exchange{}, err
	}
	if exchange.RefreshToken, err = s.issueRefreshToken(ctx, tx, sessionID); err != nil {
		return Exchange{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_used_at = $2, expires_at = $3 WHERE id = $1`,
		sessionID, now, now.Add(SessionTTL)); err != nil {
		return Exchange{}, err
	}
	return exchange, tx.Commit()
}
