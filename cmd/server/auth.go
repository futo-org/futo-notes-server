package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/config"
)

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

// requireAuth guards every /api/* route except the login path for the active
// auth mode. The session cookie is checked before the Authorization header.
func requireAuth(cfg config.Config, database *sql.DB, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.AuthMode == "password" && r.URL.Path == "/api/auth/password/login" {
			next.ServeHTTP(w, r)
			return
		}

		var token string
		if cookie, err := r.Cookie("session"); err == nil {
			token = cookie.Value
		} else if header := r.Header.Get("Authorization"); header != "" {
			token = bearerToken(header)
		}
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		session, err := auth.ValidateSession(r.Context(), database, token)
		if err != nil {
			serverError(w, r, "validating session", err)
			return
		}
		if session == nil {
			// Distinguish a dead bearer session from a request that supplied
			// no credentials, so clients re-login without treating this as a
			// password change or wiping their sync cursor.
			w.Header().Set("WWW-Authenticate", `Bearer realm="futo-notes", error="invalid_token"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "session expired or invalid",
				"code":  "invalid_session",
			})
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, session)))
	})
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

func handleCurrentUser(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": sessionFrom(r).User})
}

func handlePasswordLogin(cfg config.Config, database *sql.DB) http.HandlerFunc {
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
		if cfg.Password == "" && cfg.PasswordHash == "" {
			writeError(w, http.StatusInternalServerError, "no password set")
			return
		}

		var ok bool
		if cfg.Password != "" {
			ok = auth.VerifyPlaintext(body.Password, cfg.Password)
		} else {
			var err error
			ok, err = auth.VerifyScrypt(body.Password, cfg.PasswordHash)
			if err != nil {
				serverError(w, r, "password login", err)
				return
			}
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

		token, err := auth.CreateSession(r.Context(), database, u.ID)
		if err != nil {
			serverError(w, r, "password login: creating session", err)
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
		writeJSON(w, http.StatusOK, map[string]any{"user": u, "token": token})
	}
}

func handleLogout(cfg config.Config, database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := auth.DeleteSession(r.Context(), database, sessionFrom(r).ID); err != nil {
			serverError(w, r, "logout: deleting session", err)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
		})
		w.WriteHeader(http.StatusNoContent)
	}
}
