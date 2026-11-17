package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/blobframe"
	"futo-notes-server/internal/httpbody"
	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/ids"
	"futo-notes-server/internal/storagesvc/auth"
	"futo-notes-server/internal/storagesvc/storage"
)

// UploadResponse is the body of a successful POST /blobs.
type UploadResponse struct {
	Key     string `json:"key"`
	Size    int64  `json:"size"`
	Receipt string `json:"receipt"`
}

func (s *Server) uploadBlob(w http.ResponseWriter, r *http.Request) {
	size := r.ContentLength
	switch {
	// Go reports both an absent Content-Length and "Content-Length: 0" as 0.
	case size < 0 || r.Header.Get("Content-Length") == "":
		httputil.WriteError(w, r, http.StatusLengthRequired, "Content-Length required")
		return
	case size == 0:
		httputil.WriteError(w, r, http.StatusBadRequest, "empty body")
		return
	case size > blobframe.MaxBlobBytes:
		httputil.WriteError(w, r, http.StatusRequestEntityTooLarge, "blob too large")
		return
	}

	userID := auth.UserID(r.Context())
	key := userID + "/" + uuid.NewV7().String()
	if err := s.Storage.PutObject(r.Context(), key, r.Body, size); err != nil {
		if errors.Is(err, storage.ErrBody) {
			hlog.FromRequest(r).Warn().Err(err).Msg("upload body ended early")
			httputil.WriteError(w, r, http.StatusBadRequest, "could not read body")
			return
		}
		storageFailure(w, r, err, "upload blob failed")
		return
	}
	receipt, err := s.Signer.Sign(userID, key, size)
	if err != nil {
		// The object is stored but unclaimable; reconciliation deletes it.
		hlog.FromRequest(r).Error().Err(err).Str("key", key).Msg("sign receipt failed")
		httputil.WriteError(w, r, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	s.Metrics.ReceiptSigned(r.Context())
	s.Metrics.AddUploaded(r.Context(), size)
	httputil.WriteJSON(w, r, http.StatusCreated, UploadResponse{Key: key, Size: size, Receipt: receipt})
}

func (s *Server) getBlob(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserID(r.Context())
	blobID := chi.URLParam(r, "blobId")
	// Another user's key is reported as absent, not forbidden, as the
	// self-hosted server does.
	if chi.URLParam(r, "userId") != userID || !ids.IsUUID(blobID) {
		httputil.WriteError(w, r, http.StatusNotFound, "Not Found")
		return
	}
	key := userID + "/" + blobID
	obj, err := s.Storage.GetObject(r.Context(), key, singleRange(r.Header.Get("Range")))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httputil.WriteError(w, r, http.StatusNotFound, "Not Found")
			return
		}
		if rangeErr, ok := errors.AsType[*storage.RangeError](err); ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", rangeErr.Size))
			httputil.WriteError(w, r, http.StatusRequestedRangeNotSatisfiable, "Range Not Satisfiable")
			return
		}
		storageFailure(w, r, err, "get blob failed")
		return
	}
	defer obj.Body.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(obj.Length, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	status := http.StatusOK
	if obj.ContentRange != "" {
		w.Header().Set("Content-Range", obj.ContentRange)
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	n, err := io.Copy(w, obj.Body)
	s.Metrics.AddDownloaded(r.Context(), n)
	if err != nil {
		// The body falls short of its Content-Length, so the client sees a
		// truncated transfer.
		if clientGone(r) {
			hlog.FromRequest(r).Warn().Err(err).Msg("get blob aborted: client disconnected")
		} else {
			hlog.FromRequest(r).Error().Err(err).Str("key", key).Msg("streaming blob failed")
		}
	}
}

var rangePattern = regexp.MustCompile(`^bytes=(?:(\d+)-(\d*)|-(\d+))$`)

// singleRange returns header when it is one well-formed byte range, and ""
// otherwise. A server may ignore Range, and S3-compatible stores disagree on
// multi-range and malformed headers, so those are served whole.
func singleRange(header string) string {
	m := rangePattern.FindStringSubmatch(header)
	if m == nil {
		return ""
	}
	if m[3] != "" {
		if _, err := strconv.ParseInt(m[3], 10, 64); err != nil {
			return ""
		}
		return header
	}
	first, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return ""
	}
	if m[2] != "" {
		last, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || last < first {
			return ""
		}
	}
	return header
}

func (s *Server) batchBlobs(w http.ResponseWriter, r *http.Request) {
	body, ok := readLimitedBody(w, r, blobframe.MaxRequestBytes, "request too large")
	if !ok {
		return
	}
	keys, err := blobframe.ParseRequest(body)
	if err != nil {
		httputil.WriteError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	userID := auth.UserID(r.Context())
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	sent, err := s.streamBatch(r, w, userID, keys)
	s.Metrics.AddDownloaded(r.Context(), sent)
	if err != nil {
		if clientGone(r) {
			hlog.FromRequest(r).Warn().Err(err).Msg("blob batch aborted: client disconnected")
		} else {
			hlog.FromRequest(r).Error().Err(err).Msg("streaming blob batch failed")
		}
		// The status line is already out, so there is no error frame to send.
		// Drop the connection and let the client retry a transfer it sees as
		// truncated.
		panic(http.ErrAbortHandler)
	}
}

// streamBatch writes one frame per key, in request order, and returns the
// blob bytes it sent.
func (s *Server) streamBatch(r *http.Request, w io.Writer, userID string, keys []string) (int64, error) {
	enc := blobframe.NewEncoder(w, blobframe.MaxBatchBytes)
	var sent int64
	for _, key := range keys {
		if !ids.IsOwnBlobKey(userID, key) {
			if err := enc.Missing(key); err != nil {
				return sent, err
			}
			continue
		}
		obj, err := s.Storage.GetObject(r.Context(), key, "")
		if errors.Is(err, storage.ErrNotFound) {
			if err := enc.Missing(key); err != nil {
				return sent, err
			}
			continue
		}
		if err != nil {
			return sent, err
		}
		shipped, err := enc.Blob(key, obj.Length, obj.Body)
		obj.Body.Close()
		if err != nil {
			return sent, err
		}
		if shipped {
			sent += obj.Length
		}
	}
	return sent, nil
}

func readLimitedBody(w http.ResponseWriter, r *http.Request, limit int64, tooLarge string) ([]byte, bool) {
	body, rejection := httpbody.ReadLimited(w, r, limit, tooLarge)
	if rejection != nil {
		httputil.WriteError(w, r, rejection.Status, rejection.Message)
		return nil, false
	}
	return body, true
}
