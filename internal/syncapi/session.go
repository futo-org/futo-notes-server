package syncapi

import (
	"context"
	"net/http"
	"strings"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/db"
)

// SessionCookie is the name of the cookie that carries a session token.
const SessionCookie = "session"

type sessionCtxKey struct{}

// WithSession returns ctx carrying the authenticated session, for the
// handlers in this package to read.
func WithSession(ctx context.Context, session *auth.Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, session)
}

// SessionFrom returns the session the authentication middleware attached.
// Only call it from handlers mounted behind that middleware.
func SessionFrom(r *http.Request) *auth.Session {
	return r.Context().Value(sessionCtxKey{}).(*auth.Session)
}

// bearerToken extracts the token from an Authorization header, or returns ""
// if the header is not "Bearer <token>". Deviation from the TS server, which
// passed a malformed header through to token validation: see the migration
// plan's How Authentication Works section.
func bearerToken(header string) string {
	if len(header) >= 7 && strings.EqualFold(header[:7], "Bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

// SessionToken returns the raw session token a request presents, or "". The
// session cookie is checked before the Authorization header.
func SessionToken(r *http.Request) string {
	if cookie, err := r.Cookie(SessionCookie); err == nil {
		return cookie.Value
	}
	if header := r.Header.Get("Authorization"); header != "" {
		return bearerToken(header)
	}
	return ""
}

// invalidSessionResponse is the 401 body for a token that names no live
// session.
type invalidSessionResponse struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// WriteUnauthorized answers a request that presented no session token.
func WriteUnauthorized(w http.ResponseWriter) {
	WriteError(w, http.StatusUnauthorized, "unauthorized")
}

// WriteInvalidSession answers a request whose token names no live session.
// It is distinct from WriteUnauthorized so clients re-login without treating
// this as a password change or wiping their sync cursor.
func WriteInvalidSession(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="futo-notes", error="invalid_token"`)
	WriteJSON(w, http.StatusUnauthorized, invalidSessionResponse{
		Code:  "invalid_session",
		Error: "session expired or invalid",
	})
}

// currentUserResponse is the body of GET /api/auth.
type currentUserResponse struct {
	User auth.User `json:"user"`
}

// HandleCurrentUser serves GET /api/auth.
func HandleCurrentUser(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, currentUserResponse{User: SessionFrom(r).User})
}

// HandleLogout serves POST /api/auth/logout: it deletes the caller's session
// and clears the session cookie.
func HandleLogout(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := auth.DeleteSession(r.Context(), database, SessionFrom(r).ID); err != nil {
			ServerError(w, r, "logout: deleting session", err)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:   SessionCookie,
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
		w.WriteHeader(http.StatusNoContent)
	}
}
