package main

import (
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"

	"futo-notes-server/internal/blobs"
)

// maxBlobBytes caps one blob upload body at 100 MiB. A protocol limit, not
// deployment config.
const maxBlobBytes = 104857600

func handleUploadBlob(database *sql.DB, store *blobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBlobBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "blob too large")
				return
			}
			writeError(w, http.StatusBadRequest, "could not read body")
			return
		}
		if len(body) == 0 {
			writeError(w, http.StatusBadRequest, "empty body")
			return
		}

		key, err := blobs.Stage(r.Context(), database, store, sessionFrom(r).User.ID, body)
		if err != nil {
			log.Printf("staging blob: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"key": key})
	}
}
