package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

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

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	database, err := db.Open(cfg)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleCapability(cfg.AuthMode))
	mux.HandleFunc("GET /health", handleHealth(database))

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
