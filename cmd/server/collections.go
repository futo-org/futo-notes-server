package main

import (
	"bytes"
	"encoding/json"
	"net/http"

	"futo-notes-server/internal/collections"
	"futo-notes-server/internal/db"
)

func handleClaimCollection(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, created, err := collections.Claim(r.Context(), database, sessionFrom(r).User.ID)
		if err != nil {
			serverError(w, r, "claiming collection", err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, map[string]any{"collection": c})
	}
}

func handleListCollections(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cs, err := collections.List(r.Context(), database, sessionFrom(r).User.ID)
		if err != nil {
			serverError(w, r, "listing collections", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"collections": cs})
	}
}

func handleGetCollection(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := collections.Get(r.Context(), database, sessionFrom(r).User.ID, r.PathValue("id"))
		if err != nil {
			serverError(w, r, "getting collection", err)
			return
		}
		if c == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"collection": c})
	}
}

func handleDeleteCollection(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		found, err := collections.Delete(r.Context(), database, sessionFrom(r).User.ID, r.PathValue("id"))
		if err != nil {
			serverError(w, r, "deleting collection", err)
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleGetCollectionKey(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		found, key, err := collections.GetKey(r.Context(), database, sessionFrom(r).User.ID, r.PathValue("id"))
		if err != nil {
			serverError(w, r, "getting collection key", err)
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

// omittedJSON reports whether a raw field was left out or sent as null, which
// mean the same thing on the way in: the client is not supplying that value.
func omittedJSON(raw json.RawMessage) bool {
	return raw == nil || string(bytes.TrimSpace(raw)) == "null"
}

func handlePutCollectionKey(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			KeySalt              *string         `json:"key_salt"`
			KeyKDF               json.RawMessage `json:"key_kdf"`
			EncryptedVaultKey    *string         `json:"encrypted_vault_key"`
			PreviousKeyUpdatedAt *string         `json:"previous_key_updated_at"`

			RecoveryKeySalt           *string         `json:"recovery_key_salt"`
			RecoveryKeyKDF            json.RawMessage `json:"recovery_key_kdf"`
			RecoveryEncryptedVaultKey *string         `json:"recovery_encrypted_vault_key"`
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

		// The recovery envelope is optional, but it is one envelope: a stored
		// half would be a recovery door that cannot be opened. Either all three
		// fields arrive or none do, and omitting them clears any stored
		// envelope, because the material a client sends replaces the material
		// that was there.
		haveSalt := body.RecoveryKeySalt != nil
		haveKDF := !omittedJSON(body.RecoveryKeyKDF)
		haveVaultKey := body.RecoveryEncryptedVaultKey != nil
		hasRecovery := haveSalt && haveKDF && haveVaultKey
		if !hasRecovery && (haveSalt || haveKDF || haveVaultKey) {
			writeError(w, http.StatusBadRequest,
				"recovery_key_salt, recovery_key_kdf, and recovery_encrypted_vault_key must be sent together")
			return
		}
		if hasRecovery {
			if *body.RecoveryKeySalt == "" {
				writeError(w, http.StatusBadRequest, "recovery_key_salt must be non-empty when present")
				return
			}
			if *body.RecoveryEncryptedVaultKey == "" {
				writeError(w, http.StatusBadRequest, "recovery_encrypted_vault_key must be non-empty when present")
				return
			}
			if !isJSONObject(body.RecoveryKeyKDF) {
				writeError(w, http.StatusBadRequest, "recovery_key_kdf must be an object")
				return
			}
		}

		in := collections.KeyInput{
			KeySalt:           *body.KeySalt,
			KeyKDF:            body.KeyKDF,
			EncryptedVaultKey: *body.EncryptedVaultKey,
		}
		if hasRecovery {
			in.RecoveryKeySalt = body.RecoveryKeySalt
			in.RecoveryKeyKDF = body.RecoveryKeyKDF
			in.RecoveryEncryptedVaultKey = body.RecoveryEncryptedVaultKey
		}
		outcome, key, err := collections.PutKey(r.Context(), database,
			sessionFrom(r).User.ID, r.PathValue("id"), in, body.PreviousKeyUpdatedAt)
		if err != nil {
			serverError(w, r, "putting collection key", err)
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
