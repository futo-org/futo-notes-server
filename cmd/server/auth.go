package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
)

// The login endpoint for each auth mode. Only the active mode's path is
// routed, and it is the one /api/ path reachable without a session.
const (
	devLoginPath      = "/api/auth/dev/login"
	passwordLoginPath = "/api/auth/password/login"
)

// loginPath returns the login endpoint of mode, or "" for an unknown mode.
func loginPath(mode config.AuthMode) string {
	switch mode {
	case config.AuthModeDev:
		return devLoginPath
	case config.AuthModePassword:
		return passwordLoginPath
	}
	return ""
}

type sessionCtxKey struct{}

// sessionFrom returns the session the auth middleware attached. Only call it
// from handlers mounted behind requireAuth.
func sessionFrom(r *http.Request) *auth.Session {
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

// sessionToken returns the raw session token a request presents, or "". The
// session cookie is checked before the Authorization header.
func sessionToken(r *http.Request) string {
	if cookie, err := r.Cookie("session"); err == nil {
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

// requireAuth guards every /api/* route except the login path for the active
// auth mode, which passes through without looking at credentials so that a
// stale session never blocks logging in again.
//
// The exemption compares the decoded request path rather than registering the
// login route ahead of this middleware: the router keeps an escaped slash inside
// its segment, so a route would stop exempting spellings such as
// "/api/auth/dev%2Flogin", which skip authentication and reach the plain-text
// 404.
func requireAuth(cfg config.Config, database *db.DB, next http.Handler) http.Handler {
	publicPath := loginPath(cfg.AuthMode)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPath != "" && r.URL.Path == publicPath {
			next.ServeHTTP(w, r)
			return
		}

		token := sessionToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		session, err := auth.ValidateSession(r.Context(), database, token)
		if errors.Is(err, auth.ErrNoSession) {
			// Distinguish a dead bearer session from a request that supplied
			// no credentials, so clients re-login without treating this as a
			// password change or wiping their sync cursor.
			w.Header().Set("WWW-Authenticate", `Bearer realm="futo-notes", error="invalid_token"`)
			writeJSON(w, http.StatusUnauthorized, invalidSessionResponse{
				Code:  "invalid_session",
				Error: "session expired or invalid",
			})
			return
		}
		if err != nil {
			serverError(w, r, "validating session", err)
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, session)))
	})
}

type devLoginRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// decodeDevLogin reads and normalizes a dev login body. Its errors are the
// client-facing 400 messages.
func decodeDevLogin(r *http.Request) (devLoginRequest, error) {
	var body devLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return devLoginRequest{}, errors.New("invalid json")
	}
	body.Email = strings.ToLower(strings.TrimSpace(body.Email))
	body.Name = strings.TrimSpace(body.Name)
	if body.Email == "" {
		return devLoginRequest{}, errors.New("email is required")
	}
	if body.Name == "" {
		body.Name, _, _ = strings.Cut(body.Email, "@")
	}
	if body.Name == "" {
		return devLoginRequest{}, errors.New("email is required")
	}
	return body, nil
}

// loginResponse is the body of a successful login. The token is also set as
// the session cookie.
type loginResponse struct {
	Token string    `json:"token"`
	User  auth.User `json:"user"`
}

// startSession opens a session for user, sets the session cookie and writes
// the login response. action names the login flow in error logs.
func startSession(w http.ResponseWriter, r *http.Request, cfg config.Config, database *db.DB, action string, user auth.User) {
	token, err := auth.CreateSession(r.Context(), database, user.ID)
	if err != nil {
		serverError(w, r, action+": creating session", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		MaxAge:   int(auth.SessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, loginResponse{Token: token, User: user})
}

func handleDevLogin(cfg config.Config, database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := decodeDevLogin(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		u, err := auth.UpsertUserByEmail(r.Context(), database, body.Email, body.Name)
		if err != nil {
			serverError(w, r, "dev login: upserting user", err)
			return
		}
		startSession(w, r, cfg, database, "dev login", u)
	}
}

// rateLimited rejects over-limit requests with a 429 before next runs,
// keyed on the connection's remote IP.
func rateLimited(limiter *auth.RateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		ok, retryAfter := limiter.Allow(ip, time.Now())
		if !ok {
			secs := max(int((retryAfter+time.Second-1)/time.Second), 1)
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		next(w, r)
	}
}

// currentUserResponse is the body of GET /api/auth.
type currentUserResponse struct {
	User auth.User `json:"user"`
}

func handleCurrentUser(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUserResponse{User: sessionFrom(r).User})
}

func handlePasswordLogin(cfg config.Config, database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if body.Password == "" {
			writeError(w, http.StatusBadRequest, "password is required")
			return
		}

		var ok bool
		switch {
		case cfg.Password != "":
			ok = auth.VerifyPlaintext(body.Password, cfg.Password)
		case cfg.PasswordHash != "":
			var err error
			ok, err = auth.VerifyScrypt(body.Password, cfg.PasswordHash)
			if err != nil {
				serverError(w, r, "password login", err)
				return
			}
		default:
			// config.Load rejects this, but routes also serves Configs
			// built by hand.
			writeError(w, http.StatusInternalServerError, "no password set")
			return
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid password")
			return
		}

		u, err := auth.UpsertLocalUser(r.Context(), database)
		if err != nil {
			serverError(w, r, "password login: upserting user", err)
			return
		}
		startSession(w, r, cfg, database, "password login", u)
	}
}

func handleLogout(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := auth.DeleteSession(r.Context(), database, sessionFrom(r).ID); err != nil {
			serverError(w, r, "logout: deleting session", err)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:   "session",
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
		w.WriteHeader(http.StatusNoContent)
	}
}
