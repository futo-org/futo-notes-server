// Package auth checks the Bearer tokens the API mints for the storage
// service: access tokens on /blobs, admin tokens on /internal.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/httputil"
)

// Token kinds, as reported to OnFailure.
const (
	KindAccess = "access"
	KindAdmin  = "admin"
)

// Failure reasons, as reported to OnFailure.
const (
	ReasonMissing = "missing"
	ReasonExpired = "expired"
	ReasonInvalid = "invalid"
)

// OnFailure is told about every rejected request.
type OnFailure func(ctx context.Context, kind, reason string)

type contextKey struct{}

// UserID returns the user the request's access token names. It panics outside
// an Access-protected route, which would be a routing bug.
func UserID(ctx context.Context) string {
	return ctx.Value(contextKey{}).(string)
}

// Access admits requests carrying a valid access token for this region and
// stores its user for UserID and on the request logger.
func Access(v *hostedauth.Verifier, onFailure OnFailure) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearer(w, r, KindAccess, onFailure)
			if !ok {
				return
			}
			access, err := v.VerifyAccess(token)
			if err != nil {
				reject(w, r, KindAccess, err, onFailure)
				return
			}
			// Mutate the request logger in place: the access log line is
			// written through the outer request's logger, so this is what puts
			// the user on it.
			hlog.FromRequest(r).UpdateContext(func(c zerolog.Context) zerolog.Context {
				return c.Str("user_id", access.UserID)
			})
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, access.UserID)))
		})
	}
}

// Admin admits requests carrying a valid admin token for this region.
func Admin(v *hostedauth.Verifier, onFailure OnFailure) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearer(w, r, KindAdmin, onFailure)
			if !ok {
				return
			}
			if err := v.VerifyAdmin(token); err != nil {
				reject(w, r, KindAdmin, err, onFailure)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearer(w http.ResponseWriter, r *http.Request, kind string, onFailure OnFailure) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		onFailure(r.Context(), kind, ReasonMissing)
		httputil.WriteError(w, r, http.StatusUnauthorized, "Expected Bearer token")
		return "", false
	}
	return token, true
}

func reject(w http.ResponseWriter, r *http.Request, kind string, err error, onFailure OnFailure) {
	reason := ReasonInvalid
	if errors.Is(err, jwt.ErrTokenExpired) {
		reason = ReasonExpired
	}
	hlog.FromRequest(r).Info().Err(err).Str("token_kind", kind).Msg("rejected token")
	onFailure(r.Context(), kind, reason)
	httputil.WriteError(w, r, http.StatusUnauthorized, "Invalid token")
}
