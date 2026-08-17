package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
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

func handleHealth(w http.ResponseWriter, r *http.Request) {
	// No database yet: report connected unconditionally until Postgres
	// support lands, then this becomes a real ping with a 503 branch.
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		DB     string `json:"db"`
	}{Status: "ok", DB: "connected"})
}

func main() {
	authMode := os.Getenv("AUTH_MODE")
	if authMode == "" {
		authMode = "password"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleCapability(authMode))
	mux.HandleFunc("GET /health", handleHealth)

	addr := ":3005"
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
