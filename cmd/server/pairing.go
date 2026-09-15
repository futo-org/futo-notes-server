package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/pairing"
)

const (
	// maxPairingBody bounds both pairing request bodies. A one-time public
	// key and a sealed 32-byte vault key are a couple of hundred bytes; this
	// is generous for a client that frames them differently and still small
	// enough that no single call can write an unbounded row. It bounds one
	// request and nothing more — these routes are deliberately outside the
	// Entitlement gate and the Storage Quota, and how many pairings an
	// account may open is not capped at all.
	maxPairingBody = 8 << 10

	// maxPairingField bounds each string a pairing stores, for the same
	// reason at field granularity.
	maxPairingField = 1024
)

// readPairingBody reads a bounded request body into v. It answers the client
// itself and reports whether the handler should carry on.
func readPairingBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPairingBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request too large")
		} else {
			writeError(w, http.StatusBadRequest, "could not read body")
		}
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

// requiredPairingField validates one opaque string a pairing carries. Being
// opaque, its only rules are that it is there and that it is not enormous.
func requiredPairingField(w http.ResponseWriter, value, name string) bool {
	if value == "" {
		writeError(w, http.StatusBadRequest, name+" is required")
		return false
	}
	if len(value) > maxPairingField {
		writeError(w, http.StatusBadRequest, name+" is too long")
		return false
	}
	return true
}

// handleCreatePairing opens a Device Pairing for the signed-in account: the
// new device publishes the one-time public key an unlocked device will seal
// the vault key to, and says what to call it on the confirmation that device
// shows.
func handleCreatePairing(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PublicKey  string `json:"public_key"`
			DeviceName string `json:"device_name"`
			Platform   string `json:"platform"`
		}
		if !readPairingBody(w, r, &body) {
			return
		}
		if !requiredPairingField(w, body.PublicKey, "public_key") ||
			!requiredPairingField(w, body.DeviceName, "device_name") ||
			!requiredPairingField(w, body.Platform, "platform") {
			return
		}

		created, err := pairing.Create(r.Context(), database, sessionFrom(r).User.ID, pairing.Offer{
			PublicKey:  body.PublicKey,
			DeviceName: body.DeviceName,
			Platform:   body.Platform,
		})
		if err != nil {
			serverError(w, r, "creating a device pairing", err)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	}
}

// handleDeliverPairingKey takes the vault key an unlocked device sealed to
// the pairing's public key. The server cannot open it and never tries.
//
// A pairing that is unknown, expired, already collected, or another account's
// is one 404, so posting cannot be used to discover that a pairing id exists.
// A second post to the caller's own pairing is a 409, which tells the owner
// what happened without telling a stranger anything: only the owning account
// can reach it.
func handleDeliverPairingKey(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Ciphertext string `json:"ciphertext"`
		}
		if !readPairingBody(w, r, &body) {
			return
		}
		if !requiredPairingField(w, body.Ciphertext, "ciphertext") {
			return
		}

		code, err := pairing.Deliver(r.Context(), database,
			sessionFrom(r).User.ID, r.PathValue("id"), body.Ciphertext)
		if err != nil {
			// The ciphertext is never part of what is logged here:
			// serverError logs the error, the method, and the path.
			serverError(w, r, "delivering a device pairing key", err)
			return
		}
		switch code {
		case pairing.DeliverOK:
			w.WriteHeader(http.StatusNoContent)
		case pairing.DeliverTaken:
			writeError(w, http.StatusConflict, "pairing already has a key")
		default:
			writeError(w, http.StatusNotFound, "not found")
		}
	}
}

// handleCollectPairing answers the new device polling its pairing: 202 while
// the unlocked device has not posted yet, the sealed vault key exactly once
// after that, and 404 for a pairing that is unknown, already collected,
// expired, or another account's.
func handleCollectPairing(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		collected, err := pairing.Collect(r.Context(), database,
			sessionFrom(r).User.ID, r.PathValue("id"))
		if err != nil {
			serverError(w, r, "collecting a device pairing key", err)
			return
		}
		switch collected.Code {
		case pairing.CollectPending:
			w.WriteHeader(http.StatusAccepted)
		case pairing.CollectOK:
			writeJSON(w, http.StatusOK, map[string]string{"ciphertext": collected.Ciphertext})
		default:
			writeError(w, http.StatusNotFound, "not found")
		}
	}
}
