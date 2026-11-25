package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"futo-notes-server/internal/hosted/pairing"
	"futo-notes-server/internal/httputil"
)

const (
	// maxPairingBody bounds a pairing request body.
	maxPairingBody = 8 << 10
	// maxPairingField bounds each string a pairing stores, in characters.
	maxPairingField = 1024
)

// pairingRoutes mounts the pairing relay. A pending deletion blocks it like
// the sync routes.
func (s *Server) pairingRoutes(r chi.Router) {
	r.Use(s.authenticate(hostedErrors), requireActiveAccount)
	r.Post("/pairings", s.handleCreatePairing)
	r.Post("/pairings/{id}/key", s.handleDeliverPairingKey)
	r.Get("/pairings/{id}", s.handleCollectPairing)
}

// pairingField is a string field of a pairing body and where it goes.
type pairingField struct {
	name  string
	value *string
}

// decodePairingBody reads a pairing body into fields, answering 413 over
// maxPairingBody and 400 when it is not JSON or a field is missing, empty or
// over maxPairingField characters. Error messages never quote a value: one
// may be a sealed vault key.
func decodePairingBody(w http.ResponseWriter, r *http.Request, fields ...pairingField) bool {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPairingBody)).Decode(&body); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			httputil.WriteError(w, r, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
				fmt.Sprintf("Request body over %d bytes", maxPairingBody))
			return false
		}
		badRequest(w, r, "Invalid JSON body")
		return false
	}
	for _, field := range fields {
		raw, ok := body[field.name]
		if !ok || json.Unmarshal(raw, field.value) != nil || *field.value == "" {
			badRequest(w, r, field.name+" must be a non-empty string")
			return false
		}
		if utf8.RuneCountInString(*field.value) > maxPairingField {
			badRequest(w, r, fmt.Sprintf("%s is over %d characters", field.name, maxPairingField))
			return false
		}
	}
	return true
}

// writePairingNotFound answers every pairing the caller cannot reach alike,
// and alike with an unknown route, so a pairing id cannot be probed.
func writePairingNotFound(w http.ResponseWriter, r *http.Request) {
	httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, "Not found")
}

type createdPairing struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleCreatePairing serves POST /api/pairings: the new device opens a
// pairing with its one-time public key.
func (s *Server) handleCreatePairing(w http.ResponseWriter, r *http.Request) {
	var offer pairing.Offer
	if !decodePairingBody(w, r, pairingField{"public_key", &offer.PublicKey},
		pairingField{"device_name", &offer.DeviceName}, pairingField{"platform", &offer.Platform}) {
		return
	}
	id, expiresAt, err := pairing.Create(r.Context(), s.database, principalFrom(r).Session.User.ID, offer)
	if err != nil {
		serverError(w, r, "creating pairing", err)
		return
	}
	httputil.WriteJSON(w, r, http.StatusCreated, createdPairing{ID: id, ExpiresAt: expiresAt})
}

// handleDeliverPairingKey serves POST /api/pairings/{id}/key: the unlocked
// device posts the vault key sealed to the pairing's public key.
func (s *Server) handleDeliverPairingKey(w http.ResponseWriter, r *http.Request) {
	var ciphertext string
	if !decodePairingBody(w, r, pairingField{"ciphertext", &ciphertext}) {
		return
	}
	err := pairing.Deliver(r.Context(), s.database, principalFrom(r).Session.User.ID, chi.URLParam(r, "id"), ciphertext)
	switch {
	case errors.Is(err, pairing.ErrNotFound):
		writePairingNotFound(w, r)
	case errors.Is(err, pairing.ErrAlreadyKeyed):
		httputil.WriteError(w, r, http.StatusConflict, codePairingAlreadyKeyed, "A key was already posted to this pairing")
	case err != nil:
		serverError(w, r, "delivering pairing key", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleCollectPairing serves GET /api/pairings/{id}: the new device polls
// until the key arrives, and collecting it ends the pairing.
func (s *Server) handleCollectPairing(w http.ResponseWriter, r *http.Request) {
	ciphertext, delivered, err := pairing.Collect(r.Context(), s.database, principalFrom(r).Session.User.ID, chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, pairing.ErrNotFound):
		writePairingNotFound(w, r)
	case err != nil:
		serverError(w, r, "collecting pairing", err)
	case !delivered:
		w.WriteHeader(http.StatusAccepted)
	default:
		w.Header().Set("Cache-Control", "no-store")
		httputil.WriteJSON(w, r, http.StatusOK, struct {
			Ciphertext string `json:"ciphertext"`
		}{ciphertext})
	}
}
