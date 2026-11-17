package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/ids"
	"futo-notes-server/internal/objects"
)

const (
	maxBatchBytes   = 32 << 20
	maxBatchEntries = 200
	maxPullLimit    = 1000
)

func mutationID(r *http.Request) (string, bool) {
	values := r.Header.Values("Mutation-Id")
	if len(values) == 0 {
		return "", true
	}
	if len(values) != 1 {
		return "", false
	}
	return values[0], objects.ValidMutationID(values[0])
}

func parseNonnegative(value string) (int64, bool) {
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil && n >= 0
}

func parsePositive(value string) (int64, bool) {
	n, ok := parseNonnegative(value)
	return n, ok && n > 0
}

// writeObjectError answers with the response an objects failure maps to.
// Anything that is not a domain failure is logged under action and answered
// with a 500.
func writeObjectError(w http.ResponseWriter, r *http.Request, action string, err error) {
	if conflict, ok := errors.AsType[*objects.ConflictError](err); ok {
		writeJSON(w, http.StatusConflict, conflict.Conflict)
		return
	}
	switch {
	case errors.Is(err, objects.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, objects.ErrBlobNotStaged):
		writeError(w, http.StatusConflict, "blob is not staged")
	case errors.Is(err, objects.ErrMutationMismatch):
		writeError(w, http.StatusConflict, "Mutation-Id reused for different intent")
	case errors.Is(err, objects.ErrMutationPending):
		writeError(w, http.StatusConflict, "mutation still in progress")
	default:
		serverError(w, r, action, err)
	}
}

// listObjectsResponse keeps the key order a sorted map gave the wire.
// HasMore and NextCursor are present only for a limited pull.
type listObjectsResponse struct {
	HasMore    *bool            `json:"hasMore,omitempty"`
	NextCursor *int64           `json:"nextCursor,omitempty"`
	Objects    []objects.Object `json:"objects"`
}

type objectResponse struct {
	Object objects.Object `json:"object"`
}

func handleListObjects(database *db.DB) http.HandlerFunc {
	store := objects.New(database, nil)
	return func(w http.ResponseWriter, r *http.Request) {
		var since int64
		if value := r.URL.Query().Get("sinceVersion"); value != "" {
			var ok bool
			since, ok = parseNonnegative(value)
			if !ok {
				writeError(w, http.StatusBadRequest, "invalid sinceVersion")
				return
			}
		}
		var limit *int
		if value := r.URL.Query().Get("limit"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				writeError(w, http.StatusBadRequest, "invalid limit")
				return
			}
			limit = new(min(n, maxPullLimit))
		}

		rows, hasMore, err := store.List(r.Context(), sessionFrom(r).User.ID, r.PathValue("id"), since, limit)
		if err != nil {
			writeObjectError(w, r, "listing objects", err)
			return
		}
		response := listObjectsResponse{Objects: rows}
		if limit != nil {
			next := since
			if len(rows) > 0 {
				next = rows[len(rows)-1].ChangeSeq
			}
			response.HasMore, response.NextCursor = &hasMore, &next
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func handleGetObject(database *db.DB) http.HandlerFunc {
	store := objects.New(database, nil)
	return func(w http.ResponseWriter, r *http.Request) {
		o, err := store.Get(r.Context(), sessionFrom(r).User.ID, r.PathValue("id"), r.PathValue("objectId"))
		if err != nil {
			writeObjectError(w, r, "getting object", err)
			return
		}
		writeJSON(w, http.StatusOK, objectResponse{Object: o})
	}
}

func handleCreateObject(database *db.DB, publisher events.Publisher) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		mID, ok := mutationID(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		var body struct {
			BlobKey   *string `json:"blob_key"`
			SizeBytes *int64  `json:"size_bytes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		userID := sessionFrom(r).User.ID
		if body.BlobKey == nil || !objects.ValidBlobKey(userID, *body.BlobKey) {
			writeError(w, http.StatusBadRequest, "invalid blob_key")
			return
		}
		if body.SizeBytes == nil || *body.SizeBytes < 0 {
			writeError(w, http.StatusBadRequest, "invalid size_bytes")
			return
		}
		m := objects.Mutation{UserID: userID, CollectionID: r.PathValue("id"), MutationID: mID}
		response, err := store.Create(r.Context(), m, *body.BlobKey)
		if err != nil {
			writeObjectError(w, r, "creating object", err)
			return
		}
		writeJSON(w, http.StatusCreated, response)
	}
}

func handleUpdateObject(database *db.DB, publisher events.Publisher) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		mID, ok := mutationID(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		var body struct {
			Version   *int64  `json:"version"`
			BlobKey   *string `json:"blob_key"`
			SizeBytes *int64  `json:"size_bytes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		userID := sessionFrom(r).User.ID
		if body.Version == nil || *body.Version < 1 {
			writeError(w, http.StatusBadRequest, "invalid version")
			return
		}
		if body.BlobKey == nil || !objects.ValidBlobKey(userID, *body.BlobKey) {
			writeError(w, http.StatusBadRequest, "invalid blob_key")
			return
		}
		if body.SizeBytes == nil || *body.SizeBytes < 0 {
			writeError(w, http.StatusBadRequest, "invalid size_bytes")
			return
		}
		m := objects.Mutation{UserID: userID, CollectionID: r.PathValue("id"), ObjectID: r.PathValue("objectId"), MutationID: mID}
		response, err := store.Update(r.Context(), m, *body.BlobKey, *body.Version)
		if err != nil {
			writeObjectError(w, r, "updating object", err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func handleDeleteObject(database *db.DB, publisher events.Publisher) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		mID, ok := mutationID(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		var expected *int64
		if value := r.URL.Query().Get("version"); value != "" {
			version, ok := parsePositive(value)
			if !ok {
				writeError(w, http.StatusBadRequest, "invalid version")
				return
			}
			expected = &version
		}
		m := objects.Mutation{UserID: sessionFrom(r).User.ID, CollectionID: r.PathValue("id"),
			ObjectID: r.PathValue("objectId"), MutationID: mID}
		response, err := store.Delete(r.Context(), m, expected)
		if err != nil {
			writeObjectError(w, r, "deleting object", err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

// readLimitedBody reads at most limit bytes of the request body. A longer
// body is answered with 413 and tooLarge as the message.
func readLimitedBody(w http.ResponseWriter, r *http.Request, limit int64, tooLarge string) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err == nil {
		return body, true
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		writeError(w, http.StatusRequestEntityTooLarge, tooLarge)
	} else {
		writeError(w, http.StatusBadRequest, "could not read body")
	}
	return nil, false
}

func readBlobBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, ok := readLimitedBody(w, r, maxBlobBytes, "blob too large")
	if !ok {
		return nil, false
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "empty body")
		return nil, false
	}
	return body, true
}

func handleCreateBlobObject(database *db.DB, blobStore *blobs.Store, publisher events.Publisher) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		mID, ok := mutationID(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		body, ok := readBlobBody(w, r)
		if !ok {
			return
		}
		userID := sessionFrom(r).User.ID
		// The Mutation ID is claimed before any ciphertext is staged, so
		// recovery reports a pending create rather than a missing one while
		// this request commits, and a replay of a completed create answers
		// from the ledger without staging a second blob.
		reservation, err := store.ReserveCreate(r.Context(),
			objects.Mutation{UserID: userID, CollectionID: r.PathValue("id"), MutationID: mID})
		if err != nil {
			writeObjectError(w, r, "reserving inline create mutation", err)
			return
		}
		if reservation.Replay != nil {
			writeJSON(w, http.StatusCreated, *reservation.Replay)
			return
		}
		key, err := blobs.Stage(r.Context(), database, blobStore, userID, body)
		if err != nil {
			serverError(w, r, "staging inline create blob", err)
			return
		}
		response, err := store.CreateReserved(r.Context(), reservation, key)
		if err != nil {
			writeObjectError(w, r, "creating inline blob object", err)
			return
		}
		writeJSON(w, http.StatusCreated, response)
	}
}

func handleUpdateBlobObject(database *db.DB, blobStore *blobs.Store, publisher events.Publisher) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		mID, ok := mutationID(r)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		version, ok := parsePositive(r.URL.Query().Get("version"))
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid version")
			return
		}
		body, ok := readBlobBody(w, r)
		if !ok {
			return
		}
		userID := sessionFrom(r).User.ID
		key, err := blobs.Stage(r.Context(), database, blobStore, userID, body)
		if err != nil {
			serverError(w, r, "staging inline update blob", err)
			return
		}
		m := objects.Mutation{UserID: userID, CollectionID: r.PathValue("id"), ObjectID: r.PathValue("objectId"), MutationID: mID}
		response, err := store.Update(r.Context(), m, key, version)
		if err != nil {
			writeObjectError(w, r, "updating inline blob object", err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func handleRecoverCreate(database *db.DB) http.HandlerFunc {
	store := objects.New(database, nil)
	return func(w http.ResponseWriter, r *http.Request) {
		mID := r.PathValue("mutationId")
		if !objects.ValidMutationID(mID) {
			writeError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		response, err := store.RecoverCreate(r.Context(), sessionFrom(r).User.ID, r.PathValue("id"), mID)
		if err != nil {
			writeObjectError(w, r, "recovering create mutation", err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

// batchOp is the operation byte that opens a batch entry.
type batchOp byte

const (
	opCreate batchOp = 0
	opUpdate batchOp = 1
)

// batchEntry is one parsed batch frame. Identifier is the Mutation ID of a
// create, or the object ID of an update.
type batchEntry struct {
	Operation  batchOp
	Identifier string
	Version    uint32
	Blob       []byte
}

func parseBatch(body []byte) ([]batchEntry, error) {
	var entries []batchEntry
	for offset := 0; offset < len(body); {
		if len(entries) == maxBatchEntries {
			return nil, fmt.Errorf("too many entries (max %d)", maxBatchEntries)
		}
		if len(body)-offset < 1 {
			return nil, errors.New("truncated operation")
		}
		op := batchOp(body[offset])
		offset++
		if op > opUpdate {
			return nil, errors.New("invalid operation")
		}
		if len(body)-offset < 2 {
			return nil, errors.New("truncated object id length")
		}
		identifierLen := int(binary.BigEndian.Uint16(body[offset : offset+2]))
		offset += 2
		if len(body)-offset < identifierLen {
			return nil, errors.New("truncated identifier")
		}
		identifierBytes := body[offset : offset+identifierLen]
		offset += identifierLen
		if !utf8.Valid(identifierBytes) {
			return nil, errors.New("invalid identifier")
		}
		identifier := string(identifierBytes)
		if op == opCreate && !objects.ValidMutationID(identifier) {
			return nil, errors.New("invalid create mutation id")
		}
		if op == opUpdate && !ids.IsUUID(identifier) {
			return nil, errors.New("invalid update object id")
		}
		if len(body)-offset < 4 {
			return nil, errors.New("truncated version")
		}
		version := binary.BigEndian.Uint32(body[offset : offset+4])
		offset += 4
		if op == opCreate && version != 0 {
			return nil, errors.New("create version must be zero")
		}
		if op == opUpdate && version == 0 {
			return nil, errors.New("update version must be positive")
		}
		if len(body)-offset < 4 {
			return nil, errors.New("truncated blob length")
		}
		blobLen := int(binary.BigEndian.Uint32(body[offset : offset+4]))
		offset += 4
		if blobLen == 0 {
			return nil, errors.New("blob must not be empty")
		}
		if blobLen > len(body)-offset {
			return nil, errors.New("truncated blob")
		}
		entries = append(entries, batchEntry{Operation: op, Identifier: identifier, Version: version, Blob: body[offset : offset+blobLen]})
		offset += blobLen
	}
	if len(entries) == 0 {
		return nil, errors.New("batch must contain at least one entry")
	}
	return entries, nil
}

// Batch results. Field order matches the sorted keys a map gave the wire.
type (
	batchResponse struct {
		Results []any `json:"results"`
	}
	batchObjectResult struct {
		CollectionVersion int64          `json:"collectionVersion"`
		Object            objects.Object `json:"object"`
		Status            string         `json:"status"`
	}
	// batchConflictResult always carries currentBlobKey, null when the
	// object has no blob.
	batchConflictResult struct {
		CurrentBlobKey *string `json:"currentBlobKey"`
		CurrentVersion int64   `json:"currentVersion"`
		Status         string  `json:"status"`
	}
	batchStatusResult struct {
		Error  string `json:"error,omitempty"`
		Status string `json:"status"`
	}
)

var batchInternalError = batchStatusResult{Error: "internal server error", Status: "error"}

func handleBatchBlobObjects(database *db.DB, blobStore *blobs.Store, publisher events.Publisher) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := readLimitedBody(w, r, maxBatchBytes, "request too large")
		if !ok {
			return
		}
		entries, err := parseBatch(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		userID, collectionID := sessionFrom(r).User.ID, r.PathValue("id")
		if err := store.CheckCollection(r.Context(), userID, collectionID); err != nil {
			writeObjectError(w, r, "checking collection for blob object batch", err)
			return
		}
		apply := batchApplier{database: database, objects: store, blobs: blobStore, userID: userID, collectionID: collectionID}
		results := make([]any, 0, len(entries))
		for _, entry := range entries {
			results = append(results, apply.entry(r, entry))
		}
		writeJSON(w, http.StatusOK, batchResponse{Results: results})
	}
}

// batchApplier applies the entries of one batch to the caller's collection.
type batchApplier struct {
	database     *db.DB
	objects      *objects.Store
	blobs        *blobs.Store
	userID       string
	collectionID string
}

// entry stages one entry's blob and applies its mutation, returning the
// entry's result. A failure affects only this entry.
func (b batchApplier) entry(r *http.Request, entry batchEntry) any {
	if len(entry.Blob) > maxBlobBytes {
		return batchStatusResult{Status: "too_large"}
	}
	m := objects.Mutation{UserID: b.userID, CollectionID: b.collectionID}
	var reservation objects.Reservation
	if entry.Operation == opCreate {
		m.MutationID = entry.Identifier
		var err error
		reservation, err = b.objects.ReserveCreate(r.Context(), m)
		if err != nil {
			result, unexpected := batchFailure(err)
			if unexpected {
				log.Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("reserving batch create mutation")
			}
			return result
		}
		if reservation.Replay != nil {
			return batchResult(entry.Operation, *reservation.Replay)
		}
	} else {
		m.ObjectID = entry.Identifier
	}
	key, err := blobs.Stage(r.Context(), b.database, b.blobs, b.userID, entry.Blob)
	if err != nil {
		log.Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("staging batch blob")
		return batchInternalError
	}
	var response objects.MutationResponse
	if entry.Operation == opCreate {
		response, err = b.objects.CreateReserved(r.Context(), reservation, key)
	} else {
		response, err = b.objects.Update(r.Context(), m, key, int64(entry.Version))
	}
	if err != nil {
		result, unexpected := batchFailure(err)
		if unexpected {
			log.Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("applying batch object mutation")
		}
		return result
	}
	return batchResult(entry.Operation, response)
}

// batchResult is the entry result for a successful mutation.
func batchResult(operation batchOp, response objects.MutationResponse) batchObjectResult {
	status := "updated"
	if operation == opCreate {
		status = "created"
		if response.Replayed != nil && *response.Replayed {
			status = "replayed"
		}
	}
	return batchObjectResult{CollectionVersion: response.CollectionVersion, Object: response.Object, Status: status}
}

// batchFailure is the entry result for a failed mutation. unexpected reports
// a failure that is not a domain outcome, which the caller logs.
func batchFailure(err error) (result any, unexpected bool) {
	if conflict, ok := errors.AsType[*objects.ConflictError](err); ok {
		return batchConflictResult{
			CurrentBlobKey: conflict.Conflict.CurrentBlobKey,
			CurrentVersion: conflict.Conflict.CurrentVersion,
			Status:         "conflict",
		}, false
	}
	switch {
	case errors.Is(err, objects.ErrNotFound):
		return batchStatusResult{Status: "not_found"}, false
	case errors.Is(err, objects.ErrMutationMismatch):
		return batchStatusResult{Error: "mutation id reused for different intent", Status: "error"}, false
	case errors.Is(err, objects.ErrMutationPending):
		return batchStatusResult{Error: "mutation still in progress", Status: "error"}, false
	case errors.Is(err, objects.ErrBlobNotStaged):
		// This request staged the blob itself, so this does not happen in
		// practice; the entry reads as an internal error, as it always has.
		return batchInternalError, false
	default:
		return batchInternalError, true
	}
}
