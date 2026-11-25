package main

import (
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/blobframe"
	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/ids"
	"futo-notes-server/internal/syncapi"
)

type uploadBlobResponse struct {
	Key string `json:"key"`
}

func handleUploadBlob(database *db.DB, store *blobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := readBlobBody(w, r)
		if !ok {
			return
		}

		key, err := blobs.Stage(r.Context(), database, store, syncapi.SessionFrom(r).User.ID, body)
		if err != nil {
			syncapi.ServerError(w, r, "staging blob", err)
			return
		}
		syncapi.WriteJSON(w, http.StatusCreated, uploadBlobResponse{Key: key})
	}
}

// blobKeyFrom rebuilds the storage key from the path. A key naming another
// user, or a malformed one, is reported as absent rather than forbidden.
func blobKeyFrom(r *http.Request) (string, bool) {
	userID := syncapi.SessionFrom(r).User.ID
	key := r.PathValue("userId") + "/" + r.PathValue("blobId")
	return key, ids.IsOwnBlobKey(userID, key)
}

func handleDownloadBlob(store *blobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := blobKeyFrom(r)
		if !ok {
			syncapi.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		// Disk is what the client can actually read: the ledger state decides
		// when bytes may be reclaimed, not whether an owner may fetch them.
		file, err := store.Open(key)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				syncapi.WriteError(w, http.StatusNotFound, "not found")
				return
			}
			syncapi.ServerError(w, r, "opening blob", err)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			syncapi.ServerError(w, r, "stating blob", err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", info.ModTime(), file)
	}
}

func handleDeleteBlob(database *db.DB, store *blobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := blobKeyFrom(r)
		if !ok {
			syncapi.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		deleted, err := blobs.Delete(r.Context(), database, store, syncapi.SessionFrom(r).User.ID, key)
		if err != nil {
			syncapi.ServerError(w, r, "deleting blob", err)
			return
		}
		if !deleted {
			syncapi.WriteError(w, http.StatusConflict, "blob is in use")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleBatchFetchBlobs(store *blobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := syncapi.ReadLimitedBody(w, r, blobframe.MaxRequestBytes, "request too large")
		if !ok {
			return
		}
		keys, err := blobframe.ParseRequest(body)
		if err != nil {
			syncapi.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if err := streamBlobBatch(w, store, syncapi.SessionFrom(r).User.ID, keys); err != nil {
			hlog.FromRequest(r).Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("streaming blob batch")
			// The status line is already out, so there is no error frame to
			// send. Drop the connection and let the client retry a transfer it
			// sees as truncated.
			panic(http.ErrAbortHandler)
		}
	}
}

// streamBlobBatch writes one frame per key, in request order.
func streamBlobBatch(w io.Writer, store *blobs.Store, userID string, keys []string) error {
	enc := blobframe.NewEncoder(w, blobframe.MaxBatchBytes)
	for _, key := range keys {
		if err := writeBlobFrame(enc, store, userID, key); err != nil {
			return err
		}
	}
	return nil
}

func writeBlobFrame(enc *blobframe.Encoder, store *blobs.Store, userID, key string) error {
	if !ids.IsOwnBlobKey(userID, key) {
		return enc.Missing(key)
	}
	file, err := store.Open(key)
	if errors.Is(err, os.ErrNotExist) {
		return enc.Missing(key)
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	_, err = enc.Blob(key, info.Size(), file)
	return err
}
