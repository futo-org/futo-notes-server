package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/hosted/billing"
)

// RotationKeySize is the length of the key storage refresh token successors
// are derived with.
const RotationKeySize = 32

// Refresh token exchange failures.
var (
	// ErrInvalidRefreshToken reports a token that names no live session, or
	// one whose successor is unknown because the rotation key changed since
	// it was exchanged. The session, if any, is left alone.
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	// ErrRefreshTokenReused reports a token whose successor had already been
	// exchanged. Its whole session, every token it holds included, has been
	// revoked.
	ErrRefreshTokenReused = errors.New("refresh token reused")
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

// successor is the token that replaces token when it is exchanged. Deriving
// it rather than drawing it at random makes the exchange idempotent: the
// server can hand the same successor out again without ever storing it raw.
func (s *Store) successor(token string) string {
	mac := hmac.New(sha256.New, s.rotationKey)
	mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Exchange is the outcome of exchanging a storage refresh token.
type Exchange struct {
	UserID   string
	RegionID string
	// Entitled reports that the account may write: its storage access token
	// gets the write scope.
	Entitled bool
	// RefreshToken replaces the exchanged token.
	RefreshToken string
	// Reissued reports that the token had been exchanged before and
	// RefreshToken is the successor that exchange returned.
	Reissued bool
}

// ExchangeRefreshToken spends a storage refresh token for the right to an
// access token, returning its successor. The exchange is a use of the
// session, so it slides the session's expiry too.
//
// Presenting a token again while its successor has never been exchanged
// returns the same successor: a client whose response was lost, or two
// concurrent exchanges, all end up holding it. Presenting it once the
// successor has been exchanged is reuse: it fails with ErrRefreshTokenReused
// and revokes the session. A token that is not reused fails with
// ErrPendingDeletion or ErrNoRegion, unspent, when the account cannot hold
// storage tokens.
//
// The presented token's row lock serializes exchanges of it across replicas.
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
	err = tx.QueryRowContext(ctx, `SELECT t.session_id, t.rotated_at, s.expires_at, u.id, u.region_id, u.deletion_requested_at,
			`+billing.EntitledSQL("u.id")+`
		FROM storage_refresh_tokens t
		JOIN sessions s ON s.id = t.session_id
		JOIN users u ON u.id = s.user_id
		WHERE t.token_hash = $1 FOR UPDATE OF t`, hash).
		Scan(&sessionID, &rotatedAt, &expiresAt, &exchange.UserID, &region, &deletionRequestedAt, &exchange.Entitled)
	if errors.Is(err, sql.ErrNoRows) {
		return Exchange{}, ErrInvalidRefreshToken
	}
	if err != nil {
		return Exchange{}, err
	}
	now := s.now()
	if !expiresAt.After(now) {
		return Exchange{}, ErrInvalidRefreshToken
	}
	exchange.RefreshToken = s.successor(token)
	successorHash := auth.HashToken(exchange.RefreshToken)

	if rotatedAt != nil {
		var successorRotatedAt *time.Time
		err := tx.QueryRowContext(ctx, `SELECT rotated_at FROM storage_refresh_tokens
			WHERE token_hash = $1 AND session_id = $2 FOR UPDATE`, successorHash, sessionID).Scan(&successorRotatedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Derived with a rotation key that has since been replaced.
			return Exchange{}, ErrInvalidRefreshToken
		case err != nil:
			return Exchange{}, err
		case successorRotatedAt != nil:
			if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID); err != nil {
				return Exchange{}, err
			}
			if err := tx.Commit(); err != nil {
				return Exchange{}, err
			}
			return Exchange{}, ErrRefreshTokenReused
		}
	}
	switch {
	case deletionRequestedAt != nil:
		return Exchange{}, ErrPendingDeletion
	case !region.Valid:
		return Exchange{}, ErrNoRegion
	}
	exchange.RegionID = region.String
	exchange.Reissued = rotatedAt != nil

	if rotatedAt == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE storage_refresh_tokens SET rotated_at = $2 WHERE token_hash = $1`, hash, now); err != nil {
			return Exchange{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO storage_refresh_tokens (token_hash, session_id, created_at)
			VALUES ($1, $2, $3) ON CONFLICT (token_hash) DO NOTHING`, successorHash, sessionID, now); err != nil {
			return Exchange{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_used_at = $2, expires_at = $3 WHERE id = $1`,
		sessionID, now, now.Add(SessionTTL)); err != nil {
		return Exchange{}, err
	}
	return exchange, tx.Commit()
}
