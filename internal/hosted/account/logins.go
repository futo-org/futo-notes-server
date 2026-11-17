package account

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"regexp"
	"time"

	"golang.org/x/oauth2"

	"futo-notes-server/internal/auth"
)

const (
	// LoginTTL is how long a login may take between start and callback.
	LoginTTL = 10 * time.Minute
	// appCodeTTL is how long an app has to swap its one-time code. The app
	// receives it in a redirect and swaps it right away.
	appCodeTTL = time.Minute
)

// verifierPattern is RFC 7636's code_verifier.
var verifierPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// Login failures.
var (
	// ErrUnknownLogin reports a callback whose state names no login in
	// flight: unknown, already completed, or expired.
	ErrUnknownLogin = errors.New("unknown or expired login")
	// ErrInvalidCode reports a one-time code that is unknown, already
	// swapped, or expired.
	ErrInvalidCode = errors.New("invalid or expired code")
	// ErrCodeVerifier reports a code_verifier that does not match the
	// code_challenge the app started with. The code is spent regardless.
	ErrCodeVerifier = errors.New("code_verifier does not match code_challenge")
)

// Login is a sign-in in flight with the identity provider. State, Nonce and
// CodeVerifier belong to the API's own request to the provider. The App
// fields are set when an app, not the dashboard, started the login: where to
// send it back, the state to echo, and the PKCE challenge it must answer.
type Login struct {
	State            string
	Nonce            string
	CodeVerifier     string
	AppRedirectURI   string
	AppState         string
	AppCodeChallenge string
}

// ForApp reports whether an app started the login.
func (l Login) ForApp() bool { return l.AppRedirectURI != "" }

// SaveLogin records a login in flight for LoginTTL.
func (s *Store) SaveLogin(ctx context.Context, l Login) error {
	_, err := s.database.ExecContext(ctx, `INSERT INTO oidc_logins
		(state_hash, nonce, code_verifier, app_redirect_uri, app_state, app_code_challenge, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, auth.HashToken(l.State), l.Nonce, l.CodeVerifier,
		nullable(l.AppRedirectURI), nullable(l.AppState), nullable(l.AppCodeChallenge), s.now().Add(LoginTTL))
	return err
}

// TakeLogin removes and returns the login in flight under state, so a
// callback completes it at most once. It returns ErrUnknownLogin when there
// is none or it expired.
func (s *Store) TakeLogin(ctx context.Context, state string) (Login, error) {
	l := Login{State: state}
	var redirectURI, appState, challenge sql.NullString
	var expiresAt time.Time
	err := s.database.QueryRowContext(ctx, `DELETE FROM oidc_logins WHERE state_hash = $1
		RETURNING nonce, code_verifier, app_redirect_uri, app_state, app_code_challenge, expires_at`, auth.HashToken(state)).
		Scan(&l.Nonce, &l.CodeVerifier, &redirectURI, &appState, &challenge, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Login{}, ErrUnknownLogin
	}
	if err != nil {
		return Login{}, err
	}
	if !expiresAt.After(s.now()) {
		return Login{}, ErrUnknownLogin
	}
	l.AppRedirectURI, l.AppState, l.AppCodeChallenge = redirectURI.String, appState.String, challenge.String
	return l, nil
}

// CreateAppCode issues the one-time code an app swaps for a session of
// userID, bound to the app's PKCE codeChallenge. Only its hash is stored.
func (s *Store) CreateAppCode(ctx context.Context, userID, codeChallenge string) (string, error) {
	code := auth.GenerateToken()
	_, err := s.database.ExecContext(ctx, `INSERT INTO oidc_app_codes (code_hash, user_id, code_challenge, expires_at)
		VALUES ($1, $2, $3, $4)`, auth.HashToken(code), userID, codeChallenge, s.now().Add(appCodeTTL))
	if err != nil {
		return "", err
	}
	return code, nil
}

// AppSession is what an app receives for its one-time code.
type AppSession struct {
	Token        string
	RefreshToken string
	User         auth.User
}

// RedeemAppCode spends a one-time code: if codeVerifier answers the code's
// PKCE challenge it opens a session for the code's user, with a storage
// refresh token. The code is spent even when the verifier is wrong.
func (s *Store) RedeemAppCode(ctx context.Context, code, codeVerifier, userAgent string) (AppSession, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return AppSession{}, err
	}
	defer tx.Discard()
	var session AppSession
	var challenge string
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `DELETE FROM oidc_app_codes WHERE code_hash = $1
		RETURNING user_id, code_challenge, expires_at`, auth.HashToken(code)).
		Scan(&session.User.ID, &challenge, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AppSession{}, ErrInvalidCode
	}
	if err != nil {
		return AppSession{}, err
	}
	var failure error
	switch {
	case !expiresAt.After(s.now()):
		failure = ErrInvalidCode
	case !pkceMatches(codeVerifier, challenge):
		failure = ErrCodeVerifier
	}
	if failure != nil {
		if err := tx.Commit(); err != nil {
			return AppSession{}, err
		}
		return AppSession{}, failure
	}
	if err := tx.QueryRowContext(ctx, `SELECT email, name FROM users WHERE id = $1`, session.User.ID).
		Scan(&session.User.Email, &session.User.Name); err != nil {
		return AppSession{}, err
	}
	var sessionID string
	if session.Token, sessionID, err = s.createSession(ctx, tx, session.User.ID, userAgent); err != nil {
		return AppSession{}, err
	}
	if session.RefreshToken, err = s.issueRefreshToken(ctx, tx, sessionID); err != nil {
		return AppSession{}, err
	}
	return session, tx.Commit()
}

// DeleteExpiredLogins removes logins in flight and one-time codes that
// expired unused, returning how many.
func (s *Store) DeleteExpiredLogins(ctx context.Context) (int64, error) {
	var deleted int64
	for _, table := range []string{"oidc_logins", "oidc_app_codes"} {
		result, err := s.database.ExecContext(ctx, `DELETE FROM `+table+` WHERE expires_at <= $1`, s.now())
		if err != nil {
			return deleted, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	return deleted, nil
}

// pkceMatches reports whether verifier is a well-formed RFC 7636
// code_verifier whose S256 transform is challenge.
func pkceMatches(verifier, challenge string) bool {
	if !verifierPattern.MatchString(verifier) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(oauth2.S256ChallengeFromVerifier(verifier)), []byte(challenge)) == 1
}

// ValidCodeChallenge reports whether challenge is an S256 code_challenge: the
// unpadded base64url encoding of a SHA-256 digest.
func ValidCodeChallenge(challenge string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(challenge)
	return err == nil && len(decoded) == sha256.Size
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
