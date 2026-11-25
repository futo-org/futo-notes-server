package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/objects"
	"futo-notes-server/internal/syncapi"
)

const (
	// maxBatchEntries caps the entries of one batch claim, as the
	// self-hosted batch upload caps its frames.
	maxBatchEntries = 200
	// maxBatchBodyBytes caps a batch claim body. 200 entries of the longest
	// Mutation-Id, an object id, a version and a receipt fit in well under
	// half of it.
	maxBatchBodyBytes = 256 << 10
)

// batchEntry is one object write of a batch claim. An entry naming
// object_id updates that object to version; one without creates an object.
type batchEntry struct {
	MutationID *string `json:"mutation_id"`
	ObjectID   *string `json:"object_id"`
	Version    *int64  `json:"version"`
	Receipt    *string `json:"receipt"`
}

// batchResult is one entry's answer: the status and body the single create
// or update would have answered.
type batchResult struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

type batchResponse struct {
	Results []batchResult `json:"results"`
}

// handleBatchObjects serves POST /api/collections/{id}/objects/batch: up to
// 200 creates and updates from storage receipts, each run as the single
// create or update would run it, in its own transaction under its own
// Mutation-Id, so a retried batch replays every entry that already has a
// result.
func (s *Server) handleBatchObjects(blobs receiptBlobs) http.HandlerFunc {
	store := objects.New(s.database, s.publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := syncapi.ReadLimitedBody(w, r, maxBatchBodyBytes, "request too large")
		if !ok {
			return
		}
		var request struct {
			Entries []batchEntry `json:"entries"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		// A misspelt object_id would otherwise turn an update into a create.
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			syncapi.WriteError(w, http.StatusBadRequest, "invalid json")
			return
		}
		switch {
		case len(request.Entries) == 0:
			syncapi.WriteError(w, http.StatusBadRequest, "batch must contain at least one entry")
			return
		case len(request.Entries) > maxBatchEntries:
			syncapi.WriteError(w, http.StatusBadRequest, fmt.Sprintf("too many entries (max %d)", maxBatchEntries))
			return
		}
		userID, collectionID := syncapi.SessionFrom(r).User.ID, r.PathValue("id")
		if err := store.CheckCollection(r.Context(), userID, collectionID); err != nil {
			syncapi.WriteObjectError(w, r, "checking collection for object batch", err)
			return
		}
		results := make([]batchResult, 0, len(request.Entries))
		for i, entry := range request.Entries {
			m := objects.Mutation{UserID: userID, CollectionID: collectionID}
			out, err := runBatchEntry(r, store, blobs, m, entry)
			if err != nil {
				hlog.FromRequest(r).Error().Err(err).Int("entry", i).Str("method", r.Method).Str("path", r.URL.Path).
					Msg("applying object batch entry")
				out = syncapi.Outcome{Status: http.StatusInternalServerError, Body: syncapi.ErrorResponse{Error: "internal server error"}}
			}
			results = append(results, batchResult{Status: out.Status, Body: out.Body})
		}
		syncapi.WriteJSON(w, http.StatusOK, batchResponse{Results: results})
	}
}

// runBatchEntry runs one entry as the single create or update with its
// Mutation-Id and body would run, and returns that call's answer. A
// Mutation-Id is required here, so every entry can replay.
func runBatchEntry(r *http.Request, store *objects.Store, blobs receiptBlobs, m objects.Mutation, entry batchEntry) (syncapi.Outcome, error) {
	if entry.MutationID == nil || !objects.ValidMutationID(*entry.MutationID) {
		return syncapi.Outcome{Status: http.StatusBadRequest, Body: syncapi.ErrorResponse{Error: "invalid Mutation-Id"}}, nil
	}
	m.MutationID = *entry.MutationID
	claim := syncapi.DeferredClaim(func() (objects.Blob, error) { return blobs.verify(r, entry.Receipt) })
	if entry.ObjectID == nil {
		if entry.Version != nil {
			return invalidVersion, nil
		}
		return syncapi.CreateObject(r.Context(), store, m, claim)
	}
	if entry.Version == nil || *entry.Version < 1 {
		return invalidVersion, nil
	}
	m.ObjectID = *entry.ObjectID
	return syncapi.UpdateObject(r.Context(), store, m, *entry.Version, claim)
}

var invalidVersion = syncapi.Outcome{Status: http.StatusBadRequest, Body: syncapi.ErrorResponse{Error: "invalid version"}}
