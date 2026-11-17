package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/ids"
	"futo-notes-server/internal/objects"
	"futo-notes-server/internal/syncapi"
)

const (
	maxBatchBytes   = 32 << 20
	maxBatchEntries = 200
)

// stagedBlobs reads the blob of an object create or update as the self-hosted
// server names it: a blob_key the caller staged with POST /api/blobs, and its
// size_bytes, which is validated but not used.
type stagedBlobs struct{}

// ReadCreate decodes {"blob_key", "size_bytes"}.
func (stagedBlobs) ReadCreate(r *http.Request) (syncapi.BlobClaim, error) {
	var body struct {
		BlobKey   *string `json:"blob_key"`
		SizeBytes *int64  `json:"size_bytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return syncapi.BlobClaim{}, errors.New("invalid json")
	}
	if body.BlobKey == nil || !objects.ValidBlobKey(syncapi.SessionFrom(r).User.ID, *body.BlobKey) {
		return syncapi.BlobClaim{}, errors.New("invalid blob_key")
	}
	if body.SizeBytes == nil || *body.SizeBytes < 0 {
		return syncapi.BlobClaim{}, errors.New("invalid size_bytes")
	}
	return syncapi.ReadyClaim(objects.StagedBlob(*body.BlobKey)), nil
}

// ReadUpdate decodes {"version", "blob_key", "size_bytes"}.
func (stagedBlobs) ReadUpdate(r *http.Request) (int64, syncapi.BlobClaim, error) {
	var body struct {
		Version   *int64  `json:"version"`
		BlobKey   *string `json:"blob_key"`
		SizeBytes *int64  `json:"size_bytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return 0, syncapi.BlobClaim{}, errors.New("invalid json")
	}
	if body.Version == nil || *body.Version < 1 {
		return 0, syncapi.BlobClaim{}, errors.New("invalid version")
	}
	if body.BlobKey == nil || !objects.ValidBlobKey(syncapi.SessionFrom(r).User.ID, *body.BlobKey) {
		return 0, syncapi.BlobClaim{}, errors.New("invalid blob_key")
	}
	if body.SizeBytes == nil || *body.SizeBytes < 0 {
		return 0, syncapi.BlobClaim{}, errors.New("invalid size_bytes")
	}
	return *body.Version, syncapi.ReadyClaim(objects.StagedBlob(*body.BlobKey)), nil
}

func readBlobBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, ok := syncapi.ReadLimitedBody(w, r, maxBlobBytes, "blob too large")
	if !ok {
		return nil, false
	}
	if len(body) == 0 {
		syncapi.WriteError(w, http.StatusBadRequest, "empty body")
		return nil, false
	}
	return body, true
}

func handleCreateBlobObject(database *db.DB, blobStore *blobs.Store, publisher events.Publisher) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		mID, ok := syncapi.MutationID(r)
		if !ok {
			syncapi.WriteError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		body, ok := readBlobBody(w, r)
		if !ok {
			return
		}
		userID := syncapi.SessionFrom(r).User.ID
		// The Mutation ID is claimed before any ciphertext is staged, so
		// recovery reports a pending create rather than a missing one while
		// this request commits, and a replay of a completed create answers
		// from the ledger without staging a second blob.
		reservation, err := store.ReserveCreate(r.Context(),
			objects.Mutation{UserID: userID, CollectionID: r.PathValue("id"), MutationID: mID})
		if err != nil {
			syncapi.WriteObjectError(w, r, "reserving inline create mutation", err)
			return
		}
		if reservation.Replay != nil {
			syncapi.WriteJSON(w, http.StatusCreated, *reservation.Replay)
			return
		}
		key, err := blobs.Stage(r.Context(), database, blobStore, userID, body)
		if err != nil {
			syncapi.ServerError(w, r, "staging inline create blob", err)
			return
		}
		response, err := store.CreateReserved(r.Context(), reservation, objects.StagedBlob(key))
		if err != nil {
			syncapi.WriteObjectError(w, r, "creating inline blob object", err)
			return
		}
		syncapi.WriteJSON(w, http.StatusCreated, response)
	}
}

func handleUpdateBlobObject(database *db.DB, blobStore *blobs.Store, publisher events.Publisher) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		mID, ok := syncapi.MutationID(r)
		if !ok {
			syncapi.WriteError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		version, ok := syncapi.ParsePositive(r.URL.Query().Get("version"))
		if !ok {
			syncapi.WriteError(w, http.StatusBadRequest, "invalid version")
			return
		}
		body, ok := readBlobBody(w, r)
		if !ok {
			return
		}
		userID := syncapi.SessionFrom(r).User.ID
		key, err := blobs.Stage(r.Context(), database, blobStore, userID, body)
		if err != nil {
			syncapi.ServerError(w, r, "staging inline update blob", err)
			return
		}
		m := objects.Mutation{UserID: userID, CollectionID: r.PathValue("id"), ObjectID: r.PathValue("objectId"), MutationID: mID}
		response, err := store.Update(r.Context(), m, objects.StagedBlob(key), version)
		if err != nil {
			syncapi.WriteObjectError(w, r, "updating inline blob object", err)
			return
		}
		syncapi.WriteJSON(w, http.StatusOK, response)
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
		body, ok := syncapi.ReadLimitedBody(w, r, maxBatchBytes, "request too large")
		if !ok {
			return
		}
		entries, err := parseBatch(body)
		if err != nil {
			syncapi.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		userID, collectionID := syncapi.SessionFrom(r).User.ID, r.PathValue("id")
		if err := store.CheckCollection(r.Context(), userID, collectionID); err != nil {
			syncapi.WriteObjectError(w, r, "checking collection for blob object batch", err)
			return
		}
		apply := batchApplier{database: database, objects: store, blobs: blobStore, userID: userID, collectionID: collectionID}
		results := make([]any, 0, len(entries))
		for _, entry := range entries {
			results = append(results, apply.entry(r, entry))
		}
		syncapi.WriteJSON(w, http.StatusOK, batchResponse{Results: results})
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
				hlog.FromRequest(r).Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("reserving batch create mutation")
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
		hlog.FromRequest(r).Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("staging batch blob")
		return batchInternalError
	}
	var response objects.MutationResponse
	if entry.Operation == opCreate {
		response, err = b.objects.CreateReserved(r.Context(), reservation, objects.StagedBlob(key))
	} else {
		response, err = b.objects.Update(r.Context(), m, objects.StagedBlob(key), int64(entry.Version))
	}
	if err != nil {
		result, unexpected := batchFailure(err)
		if unexpected {
			hlog.FromRequest(r).Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("applying batch object mutation")
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
