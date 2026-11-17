package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"unicode/utf8"

	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/objects"
)

// maxBlobBytes caps one blob upload body at 100 MiB. A protocol limit, not
// deployment config.
const maxBlobBytes = 100 << 20

const (
	maxBatchFetchBody = 64 << 10
	maxBatchFetchKeys = 200
	maxBatchKeyChars  = 128
)

// frameStatus is the status byte of a batch download frame.
type frameStatus byte

const (
	frameOK      frameStatus = 0
	frameMissing frameStatus = 1
	frameOmitted frameStatus = 2
)

const frameOverhead = 7 // u16 key length + u8 status + u32 blob length

type uploadBlobResponse struct {
	Key string `json:"key"`
}

func handleUploadBlob(database *db.DB, store *blobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := readBlobBody(w, r)
		if !ok {
			return
		}

		key, err := blobs.Stage(r.Context(), database, store, sessionFrom(r).User.ID, body)
		if err != nil {
			serverError(w, r, "staging blob", err)
			return
		}
		writeJSON(w, http.StatusCreated, uploadBlobResponse{Key: key})
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
			serverError(w, r, "opening blob", err)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			serverError(w, r, "stating blob", err)
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
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		deleted, err := blobs.Delete(r.Context(), database, store, sessionFrom(r).User.ID, key)
		if err != nil {
			serverError(w, r, "deleting blob", err)
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
		body, ok := readLimitedBody(w, r, maxBatchFetchBody, "request too large")
		if !ok {
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
			writeError(w, http.StatusBadRequest, fmt.Sprintf("too many keys (max %d)", maxBatchFetchKeys))
			return
		}
		keys := make([]string, 0, len(request.Keys))
		for _, key := range request.Keys {
			if key == nil {
				writeError(w, http.StatusBadRequest, "keys must be strings")
				return
			}
			if utf8.RuneCountInString(*key) > maxBatchKeyChars {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("key too long (max %d chars)", maxBatchKeyChars))
				return
			}
			keys = append(keys, *key)
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if err := streamBlobBatch(w, store, sessionFrom(r).User.ID, keys); err != nil {
			hlog.FromRequest(r).Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("streaming blob batch")
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
		n, sent, err := writeBlobFrame(w, store, userID, key, shipped, maxBatchBytes-written)
		if err != nil {
			return err
		}
		written += n
		shipped = shipped || sent
	}
	return nil
}

// writeBlobFrame writes key's frame and returns its length on the wire and
// whether it carried the blob. With mayOmit set, a blob whose frame would not
// fit in room is reported as omitted.
func writeBlobFrame(w io.Writer, store *blobs.Store, userID, key string, mayOmit bool, room int) (int, bool, error) {
	fixed := frameOverhead + len(key)
	if !objects.ValidBlobKey(userID, key) {
		return fixed, false, writeFrameHeader(w, key, frameMissing, 0)
	}
	file, err := store.Open(key)
	if errors.Is(err, os.ErrNotExist) {
		return fixed, false, writeFrameHeader(w, key, frameMissing, 0)
	}
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, false, err
	}
	size := info.Size()
	switch {
	case size > maxBlobBytes:
		// A file too large for one upload cannot be described by the frame's
		// u32 length, so it is reported as absent.
		return fixed, false, writeFrameHeader(w, key, frameMissing, 0)
	case mayOmit && fixed+int(size) > room:
		return fixed, false, writeFrameHeader(w, key, frameOmitted, 0)
	}
	if err := writeFrameHeader(w, key, frameOK, uint32(size)); err != nil {
		return 0, false, err
	}
	copied, err := io.Copy(w, file)
	if err != nil {
		return 0, false, err
	}
	if copied != size {
		// The frame's declared length is already on the wire, so a short read
		// desynchronizes the client's parser. Fail instead, which aborts the
		// response.
		return 0, false, fmt.Errorf("blob %s: sent %d of %d bytes", key, copied, size)
	}
	return fixed + int(size), true, nil
}

func writeFrameHeader(w io.Writer, key string, status frameStatus, blobLen uint32) error {
	header := make([]byte, 0, frameOverhead+len(key))
	header = binary.BigEndian.AppendUint16(header, uint16(len(key)))
	header = append(header, key...)
	header = append(header, byte(status))
	header = binary.BigEndian.AppendUint32(header, blobLen)
	_, err := w.Write(header)
	return err
}
