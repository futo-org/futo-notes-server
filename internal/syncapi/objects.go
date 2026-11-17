package syncapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/httpbody"
	"futo-notes-server/internal/objects"
)

const maxPullLimit = 1000

// CodeReceiptUsed is the code of the 409 for a receipt an earlier mutation
// already spent. Only the hosted API claims receipts.
const CodeReceiptUsed = "receipt_used"

// BlobReader decodes the body of an object create or update, which names the
// blob the object will point at: a staged blob_key on the self-hosted server,
// a storage receipt on the hosted API. The text of a returned error is the
// 400 message, with its code when it is a CodedError. Readers find the caller
// with SessionFrom.
type BlobReader interface {
	ReadCreate(r *http.Request) (BlobClaim, error)
	ReadUpdate(r *http.Request) (version int64, claim BlobClaim, err error)
}

// BlobClaim is the blob a decoded create or update names.
type BlobClaim struct {
	blob   objects.Blob
	verify func() (objects.Blob, error)
}

// ReadyClaim is a claim the reader has already checked in full.
func ReadyClaim(blob objects.Blob) BlobClaim { return BlobClaim{blob: blob} }

// DeferredClaim is a claim verify checks, returning the blob or, as its
// error, the 400 message (coded when a CodedError). Handlers call verify only when the mutation will
// run: a retry under a Mutation-Id that already carries a result is answered
// with that result whatever the claim now says, so a receipt that expired
// since the first attempt cannot turn a replay into a 400.
func DeferredClaim(verify func() (objects.Blob, error)) BlobClaim {
	return BlobClaim{verify: verify}
}

// resolve returns the claim's blob, verifying a deferred claim.
func (c BlobClaim) resolve() (objects.Blob, error) {
	if c.verify == nil {
		return c.blob, nil
	}
	return c.verify()
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
	if out, ok := objectErrorOutcome(err); ok {
		out.write(w)
		return
	}
	ServerError(w, r, action, err)
}

// Outcome is the status and body an object write answers with.
type Outcome struct {
	Status int
	Body   any
}

func (o Outcome) write(w http.ResponseWriter) { WriteJSON(w, o.Status, o.Body) }

// objectErrorOutcome is the answer to an objects domain failure, and false
// for any other error.
func objectErrorOutcome(err error) (Outcome, bool) {
	if conflict, ok := errors.AsType[*objects.ConflictError](err); ok {
		return Outcome{http.StatusConflict, conflict.Conflict}, true
	}
	switch {
	case errors.Is(err, objects.ErrNotFound):
		return Outcome{http.StatusNotFound, ErrorResponse{Error: "not found"}}, true
	case errors.Is(err, objects.ErrBlobNotStaged):
		return Outcome{http.StatusConflict, ErrorResponse{Error: "blob is not staged"}}, true
	case errors.Is(err, objects.ErrReceiptUsed):
		return Outcome{http.StatusConflict, CodedErrorResponse{Code: CodeReceiptUsed, Error: "receipt already used"}}, true
	case errors.Is(err, objects.ErrMutationMismatch):
		return Outcome{http.StatusConflict, ErrorResponse{Error: "Mutation-Id reused for different intent"}}, true
	case errors.Is(err, objects.ErrMutationPending):
		return Outcome{http.StatusConflict, ErrorResponse{Error: "mutation still in progress"}}, true
	default:
		return Outcome{}, false
	}
}

// CreateObject runs an object create for m and the blob claim names, and
// returns what POST /api/collections/{id}/objects answers. A deferred claim
// is verified only when no result is recorded under m's Mutation ID. The
// error is set only for a failure that is not a domain outcome, which the
// caller answers with a 500.
func CreateObject(ctx context.Context, store *objects.Store, m objects.Mutation, claim BlobClaim) (Outcome, error) {
	if claim.verify != nil {
		response, replayed, err := store.ReplayCreate(ctx, m)
		if err != nil || replayed {
			return mutationOutcome(http.StatusCreated, response, err)
		}
	}
	blob, err := claim.resolve()
	if err != nil {
		return badRequestOutcome(err), nil
	}
	response, err := store.Create(ctx, m, blob)
	return mutationOutcome(http.StatusCreated, response, err)
}

// UpdateObject is CreateObject for an update of m's object to version, as
// PUT /api/collections/{id}/objects/{objectId} answers it.
func UpdateObject(ctx context.Context, store *objects.Store, m objects.Mutation, version int64, claim BlobClaim) (Outcome, error) {
	if claim.verify != nil {
		response, replayed, err := store.ReplayUpdate(ctx, m, version)
		if err != nil || replayed {
			return mutationOutcome(http.StatusOK, response, err)
		}
	}
	blob, err := claim.resolve()
	if err != nil {
		return badRequestOutcome(err), nil
	}
	response, err := store.Update(ctx, m, blob, version)
	return mutationOutcome(http.StatusOK, response, err)
}

// mutationOutcome answers a create or update's result: response with status
// on success, the failure's answer otherwise.
func mutationOutcome(status int, response objects.MutationResponse, err error) (Outcome, error) {
	if err == nil {
		return Outcome{status, response}, nil
	}
	if out, ok := objectErrorOutcome(err); ok {
		return out, nil
	}
	return Outcome{}, err
}

// ReadLimitedBody reads at most limit bytes of the request body. A longer
// body is answered with 413 and tooLarge as the message.
func ReadLimitedBody(w http.ResponseWriter, r *http.Request, limit int64, tooLarge string) ([]byte, bool) {
	body, rejection := httpbody.ReadLimited(w, r, limit, tooLarge)
	if rejection != nil {
		WriteError(w, rejection.Status, rejection.Message)
		return nil, false
	}
	return body, true
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
		claim, err := blobs.ReadCreate(r)
		if err != nil {
			writeBadRequest(w, err)
			return
		}
		m := objects.Mutation{UserID: SessionFrom(r).User.ID, CollectionID: r.PathValue("id"), MutationID: mID}
		out, err := CreateObject(r.Context(), store, m, claim)
		if err != nil {
			ServerError(w, r, "creating object", err)
			return
		}
		out.write(w)
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
		version, claim, err := blobs.ReadUpdate(r)
		if err != nil {
			writeBadRequest(w, err)
			return
		}
		m := objects.Mutation{UserID: SessionFrom(r).User.ID, CollectionID: r.PathValue("id"), ObjectID: r.PathValue("objectId"), MutationID: mID}
		out, err := UpdateObject(r.Context(), store, m, version, claim)
		if err != nil {
			ServerError(w, r, "updating object", err)
			return
		}
		out.write(w)
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
