package main

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"unicode/utf8"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/objects"
)

// maxBlobBytes caps one blob upload body at 100 MiB. A protocol limit, not
// deployment config.
const maxBlobBytes = 104857600

const (
	maxBatchFetchBody = 65536
	maxBatchFetchKeys = 200
	maxBatchKeyChars  = 128
)

const (
	frameOK       = 0
	frameMissing  = 1
	frameOmitted  = 2
	frameOverhead = 7 // u16 key length + u8 status + u32 blob length
)

func handleUploadBlob(database *sql.DB, store *blobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := readBlobBody(w, r)
		if !ok {
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

// blobKeyFrom rebuilds the storage key from the path. A key naming another
// user, or a malformed one, is reported as absent rather than forbidden.
func blobKeyFrom(r *http.Request) (string, bool) {
	userID := sessionFrom(r).User.ID
	key := r.PathValue("userId") + "/" + r.PathValue("blobId")
	return key, objects.ValidBlobKey(userID, key)
}

func handleDownloadBlob(store *blobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := blobKeyFrom(r)
		if !ok {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		// Disk is what the client can actually read: the ledger state decides
		// when bytes may be reclaimed, not whether an owner may fetch them.
		file, err := store.Open(key)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, "not found")
				return
			}
			log.Printf("opening blob: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			log.Printf("stating blob: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", info.ModTime(), file)
	}
}

func handleDeleteBlob(database *sql.DB, store *blobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := blobKeyFrom(r)
		if !ok {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		deleted, err := blobs.Delete(r.Context(), database, store, sessionFrom(r).User.ID, key)
		if err != nil {
			log.Printf("deleting blob: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if !deleted {
			writeError(w, http.StatusConflict, "blob is in use")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleBatchFetchBlobs(store *blobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBatchFetchBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "request too large")
			} else {
				writeError(w, http.StatusBadRequest, "could not read body")
			}
			return
		}
		// Pointers, so that a JSON null is rejected rather than silently
		// decoded as the empty string.
		var request struct {
			Keys []*string `json:"keys"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if len(request.Keys) == 0 {
			writeError(w, http.StatusBadRequest, "keys must not be empty")
			return
		}
		if len(request.Keys) > maxBatchFetchKeys {
			writeError(w, http.StatusBadRequest, "too many keys (max 200)")
			return
		}
		keys := make([]string, 0, len(request.Keys))
		for _, key := range request.Keys {
			if key == nil {
				writeError(w, http.StatusBadRequest, "keys must be strings")
				return
			}
			if utf8.RuneCountInString(*key) > maxBatchKeyChars {
				writeError(w, http.StatusBadRequest, "key too long (max 128 chars)")
				return
			}
			keys = append(keys, *key)
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if err := streamBlobBatch(w, store, sessionFrom(r).User.ID, keys); err != nil {
			log.Printf("streaming blob batch: %v", err)
			// The status line is already out, so there is no error frame to
			// send. Drop the connection and let the client retry a transfer it
			// sees as truncated.
			panic(http.ErrAbortHandler)
		}
	}
}

// streamBlobBatch writes one frame per key, in request order, stopping the
// payload from growing past maxBatchBytes. The first blob available always
// ships, so a client whose next blob is larger than the whole cap still makes
// progress.
func streamBlobBatch(w io.Writer, store *blobs.Store, userID string, keys []string) error {
	written, shipped := 0, false
	for _, key := range keys {
		var size int64
		var file *os.File
		if objects.ValidBlobKey(userID, key) {
			opened, err := store.Open(key)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil {
				info, err := opened.Stat()
				if err != nil {
					opened.Close()
					return err
				}
				// A file too large for one upload cannot be described by the
				// frame's u32 length, so it is reported as absent.
				if info.Size() <= maxBlobBytes {
					size, file = info.Size(), opened
				} else {
					opened.Close()
				}
			}
		}

		fixed := frameOverhead + len(key)
		switch {
		case file == nil:
			if err := writeFrameHeader(w, key, frameMissing, 0); err != nil {
				return err
			}
			written += fixed
		case shipped && written+fixed+int(size) > maxBatchBytes:
			file.Close()
			if err := writeFrameHeader(w, key, frameOmitted, 0); err != nil {
				return err
			}
			written += fixed
		default:
			err := writeFrameHeader(w, key, frameOK, uint32(size))
			var copied int64
			if err == nil {
				copied, err = io.Copy(w, file)
			}
			file.Close()
			if err != nil {
				return err
			}
			if copied != size {
				// The frame's declared length is already on the wire, so a
				// short read desynchronizes the client's parser. Fail instead,
				// which aborts the response.
				return fmt.Errorf("blob %s: sent %d of %d bytes", key, copied, size)
			}
			written += fixed + int(size)
			shipped = true
		}
	}
	return nil
}

func writeFrameHeader(w io.Writer, key string, status byte, blobLen uint32) error {
	header := make([]byte, 0, frameOverhead+len(key))
	header = binary.BigEndian.AppendUint16(header, uint16(len(key)))
	header = append(header, key...)
	header = append(header, status)
	header = binary.BigEndian.AppendUint32(header, blobLen)
	_, err := w.Write(header)
	return err
}
