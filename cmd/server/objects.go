package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"unicode/utf8"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/objects"
)

const (
	maxBatchBytes   = 33554432
	maxBatchEntries = 200
	maxPullLimit    = 1000
)

func mutationID(r *http.Request) (string, bool) {
	values, present := r.Header[http.CanonicalHeaderKey("Mutation-Id")]
	if !present {
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

func handleListObjects(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		since := int64(0)
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
			if n > maxPullLimit {
				n = maxPullLimit
			}
			limit = &n
		}

		userID := sessionFrom(r).User.ID
		collectionID := r.PathValue("id")
		rows, hasMore, err := objects.List(r.Context(), database, userID, collectionID, since, limit)
		if err != nil {
			if errors.Is(err, objects.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not found")
				return
			}
			serverError(w, r, "listing objects", err)
			return
		}
		response := map[string]any{"objects": rows}
		if limit != nil {
			next := since
			if len(rows) > 0 {
				next, _ = strconv.ParseInt(rows[len(rows)-1].ChangeSeq, 10, 64)
			}
			response["hasMore"] = hasMore
			response["nextCursor"] = next
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func handleGetObject(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o, err := objects.Get(r.Context(), database, sessionFrom(r).User.ID, r.PathValue("id"), r.PathValue("objectId"))
		if err != nil {
			serverError(w, r, "getting object", err)
			return
		}
		if o == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": o})
	}
}

func handleCreateObject(database *db.DB, publisher events.Publisher) http.HandlerFunc {
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
		outcome, err := objects.Create(r.Context(), database, publisher, userID, r.PathValue("id"), mID, *body.BlobKey)
		if err != nil {
			serverError(w, r, "creating object", err)
			return
		}
		writeMutationOutcome(w, http.StatusCreated, outcome)
	}
}

func handleUpdateObject(database *db.DB, publisher events.Publisher) http.HandlerFunc {
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
		outcome, err := objects.Update(r.Context(), database, publisher, userID, r.PathValue("id"),
			r.PathValue("objectId"), mID, *body.BlobKey, *body.Version)
		if err != nil {
			serverError(w, r, "updating object", err)
			return
		}
		writeMutationOutcome(w, http.StatusOK, outcome)
	}
}

func handleDeleteObject(database *db.DB, publisher events.Publisher) http.HandlerFunc {
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
		outcome, err := objects.Delete(r.Context(), database, publisher, sessionFrom(r).User.ID,
			r.PathValue("id"), r.PathValue("objectId"), mID, expected)
		if err != nil {
			serverError(w, r, "deleting object", err)
			return
		}
		switch outcome.Code {
		case objects.OK:
			writeJSON(w, http.StatusOK, outcome.Response)
		case objects.NotFound:
			writeError(w, http.StatusNotFound, "not found")
		case objects.VersionConflict:
			writeJSON(w, http.StatusConflict, outcome.Conflict)
		case objects.MutationMismatch:
			writeError(w, http.StatusConflict, "Mutation-Id reused for different intent")
		case objects.MutationPending:
			writeError(w, http.StatusConflict, "mutation still in progress")
		default:
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
	}
}

func readBlobBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBlobBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "blob too large")
		} else {
			writeError(w, http.StatusBadRequest, "could not read body")
		}
		return nil, false
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "empty body")
		return nil, false
	}
	return body, true
}

func handleCreateBlobObject(database *db.DB, store *blobs.Store, publisher events.Publisher) http.HandlerFunc {
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
		key, err := blobs.Stage(r.Context(), database, store, userID, body)
		if err != nil {
			serverError(w, r, "staging inline create blob", err)
			return
		}
		outcome, err := objects.Create(r.Context(), database, publisher, userID, r.PathValue("id"), mID, key)
		if err != nil {
			serverError(w, r, "creating inline blob object", err)
			return
		}
		writeMutationOutcome(w, http.StatusCreated, outcome)
	}
}

func handleUpdateBlobObject(database *db.DB, store *blobs.Store, publisher events.Publisher) http.HandlerFunc {
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
		key, err := blobs.Stage(r.Context(), database, store, userID, body)
		if err != nil {
			serverError(w, r, "staging inline update blob", err)
			return
		}
		outcome, err := objects.Update(r.Context(), database, publisher, userID, r.PathValue("id"),
			r.PathValue("objectId"), mID, key, version)
		if err != nil {
			serverError(w, r, "updating inline blob object", err)
			return
		}
		writeMutationOutcome(w, http.StatusOK, outcome)
	}
}

func handleRecoverCreate(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mID := r.PathValue("mutationId")
		if !objects.ValidMutationID(mID) {
			writeError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		outcome, err := objects.RecoverCreate(r.Context(), database, sessionFrom(r).User.ID, r.PathValue("id"), mID)
		if err != nil {
			serverError(w, r, "recovering create mutation", err)
			return
		}
		switch outcome.Code {
		case objects.OK:
			writeJSON(w, http.StatusOK, outcome.Response)
		case objects.MutationMismatch:
			writeError(w, http.StatusConflict, "Mutation-Id reused for different intent")
		case objects.MutationPending:
			writeError(w, http.StatusConflict, "mutation still in progress")
		default:
			writeError(w, http.StatusNotFound, "not found")
		}
	}
}

func writeMutationOutcome(w http.ResponseWriter, successStatus int, outcome objects.MutationOutcome) {
	switch outcome.Code {
	case objects.OK:
		writeJSON(w, successStatus, outcome.Response)
	case objects.NotFound:
		writeError(w, http.StatusNotFound, "not found")
	case objects.VersionConflict:
		writeJSON(w, http.StatusConflict, outcome.Conflict)
	case objects.BlobNotStaged:
		writeError(w, http.StatusConflict, "blob is not staged")
	case objects.MutationMismatch:
		writeError(w, http.StatusConflict, "Mutation-Id reused for different intent")
	case objects.MutationPending:
		writeError(w, http.StatusConflict, "mutation still in progress")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

type batchEntry struct {
	Operation  byte
	Identifier string
	Version    uint32
	Blob       []byte
}

func parseBatch(body []byte) ([]batchEntry, string) {
	entries := make([]batchEntry, 0)
	for offset := 0; offset < len(body); {
		if len(entries) == maxBatchEntries {
			return nil, "too many entries (max 200)"
		}
		if len(body)-offset < 1 {
			return nil, "truncated operation"
		}
		op := body[offset]
		offset++
		if op > 1 {
			return nil, "invalid operation"
		}
		if len(body)-offset < 2 {
			return nil, "truncated object id length"
		}
		identifierLen := int(binary.BigEndian.Uint16(body[offset : offset+2]))
		offset += 2
		if len(body)-offset < identifierLen {
			return nil, "truncated identifier"
		}
		identifierBytes := body[offset : offset+identifierLen]
		offset += identifierLen
		if !utf8.Valid(identifierBytes) {
			return nil, "invalid identifier"
		}
		identifier := string(identifierBytes)
		if op == 0 && !objects.ValidMutationID(identifier) {
			return nil, "invalid create mutation id"
		}
		if op == 1 && !objects.ValidUUID(identifier) {
			return nil, "invalid update object id"
		}
		if len(body)-offset < 4 {
			return nil, "truncated version"
		}
		version := binary.BigEndian.Uint32(body[offset : offset+4])
		offset += 4
		if op == 0 && version != 0 {
			return nil, "create version must be zero"
		}
		if op == 1 && version == 0 {
			return nil, "update version must be positive"
		}
		if len(body)-offset < 4 {
			return nil, "truncated blob length"
		}
		blobLen := int(binary.BigEndian.Uint32(body[offset : offset+4]))
		offset += 4
		if blobLen == 0 {
			return nil, "blob must not be empty"
		}
		if blobLen > len(body)-offset {
			return nil, "truncated blob"
		}
		entries = append(entries, batchEntry{Operation: op, Identifier: identifier, Version: version, Blob: body[offset : offset+blobLen]})
		offset += blobLen
	}
	if len(entries) == 0 {
		return nil, "batch must contain at least one entry"
	}
	return entries, ""
}

func handleBatchBlobObjects(database *db.DB, store *blobs.Store, publisher events.Publisher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBatchBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "request too large")
			} else {
				writeError(w, http.StatusBadRequest, "could not read body")
			}
			return
		}
		entries, parseError := parseBatch(body)
		if parseError != "" {
			writeError(w, http.StatusBadRequest, parseError)
			return
		}
		userID, collectionID := sessionFrom(r).User.ID, r.PathValue("id")
		found, err := objects.Exists(r.Context(), database, userID, collectionID)
		if err != nil {
			serverError(w, r, "checking collection for blob object batch", err)
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		results := make([]any, 0, len(entries))
		for _, entry := range entries {
			if len(entry.Blob) > maxBlobBytes {
				results = append(results, map[string]any{"status": "too_large"})
				continue
			}
			key, err := blobs.Stage(r.Context(), database, store, userID, entry.Blob)
			if err != nil {
				slog.Error("staging batch blob", "err", err, "method", r.Method, "path", r.URL.Path)
				results = append(results, map[string]any{"status": "error", "error": "internal server error"})
				continue
			}
			var outcome objects.MutationOutcome
			if entry.Operation == 0 {
				outcome, err = objects.Create(r.Context(), database, publisher, userID, collectionID, entry.Identifier, key)
			} else {
				outcome, err = objects.Update(r.Context(), database, publisher, userID, collectionID, entry.Identifier, "", key, int64(entry.Version))
			}
			if err != nil {
				slog.Error("applying batch object mutation", "err", err, "method", r.Method, "path", r.URL.Path)
				results = append(results, map[string]any{"status": "error", "error": "internal server error"})
				continue
			}
			results = append(results, batchResult(entry.Operation, outcome))
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	}
}

func batchResult(operation byte, outcome objects.MutationOutcome) any {
	switch outcome.Code {
	case objects.OK:
		status := "updated"
		if operation == 0 {
			status = "created"
			if outcome.Response.Replayed != nil && *outcome.Response.Replayed {
				status = "replayed"
			}
		}
		return map[string]any{"status": status, "object": outcome.Response.Object, "collectionVersion": outcome.Response.CollectionVersion}
	case objects.NotFound:
		return map[string]any{"status": "not_found"}
	case objects.VersionConflict:
		return map[string]any{"status": "conflict", "currentVersion": outcome.Conflict.CurrentVersion, "currentBlobKey": outcome.Conflict.CurrentBlobKey}
	case objects.MutationMismatch:
		return map[string]any{"status": "error", "error": "mutation id reused for different intent"}
	case objects.MutationPending:
		return map[string]any{"status": "error", "error": "mutation still in progress"}
	default:
		return map[string]any{"status": "error", "error": "internal server error"}
	}
}
