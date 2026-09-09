package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/handoff"
	"futo-notes-server/internal/oidc"
)

// oidcFlowCookie carries the OIDC state and PKCE verifier between start and
// callback. It is never sent to the identity provider.
const oidcFlowCookie = "oidc_flow"

// oidcFlowCookiePath scopes the flow cookie to the OIDC routes.
const oidcFlowCookiePath = "/api/auth/oidc/"

// encodeOIDCFlow packs the flow cookie: the OAuth state, the PKCE verifier,
// and the Login Hand-off ticket this login fulfils, in that order. State and
// verifier are base64url, so dots separate them unambiguously. The ticket is
// empty for a login started in a browser rather than by an app.
func encodeOIDCFlow(state, verifier, ticket string) string {
	return state + "." + verifier + "." + ticket
}

func decodeOIDCFlow(value string) (state, verifier, ticket string, ok bool) {
	parts := strings.SplitN(value, ".", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
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

// loginPath reports whether requestPath is part of the unauthenticated login
// surface mounted for authMode. In oidc mode that is the browser's two OIDC
// routes plus the two Login Hand-off routes an app drives around them.
func loginPath(authMode, requestPath string) bool {
	switch authMode {
	case "password":
		return requestPath == "/api/auth/password/login"
	case "dev":
		return requestPath == "/api/auth/dev/login"
	case "oidc":
		return requestPath == "/api/auth/oidc/start" ||
			requestPath == "/api/auth/oidc/callback" ||
			requestPath == "/api/auth/handoff" ||
			strings.HasPrefix(requestPath, "/api/auth/handoff/")
	}
	return false
}

// requireAuth guards every /api/* route except the login paths for the active
// auth mode. The session cookie is checked before the Authorization header.
func requireAuth(cfg config.Config, database *db.DB, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loginPath(cfg.AuthMode, r.URL.Path) {
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

type devLoginRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

func decodeDevLogin(r *http.Request) (devLoginRequest, string) {
	var body devLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return devLoginRequest{}, "invalid json"
	}
	body.Email = strings.ToLower(strings.TrimSpace(body.Email))
	body.Name = strings.TrimSpace(body.Name)
	if body.Email == "" {
		return devLoginRequest{}, "email is required"
	}
	if body.Name == "" {
		body.Name = strings.SplitN(body.Email, "@", 2)[0]
	}
	if body.Name == "" {
		return devLoginRequest{}, "email is required"
	}
	return body, ""
}

func handleDevLogin(cfg config.Config, database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, validationError := decodeDevLogin(r)
		if validationError != "" {
			writeError(w, http.StatusBadRequest, validationError)
			return
		}

		u, err := auth.UpsertUserByEmail(r.Context(), database, body.Email, body.Name)
		if err != nil {
			serverError(w, r, "dev login: upserting user", err)
			return
		}
		token, err := auth.CreateSession(r.Context(), database, u.ID)
		if err != nil {
			serverError(w, r, "dev login: creating session", err)
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

// handleOIDCStart begins the Authorization Code + PKCE flow: it generates a
// state and PKCE verifier, stashes both — plus any Login Hand-off ticket the
// URL carries — in a short-lived cookie, and redirects to the provider's
// authorization endpoint. A ticket that is not shaped like a minted one is
// dropped, leaving an ordinary browser login.
func handleOIDCStart(cfg config.Config, provider *oidc.Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, verifier := oidc.NewFlow()
		ticket := r.URL.Query().Get(handoffTicketParam)
		if !handoff.ValidTicket(ticket) {
			ticket = ""
		}

		http.SetCookie(w, &http.Cookie{
			Name:     oidcFlowCookie,
			Value:    encodeOIDCFlow(state, verifier, ticket),
			Path:     oidcFlowCookiePath,
			MaxAge:   600,
			HttpOnly: true,
			Secure:   cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, provider.AuthorizeURL(cfg.OIDCClientID, cfg.OIDCRedirectURI, state, verifier), http.StatusFound)
	}
}

// clearOIDCFlowCookie deletes the flow cookie set by handleOIDCStart.
func clearOIDCFlowCookie(w http.ResponseWriter, cfg config.Config) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcFlowCookie,
		Value:    "",
		Path:     oidcFlowCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// handleOIDCCallback completes the flow: validates state against the flow
// cookie, exchanges the code for an access token, fetches userinfo, and
// upserts the user by sub. On success it sets the session cookie exactly
// like the other login modes. A login carrying a Login Hand-off ticket parks
// its session against that ticket and renders the "go back to the app" page;
// otherwise it redirects to /dev (DevUI) or returns the standard
// {user, token} JSON.
func handleOIDCCallback(cfg config.Config, provider *oidc.Provider, database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flowCookie, err := r.Cookie(oidcFlowCookie)
		if err != nil || flowCookie.Value == "" {
			writeError(w, http.StatusBadRequest, "missing oidc flow cookie")
			return
		}
		state, verifier, ticket, ok := decodeOIDCFlow(flowCookie.Value)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid oidc flow cookie")
			return
		}
		if r.URL.Query().Get("state") != state {
			writeError(w, http.StatusBadRequest, "state mismatch")
			return
		}
		clearOIDCFlowCookie(w, cfg)

		code := r.URL.Query().Get("code")
		if code == "" {
			writeError(w, http.StatusBadRequest, "missing code")
			return
		}

		accessToken, err := provider.Exchange(r.Context(), cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.OIDCRedirectURI, code, verifier)
		if err != nil {
			serverError(w, r, "oidc callback: exchanging code", err)
			return
		}
		claims, err := provider.Userinfo(r.Context(), accessToken)
		if err != nil {
			serverError(w, r, "oidc callback: fetching userinfo", err)
			return
		}
		if claims.Sub == "" {
			writeError(w, http.StatusBadGateway, "identity provider did not return sub")
			return
		}
		if claims.Email == "" {
			writeError(w, http.StatusBadGateway, "identity provider did not return email")
			return
		}
		if !cfg.AllowsEmailDomain(claims.Email) {
			// The person is told here. Spend the ticket too, so an app is not
			// left polling a login that is never coming.
			if ticket != "" {
				if err := handoff.Discard(r.Context(), database, ticket); err != nil {
					serverError(w, r, "oidc callback: discarding a refused login hand-off", err)
					return
				}
			}
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "sign-up is not open yet",
				"code":  "signup_not_open",
			})
			return
		}
		name := claims.Name
		if name == "" {
			name = claims.PreferredUsername
		}
		if name == "" {
			name = claims.Email
		}

		u, err := auth.UpsertUserBySub(r.Context(), database, claims.Sub, claims.Email, name)
		if err != nil {
			serverError(w, r, "oidc callback: upserting user", err)
			return
		}
		token, err := auth.CreateSession(r.Context(), database, u.ID)
		if err != nil {
			serverError(w, r, "oidc callback: creating session", err)
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

		if ticket != "" {
			if err := handoff.Fulfil(r.Context(), database, ticket, u.ID); err != nil {
				serverError(w, r, "oidc callback: parking the login hand-off", err)
				return
			}
			writeBrowserDonePage(w, handoffDonePage)
			return
		}
		if cfg.DevUI {
			http.Redirect(w, r, "/dev", http.StatusFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"user": u, "token": token})
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

func handleCurrentUser(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": sessionFrom(r).User})
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
