package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/uuidv7"
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

// The singleton user in password mode. All E2EE data is owned by this row.
const (
	singletonSub   = "local"
	singletonEmail = "local@futo-notes.local"
	singletonName  = "FUTO Notes"
)

type user struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// upsertLocalUser returns the singleton user, creating it on first login.
// ON CONFLICT DO NOTHING plus a re-select keeps two concurrent first logins
// from failing on the sub unique constraint.
func upsertLocalUser(ctx context.Context, database *sql.DB) (user, error) {
	var u user
	err := database.QueryRowContext(ctx,
		`SELECT id, email, name FROM users WHERE sub = $1`, singletonSub).
		Scan(&u.ID, &u.Email, &u.Name)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return user{}, err
	}

	err = database.QueryRowContext(ctx,
		`INSERT INTO users (id, sub, email, name) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (sub) DO NOTHING
		 RETURNING id, email, name`,
		uuidv7.New(), singletonSub, singletonEmail, singletonName).
		Scan(&u.ID, &u.Email, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		// Lost the race; the row exists now.
		err = database.QueryRowContext(ctx,
			`SELECT id, email, name FROM users WHERE sub = $1`, singletonSub).
			Scan(&u.ID, &u.Email, &u.Name)
	}
	if err != nil {
		return user{}, err
	}
	return u, nil
}

// createSession opens a session for the user and returns the raw token the
// client authenticates with. Only the token's SHA-256 hash is stored.
func createSession(ctx context.Context, database *sql.DB, userID string) (string, error) {
	rawToken := auth.GenerateToken()
	_, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, access_token_hash, expires_at) VALUES ($1, $2, $3, $4)`,
		uuidv7.New(), userID, auth.HashToken(rawToken), time.Now().Add(auth.SessionTTL))
	if err != nil {
		return "", err
	}
	return rawToken, nil
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

		u, err := upsertLocalUser(r.Context(), database)
		if err != nil {
			log.Printf("password login: upserting user: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		rawToken, err := createSession(r.Context(), database, u.ID)
		if err != nil {
			log.Printf("password login: creating session: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    rawToken,
			Path:     "/",
			MaxAge:   int(auth.SessionTTL.Seconds()),
			HttpOnly: true,
			Secure:   cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
		})
		writeJSON(w, http.StatusOK, map[string]any{"user": u, "token": rawToken})
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleCapability(cfg.AuthMode))
	mux.HandleFunc("GET /health", handleHealth(database))
	if cfg.AuthMode == "password" {
		mux.HandleFunc("POST /api/auth/password/login", handlePasswordLogin(cfg, database))
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
