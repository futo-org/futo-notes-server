package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"

	"futo-notes-server/internal/collections"
)

func handleClaimCollection(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, created, err := collections.Claim(r.Context(), database, sessionFrom(r).User.ID)
		if err != nil {
			log.Printf("claiming collection: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, map[string]any{"collection": c})
	}
}

func handleListCollections(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cs, err := collections.List(r.Context(), database, sessionFrom(r).User.ID)
		if err != nil {
			log.Printf("listing collections: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"collections": cs})
	}
}

func handleGetCollection(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := collections.Get(r.Context(), database, sessionFrom(r).User.ID, r.PathValue("id"))
		if err != nil {
			log.Printf("getting collection: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if c == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"collection": c})
	}
}

func handleDeleteCollection(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		found, err := collections.Delete(r.Context(), database, sessionFrom(r).User.ID, r.PathValue("id"))
		if err != nil {
			log.Printf("deleting collection: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleGetCollectionKey(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		found, key, err := collections.GetKey(r.Context(), database, sessionFrom(r).User.ID, r.PathValue("id"))
		if err != nil {
			log.Printf("getting collection key: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"key": key})
	}
}

// isJSONObject reports whether raw (already known to be well-formed JSON)
// is an object rather than an array, string, number, bool, or null.
func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func handlePutCollectionKey(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			KeySalt              *string         `json:"key_salt"`
			KeyKDF               json.RawMessage `json:"key_kdf"`
			EncryptedVaultKey    *string         `json:"encrypted_vault_key"`
			PreviousKeyUpdatedAt *string         `json:"previous_key_updated_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if body.KeySalt == nil || *body.KeySalt == "" {
			writeError(w, http.StatusBadRequest, "key_salt is required")
			return
		}
		if body.EncryptedVaultKey == nil || *body.EncryptedVaultKey == "" {
			writeError(w, http.StatusBadRequest, "encrypted_vault_key is required")
			return
		}
		if body.KeyKDF == nil || !isJSONObject(body.KeyKDF) {
			writeError(w, http.StatusBadRequest, "key_kdf must be an object")
			return
		}
		if body.PreviousKeyUpdatedAt != nil && *body.PreviousKeyUpdatedAt == "" {
			writeError(w, http.StatusBadRequest, "previous_key_updated_at must be non-empty when present")
			return
		}

		in := collections.KeyInput{
			KeySalt:           *body.KeySalt,
			KeyKDF:            body.KeyKDF,
			EncryptedVaultKey: *body.EncryptedVaultKey,
		}
		outcome, key, err := collections.PutKey(r.Context(), database,
			sessionFrom(r).User.ID, r.PathValue("id"), in, body.PreviousKeyUpdatedAt)
		if err != nil {
			log.Printf("putting collection key: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		switch outcome {
		case collections.PutKeyNotFound:
			writeError(w, http.StatusNotFound, "not found")
		case collections.PutKeyNoCurrentKey:
			writeError(w, http.StatusBadRequest, "no existing key to rotate")
		case collections.PutKeyConflict:
			writeJSON(w, http.StatusConflict, map[string]any{"error": "key conflict", "currentKey": key})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"key": key})
		}
	}
}
