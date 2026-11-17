package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/ids"
	"futo-notes-server/internal/storagesvc/httputil"
	"futo-notes-server/internal/storagesvc/storage"
)

// maxDeleteBody fits storage.MaxDeleteKeys blob keys with room for JSON.
const maxDeleteBody = 128 << 10

// DeleteBlobsResponse is the body of DELETE /internal/blobs.
type DeleteBlobsResponse struct {
	// Deleted counts the keys that are now absent, including ones that were
	// already gone.
	Deleted int           `json:"deleted"`
	Failed  []DeleteError `json:"failed"`
}

// DeleteError is a key DELETE /internal/blobs could not remove.
type DeleteError struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

func (s *Server) deleteBlobs(w http.ResponseWriter, r *http.Request) {
	body, ok := readLimitedBody(w, r, maxDeleteBody, "request too large")
	if !ok {
		return
	}
	var request struct {
		Keys []*string `json:"keys"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		httputil.WriteError(w, r, http.StatusBadRequest, "invalid json")
		return
	}
	if len(request.Keys) == 0 {
		httputil.WriteError(w, r, http.StatusBadRequest, "keys must not be empty")
		return
	}
	if len(request.Keys) > storage.MaxDeleteKeys {
		httputil.WriteError(w, r, http.StatusBadRequest, fmt.Sprintf("too many keys (max %d)", storage.MaxDeleteKeys))
		return
	}
	keys := make([]string, 0, len(request.Keys))
	for _, key := range request.Keys {
		if key == nil || !ids.IsBlobKey(*key) {
			httputil.WriteError(w, r, http.StatusBadRequest, "keys must be <userId>/<blobId>")
			return
		}
		keys = append(keys, *key)
	}

	failed, err := s.Storage.DeleteObjects(r.Context(), keys)
	if err != nil {
		storageFailure(w, r, err, "delete blobs failed")
		return
	}
	resp := DeleteBlobsResponse{Deleted: len(keys) - len(failed), Failed: make([]DeleteError, 0, len(failed))}
	for _, f := range failed {
		resp.Failed = append(resp.Failed, DeleteError(f))
	}
	if len(failed) > 0 {
		hlog.FromRequest(r).Error().Int("failed", len(failed)).Str("first_key", failed[0].Key).
			Str("first_error", failed[0].Message).Msg("some blobs were not deleted")
	}
	httputil.WriteJSON(w, r, http.StatusOK, resp)
}

// maxListLimit is S3's own page cap.
const maxListLimit = 1000

// ListBlobsResponse is the body of GET /internal/blobs.
type ListBlobsResponse struct {
	Blobs []BlobInfo `json:"blobs"`
	// NextCursor resumes the listing; null once it is complete.
	NextCursor *string `json:"next_cursor"`
}

// BlobInfo describes a stored blob.
type BlobInfo struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
}

func (s *Server) listBlobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	if prefix != "" && !userPrefix(prefix) {
		httputil.WriteError(w, r, http.StatusBadRequest, "prefix must be empty or <userId>/")
		return
	}
	var olderThan time.Time
	if v := q.Get("older_than"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httputil.WriteError(w, r, http.StatusBadRequest, "older_than must be an RFC 3339 timestamp")
			return
		}
		olderThan = t
	}
	limit := maxListLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			httputil.WriteError(w, r, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", maxListLimit))
			return
		}
		limit = n
	}

	page, err := s.Storage.ListObjects(r.Context(), prefix, q.Get("cursor"), limit)
	if err != nil {
		storageFailure(w, r, err, "list blobs failed")
		return
	}
	// S3 cannot filter by age, so a page may hold fewer than limit blobs, or
	// none, while the listing goes on.
	resp := ListBlobsResponse{Blobs: make([]BlobInfo, 0, len(page.Objects))}
	for _, o := range page.Objects {
		if !olderThan.IsZero() && !o.LastModified.Before(olderThan) {
			continue
		}
		resp.Blobs = append(resp.Blobs, BlobInfo{Key: o.Key, Size: o.Size, LastModified: o.LastModified.UTC()})
	}
	if page.Truncated {
		resp.NextCursor = &page.LastKey
	}
	httputil.WriteJSON(w, r, http.StatusOK, resp)
}

// DeleteUserResponse is the body of DELETE /internal/users/{userId}.
type DeleteUserResponse struct {
	Deleted int  `json:"deleted"`
	Done    bool `json:"done"`
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userId")
	if !ids.IsUserID(userID) {
		httputil.WriteError(w, r, http.StatusBadRequest, "invalid user id")
		return
	}
	page, err := s.Storage.ListObjects(r.Context(), userID+"/", "", storage.MaxDeleteKeys)
	if err != nil {
		storageFailure(w, r, err, "list user blobs failed")
		return
	}
	keys := make([]string, len(page.Objects))
	for i, o := range page.Objects {
		keys[i] = o.Key
	}
	failed, err := s.Storage.DeleteObjects(r.Context(), keys)
	if err != nil {
		storageFailure(w, r, err, "delete user blobs failed")
		return
	}
	if len(failed) > 0 {
		hlog.FromRequest(r).Error().Int("failed", len(failed)).Str("first_key", failed[0].Key).
			Str("first_error", failed[0].Message).Msg("some user blobs were not deleted")
		httputil.WriteError(w, r, http.StatusInternalServerError, fmt.Sprintf("%d blobs could not be deleted", len(failed)))
		return
	}
	httputil.WriteJSON(w, r, http.StatusOK, DeleteUserResponse{Deleted: len(keys), Done: !page.Truncated})
}

func userPrefix(prefix string) bool {
	id, ok := strings.CutSuffix(prefix, "/")
	return ok && ids.IsUserID(id)
}
