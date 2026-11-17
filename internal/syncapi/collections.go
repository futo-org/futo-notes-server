package syncapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"futo-notes-server/internal/collections"
	"futo-notes-server/internal/db"
)

// collectionResponse is the body of the claim and get collection routes.
type collectionResponse struct {
	Collection collections.Collection `json:"collection"`
}

// collectionsResponse is the body of the list collections route.
type collectionsResponse struct {
	Collections []collections.Collection `json:"collections"`
}

// keyResponse is the body of the key routes; Key is null until a key is
// claimed.
type keyResponse struct {
	Key *collections.KeyMaterial `json:"key"`
}

// keyConflictResponse is the 409 body for a stale rotation. Field order
// matches the map it replaced, whose keys marshaled sorted.
type keyConflictResponse struct {
	CurrentKey collections.KeyMaterial `json:"currentKey"`
	Error      string                  `json:"error"`
}

// HandleClaimCollection serves POST /api/collections.
func HandleClaimCollection(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, created, err := collections.Claim(r.Context(), database, SessionFrom(r).User.ID)
		if err != nil {
			ServerError(w, r, "claiming collection", err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		WriteJSON(w, status, collectionResponse{Collection: c})
	}
}

// HandleListCollections serves GET /api/collections.
func HandleListCollections(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cs, err := collections.List(r.Context(), database, SessionFrom(r).User.ID)
		if err != nil {
			ServerError(w, r, "listing collections", err)
			return
		}
		WriteJSON(w, http.StatusOK, collectionsResponse{Collections: cs})
	}
}

// HandleGetCollection serves GET /api/collections/{id}.
func HandleGetCollection(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := collections.Get(r.Context(), database, SessionFrom(r).User.ID, r.PathValue("id"))
		if errors.Is(err, collections.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "not found")
			return
		}
		if err != nil {
			ServerError(w, r, "getting collection", err)
			return
		}
		WriteJSON(w, http.StatusOK, collectionResponse{Collection: c})
	}
}

// HandleDeleteCollection serves DELETE /api/collections/{id}.
func HandleDeleteCollection(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := collections.Delete(r.Context(), database, SessionFrom(r).User.ID, r.PathValue("id"))
		if errors.Is(err, collections.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "not found")
			return
		}
		if err != nil {
			ServerError(w, r, "deleting collection", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// HandleGetCollectionKey serves GET /api/collections/{id}/key.
func HandleGetCollectionKey(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, claimed, err := collections.GetKey(r.Context(), database, SessionFrom(r).User.ID, r.PathValue("id"))
		if errors.Is(err, collections.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "not found")
			return
		}
		if err != nil {
			ServerError(w, r, "getting collection key", err)
			return
		}
		var resp keyResponse
		if claimed {
			resp.Key = &key
		}
		WriteJSON(w, http.StatusOK, resp)
	}
}

// isJSONObject reports whether raw (already known to be well-formed JSON)
// is an object rather than an array, string, number, bool, or null.
func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// HandlePutCollectionKey serves PUT /api/collections/{id}/key: a first claim,
// or a rotation when previous_key_updated_at is present.
func HandlePutCollectionKey(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			KeySalt              *string         `json:"key_salt"`
			KeyKDF               json.RawMessage `json:"key_kdf"`
			EncryptedVaultKey    *string         `json:"encrypted_vault_key"`
			PreviousKeyUpdatedAt *string         `json:"previous_key_updated_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if body.KeySalt == nil || *body.KeySalt == "" {
			WriteError(w, http.StatusBadRequest, "key_salt is required")
			return
		}
		if body.EncryptedVaultKey == nil || *body.EncryptedVaultKey == "" {
			WriteError(w, http.StatusBadRequest, "encrypted_vault_key is required")
			return
		}
		if body.KeyKDF == nil || !isJSONObject(body.KeyKDF) {
			WriteError(w, http.StatusBadRequest, "key_kdf must be an object")
			return
		}
		if body.PreviousKeyUpdatedAt != nil && *body.PreviousKeyUpdatedAt == "" {
			WriteError(w, http.StatusBadRequest, "previous_key_updated_at must be non-empty when present")
			return
		}

		in := collections.KeyInput{
			KeySalt:           *body.KeySalt,
			KeyKDF:            body.KeyKDF,
			EncryptedVaultKey: *body.EncryptedVaultKey,
		}
		key, err := collections.PutKey(r.Context(), database,
			SessionFrom(r).User.ID, r.PathValue("id"), in, body.PreviousKeyUpdatedAt)
		if conflict, ok := errors.AsType[*collections.KeyConflictError](err); ok {
			WriteJSON(w, http.StatusConflict, keyConflictResponse{CurrentKey: conflict.Current, Error: "key conflict"})
			return
		}
		switch {
		case errors.Is(err, collections.ErrNotFound):
			WriteError(w, http.StatusNotFound, "not found")
		case errors.Is(err, collections.ErrNoCurrentKey):
			WriteError(w, http.StatusBadRequest, "no existing key to rotate")
		case err != nil:
			ServerError(w, r, "putting collection key", err)
		default:
			WriteJSON(w, http.StatusOK, keyResponse{Key: &key})
		}
	}
}
