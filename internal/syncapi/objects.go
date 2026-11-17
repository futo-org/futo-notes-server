package syncapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/objects"
)

const maxPullLimit = 1000

// BlobReader decodes the body of an object create or update, which names the
// blob the object will point at: a staged blob_key on the self-hosted server,
// a storage receipt on the hosted API. The text of a returned error is the
// 400 message. Readers find the caller with SessionFrom.
type BlobReader interface {
	ReadCreate(r *http.Request) (objects.Blob, error)
	ReadUpdate(r *http.Request) (version int64, blob objects.Blob, err error)
}

// MutationID returns the request's Mutation-Id header, "" when absent, and
// whether it is acceptable: at most one well-formed value.
func MutationID(r *http.Request) (string, bool) {
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

// ParsePositive parses a decimal integer and reports whether it is above zero.
func ParsePositive(value string) (int64, bool) {
	n, ok := parseNonnegative(value)
	return n, ok && n > 0
}

// WriteObjectError answers with the response an objects failure maps to.
// Anything that is not a domain failure is logged under action and answered
// with a 500.
func WriteObjectError(w http.ResponseWriter, r *http.Request, action string, err error) {
	if conflict, ok := errors.AsType[*objects.ConflictError](err); ok {
		WriteJSON(w, http.StatusConflict, conflict.Conflict)
		return
	}
	switch {
	case errors.Is(err, objects.ErrNotFound):
		WriteError(w, http.StatusNotFound, "not found")
	case errors.Is(err, objects.ErrBlobNotStaged):
		WriteError(w, http.StatusConflict, "blob is not staged")
	case errors.Is(err, objects.ErrReceiptUsed):
		WriteError(w, http.StatusConflict, "receipt already used")
	case errors.Is(err, objects.ErrMutationMismatch):
		WriteError(w, http.StatusConflict, "Mutation-Id reused for different intent")
	case errors.Is(err, objects.ErrMutationPending):
		WriteError(w, http.StatusConflict, "mutation still in progress")
	default:
		ServerError(w, r, action, err)
	}
}

// ReadLimitedBody reads at most limit bytes of the request body. A longer
// body is answered with 413 and tooLarge as the message.
func ReadLimitedBody(w http.ResponseWriter, r *http.Request, limit int64, tooLarge string) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err == nil {
		return body, true
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		WriteError(w, http.StatusRequestEntityTooLarge, tooLarge)
	} else {
		WriteError(w, http.StatusBadRequest, "could not read body")
	}
	return nil, false
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

// HandleListObjects serves GET /api/collections/{id}/objects.
func HandleListObjects(database *db.DB) http.HandlerFunc {
	store := objects.New(database, nil)
	return func(w http.ResponseWriter, r *http.Request) {
		var since int64
		if value := r.URL.Query().Get("sinceVersion"); value != "" {
			var ok bool
			since, ok = parseNonnegative(value)
			if !ok {
				WriteError(w, http.StatusBadRequest, "invalid sinceVersion")
				return
			}
		}
		var limit *int
		if value := r.URL.Query().Get("limit"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				WriteError(w, http.StatusBadRequest, "invalid limit")
				return
			}
			limit = new(min(n, maxPullLimit))
		}

		rows, hasMore, err := store.List(r.Context(), SessionFrom(r).User.ID, r.PathValue("id"), since, limit)
		if err != nil {
			WriteObjectError(w, r, "listing objects", err)
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
		WriteJSON(w, http.StatusOK, response)
	}
}

// HandleGetObject serves GET /api/collections/{id}/objects/{objectId}.
func HandleGetObject(database *db.DB) http.HandlerFunc {
	store := objects.New(database, nil)
	return func(w http.ResponseWriter, r *http.Request) {
		o, err := store.Get(r.Context(), SessionFrom(r).User.ID, r.PathValue("id"), r.PathValue("objectId"))
		if err != nil {
			WriteObjectError(w, r, "getting object", err)
			return
		}
		WriteJSON(w, http.StatusOK, objectResponse{Object: o})
	}
}

// HandleCreateObject serves POST /api/collections/{id}/objects, reading the
// blob the new object points at with blobs.
func HandleCreateObject(database *db.DB, publisher events.Publisher, blobs BlobReader) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		mID, ok := MutationID(r)
		if !ok {
			WriteError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		blob, err := blobs.ReadCreate(r)
		if err != nil {
			WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		m := objects.Mutation{UserID: SessionFrom(r).User.ID, CollectionID: r.PathValue("id"), MutationID: mID}
		response, err := store.Create(r.Context(), m, blob)
		if err != nil {
			WriteObjectError(w, r, "creating object", err)
			return
		}
		WriteJSON(w, http.StatusCreated, response)
	}
}

// HandleUpdateObject serves PUT /api/collections/{id}/objects/{objectId},
// reading the requested version and the blob with blobs.
func HandleUpdateObject(database *db.DB, publisher events.Publisher, blobs BlobReader) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		mID, ok := MutationID(r)
		if !ok {
			WriteError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		version, blob, err := blobs.ReadUpdate(r)
		if err != nil {
			WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		m := objects.Mutation{UserID: SessionFrom(r).User.ID, CollectionID: r.PathValue("id"), ObjectID: r.PathValue("objectId"), MutationID: mID}
		response, err := store.Update(r.Context(), m, blob, version)
		if err != nil {
			WriteObjectError(w, r, "updating object", err)
			return
		}
		WriteJSON(w, http.StatusOK, response)
	}
}

// HandleDeleteObject serves DELETE /api/collections/{id}/objects/{objectId}.
func HandleDeleteObject(database *db.DB, publisher events.Publisher) http.HandlerFunc {
	store := objects.New(database, publisher)
	return func(w http.ResponseWriter, r *http.Request) {
		mID, ok := MutationID(r)
		if !ok {
			WriteError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		var expected *int64
		if value := r.URL.Query().Get("version"); value != "" {
			version, ok := ParsePositive(value)
			if !ok {
				WriteError(w, http.StatusBadRequest, "invalid version")
				return
			}
			expected = &version
		}
		m := objects.Mutation{UserID: SessionFrom(r).User.ID, CollectionID: r.PathValue("id"),
			ObjectID: r.PathValue("objectId"), MutationID: mID}
		response, err := store.Delete(r.Context(), m, expected)
		if err != nil {
			WriteObjectError(w, r, "deleting object", err)
			return
		}
		WriteJSON(w, http.StatusOK, response)
	}
}

// HandleRecoverCreate serves GET
// /api/collections/{id}/create-mutations/{mutationId}.
func HandleRecoverCreate(database *db.DB) http.HandlerFunc {
	store := objects.New(database, nil)
	return func(w http.ResponseWriter, r *http.Request) {
		mID := r.PathValue("mutationId")
		if !objects.ValidMutationID(mID) {
			WriteError(w, http.StatusBadRequest, "invalid Mutation-Id")
			return
		}
		response, err := store.RecoverCreate(r.Context(), SessionFrom(r).User.ID, r.PathValue("id"), mID)
		if err != nil {
			WriteObjectError(w, r, "recovering create mutation", err)
			return
		}
		WriteJSON(w, http.StatusOK, response)
	}
}
