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

	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/ids"
	"futo-notes-server/internal/storagesvc/adminapi"
)

// maxDeleteBody fits adminapi.MaxDeleteKeys blob keys with room for JSON.
const maxDeleteBody = 128 << 10

func (s *Server) deleteBlobs(w http.ResponseWriter, r *http.Request) {
	body, ok := readLimitedBody(w, r, maxDeleteBody, "request too large")
	if !ok {
		return
	}
	var request struct {
		Keys []*string `json:"keys"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		httputil.WriteError(w, r, http.StatusBadRequest, httputil.CodeInvalidRequest, "invalid json")
		return
	}
	if len(request.Keys) == 0 {
		httputil.WriteError(w, r, http.StatusBadRequest, httputil.CodeInvalidRequest, "keys must not be empty")
		return
	}
	if len(request.Keys) > adminapi.MaxDeleteKeys {
		httputil.WriteError(w, r, http.StatusBadRequest, httputil.CodeInvalidRequest, fmt.Sprintf("too many keys (max %d)", adminapi.MaxDeleteKeys))
		return
	}
	keys := make([]string, 0, len(request.Keys))
	for _, key := range request.Keys {
		if key == nil || !ids.IsBlobKey(*key) {
			httputil.WriteError(w, r, http.StatusBadRequest, httputil.CodeInvalidRequest, "keys must be <userId>/<blobId>")
			return
		}
		keys = append(keys, *key)
	}

	failed, err := s.Storage.DeleteObjects(r.Context(), keys)
	if err != nil {
		storageFailure(w, r, err, "delete blobs failed")
		return
	}
	resp := adminapi.DeleteBlobsResponse{Deleted: len(keys) - len(failed), Failed: make([]adminapi.KeyFailure, 0, len(failed))}
	for _, f := range failed {
		resp.Failed = append(resp.Failed, adminapi.KeyFailure(f))
	}
	if len(failed) > 0 {
		hlog.FromRequest(r).Error().Int("failed", len(failed)).Str("first_key", failed[0].Key).
			Str("first_error", failed[0].Message).Msg("some blobs were not deleted")
	}
	httputil.WriteJSON(w, r, http.StatusOK, resp)
}

// maxListLimit is S3's own page cap.
const maxListLimit = 1000

func (s *Server) listBlobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	if prefix != "" && !userPrefix(prefix) {
		httputil.WriteError(w, r, http.StatusBadRequest, httputil.CodeInvalidRequest, "prefix must be empty or <userId>/")
		return
	}
	var olderThan time.Time
	if v := q.Get("older_than"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httputil.WriteError(w, r, http.StatusBadRequest, httputil.CodeInvalidRequest, "older_than must be an RFC 3339 timestamp")
			return
		}
		olderThan = t
	}
	limit := maxListLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			httputil.WriteError(w, r, http.StatusBadRequest, httputil.CodeInvalidRequest, fmt.Sprintf("limit must be between 1 and %d", maxListLimit))
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
	resp := adminapi.ListBlobsResponse{Blobs: make([]adminapi.Blob, 0, len(page.Objects))}
	for _, o := range page.Objects {
		if !olderThan.IsZero() && !o.LastModified.Before(olderThan) {
			continue
		}
		resp.Blobs = append(resp.Blobs, adminapi.Blob{Key: o.Key, Size: o.Size, LastModified: o.LastModified.UTC()})
	}
	if page.Truncated {
		resp.NextCursor = &page.LastKey
	}
	httputil.WriteJSON(w, r, http.StatusOK, resp)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userId")
	if !ids.IsUserID(userID) {
		httputil.WriteError(w, r, http.StatusBadRequest, httputil.CodeInvalidRequest, "invalid user id")
		return
	}
	page, err := s.Storage.ListObjects(r.Context(), userID+"/", "", adminapi.MaxDeleteKeys)
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
		httputil.WriteError(w, r, http.StatusInternalServerError, httputil.CodeInternalError, fmt.Sprintf("%d blobs could not be deleted", len(failed)))
		return
	}
	httputil.WriteJSON(w, r, http.StatusOK, adminapi.DeleteUserResponse{Deleted: len(keys), Done: !page.Truncated})
}

func userPrefix(prefix string) bool {
	id, ok := strings.CutSuffix(prefix, "/")
	return ok && ids.IsUserID(id)
}
