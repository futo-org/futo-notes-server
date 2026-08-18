package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
)

const (
	serverName    = "futo-notes"
	serverVersion = "0.1.0"
)

// capability is the discovery document served at GET /. Clients call it once
// when adding a server, to learn which login flow to drive and whether
// retry-safe mutations exist.
type capability struct {
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	AuthMode    string      `json:"auth_mode"`
	Signup      string      `json:"signup"`
	Billing     bool        `json:"billing"`
	MutationIDs mutationIDs `json:"mutation_ids"`
}

type mutationIDs struct {
	Supported                bool   `json:"supported"`
	Required                 bool   `json:"required"`
	RetentionDays            int    `json:"retention_days"`
	SuccessfulCreateOutcomes string `json:"successful_create_outcomes"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

func handleCapability(authMode string) http.HandlerFunc {
	cap := capability{
		Name:     serverName,
		Version:  serverVersion,
		AuthMode: authMode,
		Signup:   "closed",
		Billing:  false,
		MutationIDs: mutationIDs{
			Supported:                true,
			Required:                 false,
			RetentionDays:            30,
			SuccessfulCreateOutcomes: "durable",
		},
	}
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, cap)
	}
}

type healthStatus struct {
	Status string `json:"status"`
	DB     string `json:"db"`
}

func handleHealth(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := database.PingContext(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, healthStatus{Status: "degraded", DB: "unreachable"})
			return
		}
		writeJSON(w, http.StatusOK, healthStatus{Status: "ok", DB: "connected"})
	}
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
			log.Printf("validating session: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
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
				log.Printf("password login: %v", err)
				writeError(w, http.StatusInternalServerError, "internal server error")
				return
			}
		}

		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid password")
			return
		}

		u, err := auth.UpsertLocalUser(r.Context(), database)
		if err != nil {
			log.Printf("password login: upserting user: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		token, err := auth.CreateSession(r.Context(), database, u.ID)
		if err != nil {
			log.Printf("password login: creating session: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
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
			log.Printf("logout: deleting session: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
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

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	database, err := db.Open(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	applied, err := db.Migrate(ctx, database)
	cancel()
	if err != nil {
		log.Fatal(err)
	}
	if len(applied) > 0 {
		log.Printf("applied %d migration(s): %s", len(applied), strings.Join(applied, ", "))
	}

	api := http.NewServeMux()
	if cfg.AuthMode == "password" {
		api.HandleFunc("POST /api/auth/password/login", handlePasswordLogin(cfg, database))
	}
	api.HandleFunc("GET /api/auth", handleCurrentUser)
	api.HandleFunc("POST /api/auth/logout", handleLogout(cfg, database))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleCapability(cfg.AuthMode))
	mux.HandleFunc("GET /health", handleHealth(database))
	mux.Handle("/api/", requireAuth(cfg, database, api))
	if cfg.DevUI {
		mux.HandleFunc("GET /dev", handleDevUI)
		log.Print("dev UI enabled at /dev")
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
