// Package objects owns versioned object metadata and coordinates its blob
// ledger. Mutations are idempotent under a client Mutation ID: the first
// outcome is stored durably and every retry with the same ID replays it.
package objects

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"
	"uuid"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/ids"
)

const objectColumns = `id, collection_id, version, change_seq, deleted,
	blob_key, size_bytes, created_at, updated_at`

var mutationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// Domain failures. A *ConflictError reports a version conflict.
var (
	// ErrNotFound reports that the user owns no such collection or object, or
	// no recoverable create under a Mutation ID.
	ErrNotFound = errors.New("not found")
	// ErrBlobNotStaged reports that a mutation named a blob key the user has
	// not staged within the last 24 hours.
	ErrBlobNotStaged = errors.New("blob is not staged")
	// ErrMutationMismatch reports that a Mutation ID was reused for a
	// different intent than the one it was first used for.
	ErrMutationMismatch = errors.New("mutation id reused for different intent")
	// ErrMutationPending reports that the first request under a Mutation ID
	// has not finished yet.
	ErrMutationPending = errors.New("mutation still in progress")
	// ErrReceiptUsed reports that a mutation named a ReceiptBlob whose key the
	// blob ledger already records, so the receipt was already spent.
	ErrReceiptUsed = errors.New("receipt already used")
)

// ConflictError reports a mutation that named a version other than the one
// the object required. Conflict is the payload the client receives.
type ConflictError struct {
	Conflict Conflict
}

// Error returns the conflict's error message, "version conflict".
func (e *ConflictError) Error() string { return e.Conflict.Error }

// Object is the metadata of one encrypted object. The integer fields travel
// as JSON strings.
type Object struct {
	ID           string    `json:"id"`
	CollectionID string    `json:"collection_id"`
	Version      int64     `json:"version,string"`
	ChangeSeq    int64     `json:"change_seq,string"`
	Deleted      bool      `json:"deleted"`
	BlobKey      *string   `json:"blob_key"`
	SizeBytes    *int64    `json:"size_bytes,string"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// DeletedObject is the tombstone a delete answers with.
type DeletedObject struct {
	ID        string `json:"id"`
	Version   int64  `json:"version,string"`
	ChangeSeq int64  `json:"change_seq,string"`
	Deleted   bool   `json:"deleted"`
}

// MutationResponse is the answer to a create or update. Replayed is set only
// for creates: false for a fresh create, true for a replay.
type MutationResponse struct {
	Object            Object `json:"object"`
	CollectionVersion int64  `json:"collectionVersion"`
	Replayed          *bool  `json:"replayed,omitempty"`
}

// DeleteResponse is the answer to a delete.
type DeleteResponse struct {
	Object            DeletedObject `json:"object"`
	CollectionVersion int64         `json:"collectionVersion"`
}

// Conflict is the body of a version conflict: the object's current version
// and blob key, so the client can merge.
type Conflict struct {
	Error          string  `json:"error"`
	CurrentVersion int64   `json:"currentVersion"`
	CurrentBlobKey *string `json:"currentBlobKey"`
}

// Mutation names what a mutation acts on. ObjectID is the object an update or
// delete targets and is ignored for a create. MutationID is the client's
// idempotency key; empty means the mutation is not recorded for replay.
type Mutation struct {
	UserID       string
	CollectionID string
	ObjectID     string
	MutationID   string
}

// Store reads and mutates objects. The publisher announces each new
// collection version; a Store that only reads may have a nil publisher.
type Store struct {
	database  *db.DB
	publisher events.Publisher
}

// New returns a Store over database that announces changes through publisher.
func New(database *db.DB, publisher events.Publisher) *Store {
	return &Store{database: database, publisher: publisher}
}

// ValidMutationID reports whether id is a well-formed client Mutation ID.
func ValidMutationID(id string) bool { return mutationIDPattern.MatchString(id) }

func scanObject(row db.Scanner) (Object, error) {
	var o Object
	var createdAt, updatedAt db.Time
	if err := row.Scan(&o.ID, &o.CollectionID, &o.Version, &o.ChangeSeq, &o.Deleted,
		&o.BlobKey, &o.SizeBytes, &createdAt, &updatedAt); err != nil {
		return Object{}, err
	}
	o.CreatedAt = createdAt.Time
	o.UpdatedAt = updatedAt.Time
	return o, nil
}

// CheckCollection returns ErrNotFound unless userID owns collectionID.
func (s *Store) CheckCollection(ctx context.Context, userID, collectionID string) error {
	if !ids.IsUUID(collectionID) {
		return ErrNotFound
	}
	var one int
	err := s.database.QueryRowContext(ctx, `SELECT 1 FROM collections WHERE id = $1 AND user_id = $2`, collectionID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// List returns the collection's objects changed after since, in change
// order. With a limit it returns at most that many and reports whether more
// remain. It returns ErrNotFound unless userID owns the collection.
func (s *Store) List(ctx context.Context, userID, collectionID string, since int64, limit *int) ([]Object, bool, error) {
	if err := s.CheckCollection(ctx, userID, collectionID); err != nil {
		return nil, false, err
	}
	query := `SELECT ` + objectColumns + ` FROM objects
		WHERE collection_id = $1 AND user_id = $2 AND change_seq > $3 ORDER BY change_seq, id`
	args := []any{collectionID, userID, since}
	if limit != nil {
		query += ` LIMIT $4`
		args = append(args, *limit+1)
	}
	rows, err := s.database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	// Never nil: an empty delta serializes as [].
	result := []Object{}
	for rows.Next() {
		o, err := scanObject(rows)
		if err != nil {
			return nil, false, err
		}
		result = append(result, o)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := limit != nil && len(result) > *limit
	if hasMore {
		result = result[:*limit]
	}
	return result, hasMore, nil
}

// Get returns one object, or ErrNotFound unless userID owns it.
func (s *Store) Get(ctx context.Context, userID, collectionID, objectID string) (Object, error) {
	if !ids.IsUUID(collectionID) || !ids.IsUUID(objectID) {
		return Object{}, ErrNotFound
	}
	o, err := scanObject(s.database.QueryRowContext(ctx, `SELECT `+objectColumns+` FROM objects
		WHERE id = $1 AND collection_id = $2 AND user_id = $3`, objectID, collectionID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Object{}, ErrNotFound
	}
	return o, err
}

// pendingBody is the stored body of a mutation that has not finished, and
// pendingResult the wrapped result prepare inserts as the in-progress claim.
const (
	pendingBody   = `{"error":"mutation still in progress"}`
	pendingResult = `{"status":102,"body":` + pendingBody + `}`
)

// errorBody is the stored body of a failed mutation.
type errorBody struct {
	Error string `json:"error"`
}

// storedResult is a mutation's durable result: the HTTP status it answered
// with and its body.
type storedResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func decodeStoredResult(raw []byte) (storedResult, error) {
	var stored storedResult
	if err := json.Unmarshal(raw, &stored); err != nil {
		var legacy struct {
			Status string `json:"status"`
		}
		if legacyErr := json.Unmarshal(raw, &legacy); legacyErr == nil && legacy.Status == "pending" {
			return storedResult{Status: http.StatusProcessing, Body: json.RawMessage(pendingBody)}, nil
		}
		return stored, err
	}
	if stored.Status != 0 && len(stored.Body) != 0 {
		return stored, nil
	}
	// Accept unwrapped result bodies so durable outcomes written by an older
	// deployment remain recoverable after upgrading. Keep this error mapping
	// in sync with ExpireMutationResults in internal/jobs/jobs.go.
	stored.Body = bytes.Clone(raw)
	var body errorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return stored, err
	}
	switch body.Error {
	case "not found":
		stored.Status = http.StatusNotFound
	case "version conflict", "blob is not staged", "Mutation-Id reused for different intent":
		stored.Status = http.StatusConflict
	default:
		stored.Status = http.StatusOK
	}
	return stored, nil
}

// mutationResponse returns what a create or update stored as r answered.
func (r storedResult) mutationResponse(create bool) (MutationResponse, error) {
	switch r.Status {
	case http.StatusNotFound:
		return MutationResponse{}, ErrNotFound
	case http.StatusConflict:
		var body errorBody
		if err := json.Unmarshal(r.Body, &body); err != nil {
			return MutationResponse{}, err
		}
		switch body.Error {
		case "blob is not staged":
			return MutationResponse{}, ErrBlobNotStaged
		case ErrReceiptUsed.Error():
			return MutationResponse{}, ErrReceiptUsed
		}
		return MutationResponse{}, r.conflict()
	case http.StatusProcessing:
		return MutationResponse{}, ErrMutationPending
	default:
		return r.decodeMutationResponse(create)
	}
}

// deleteResponse returns what a delete stored as r answered.
func (r storedResult) deleteResponse() (DeleteResponse, error) {
	switch r.Status {
	case http.StatusProcessing:
		return DeleteResponse{}, ErrMutationPending
	case http.StatusConflict:
		return DeleteResponse{}, r.conflict()
	}
	var response DeleteResponse
	if err := json.Unmarshal(r.Body, &response); err != nil {
		return DeleteResponse{}, err
	}
	if r.Status == http.StatusNotFound {
		return DeleteResponse{}, ErrNotFound
	}
	return response, nil
}

func (r storedResult) conflict() error {
	var conflict Conflict
	if err := json.Unmarshal(r.Body, &conflict); err != nil {
		return err
	}
	return &ConflictError{Conflict: conflict}
}

func (r storedResult) decodeMutationResponse(create bool) (MutationResponse, error) {
	var response MutationResponse
	if err := json.Unmarshal(r.Body, &response); err != nil {
		return MutationResponse{}, err
	}
	if create {
		response.Replayed = new(true)
	}
	return response, nil
}

// mutationKind is the kind a mutation's result is recorded under.
type mutationKind string

const (
	kindCreate mutationKind = "create"
	kindUpdate mutationKind = "update"
	kindDelete mutationKind = "delete"
)

// intent is a mutation together with what a replay must match: its kind and
// requested version. A create's ObjectID is empty until the object exists.
type intent struct {
	Mutation
	kind             mutationKind
	requestedVersion *int64
}

func createIntent(m Mutation) intent {
	m.ObjectID = ""
	return intent{Mutation: m, kind: kindCreate}
}

// recorded reports whether the intent's result is kept under its Mutation ID.
// A malformed collection or object ID can never match a row, so its outcome
// is not recorded.
func (in intent) recorded() bool {
	return in.MutationID != "" && ids.IsUUID(in.CollectionID) &&
		(in.kind == kindCreate || ids.IsUUID(in.ObjectID))
}

// objectID is the object ID a result row records: NULL for a create that has
// not made its object.
func (in intent) objectID() *string {
	if in.ObjectID == "" {
		return nil
	}
	return &in.ObjectID
}

// prepare inserts the in-progress claim for the intent's Mutation ID and
// reports whether this call inserted it.
func (s *Store) prepare(ctx context.Context, in intent) (bool, error) {
	if !in.recorded() {
		return false, nil
	}
	var inserted int
	err := s.database.QueryRowContext(ctx, `INSERT INTO mutation_results
		(user_id, mutation_id, kind, collection_id, object_id, requested_version, result, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (user_id, mutation_id) DO NOTHING RETURNING 1`, in.UserID, in.MutationID,
		string(in.kind), in.CollectionID, in.objectID(), in.requestedVersion, []byte(pendingResult), db.Timestamp(time.Now())).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// begin starts the transaction a mutation runs in, serialized against every
// other request under the same Mutation ID. When the ID already carries a
// settled result, begin commits and returns that result instead of a
// transaction. prepared says this request inserted the ID's pending claim
// itself, so a pending result is its own claim rather than a settled one.
func (s *Store) begin(ctx context.Context, in intent, prepared bool) (*db.Tx, *storedResult, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	stored, settled, err := settledResult(ctx, tx, in, prepared)
	if err == nil && settled {
		err = tx.Commit()
	}
	if err != nil {
		tx.Discard()
		return nil, nil, err
	}
	if settled {
		return nil, &stored, nil
	}
	return tx, nil, nil
}

// settledResult takes the Mutation ID's lock and returns the result the ID
// already carries, if any. It returns ErrMutationMismatch when the ID was
// first used for a different intent.
func settledResult(ctx context.Context, tx *db.Tx, in intent, prepared bool) (storedResult, bool, error) {
	if in.MutationID == "" {
		return storedResult{}, false, nil
	}
	if err := tx.Dialect().LockMutation(ctx, tx, in.UserID+":"+in.MutationID); err != nil {
		return storedResult{}, false, err
	}
	var kind, collectionID string
	var objectID sql.NullString
	var requestedVersion sql.NullInt64
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT kind, collection_id, object_id, requested_version, result
		FROM mutation_results WHERE user_id = $1 AND mutation_id = $2`, in.UserID, in.MutationID).
		Scan(&kind, &collectionID, &objectID, &requestedVersion, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return storedResult{}, false, nil
	}
	if err != nil {
		return storedResult{}, false, err
	}
	matches := mutationKind(kind) == in.kind && collectionID == in.CollectionID
	if in.kind != kindCreate {
		matches = matches && objectID.Valid && objectID.String == in.ObjectID &&
			requestedVersion.Valid == (in.requestedVersion != nil) &&
			(!requestedVersion.Valid || requestedVersion.Int64 == *in.requestedVersion)
	}
	if !matches {
		return storedResult{}, false, ErrMutationMismatch
	}
	stored, err := decodeStoredResult(raw)
	if err != nil {
		return storedResult{}, false, err
	}
	if prepared && stored.Status == http.StatusProcessing {
		return storedResult{}, false, nil
	}
	return stored, true, nil
}

func saveResult(ctx context.Context, tx *db.Tx, in intent, status int, body any) error {
	if !in.recorded() {
		return nil
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return err
	}
	storedJSON, err := json.Marshal(storedResult{Status: status, Body: bodyJSON})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mutation_results
		(user_id, mutation_id, kind, collection_id, object_id, requested_version, result, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (user_id, mutation_id) DO UPDATE SET object_id = EXCLUDED.object_id,
		requested_version = EXCLUDED.requested_version, result = EXCLUDED.result`, in.UserID, in.MutationID, string(in.kind),
		in.CollectionID, in.objectID(), in.requestedVersion, storedJSON, db.Timestamp(time.Now()))
	return err
}

// settle stores status and body as the mutation's result and commits.
func settle(ctx context.Context, tx *db.Tx, in intent, status int, body any) error {
	if err := saveResult(ctx, tx, in, status, body); err != nil {
		return err
	}
	return tx.Commit()
}

// settleChange stores status and body as the mutation's result, announces
// the collection's new version, and commits.
func (s *Store) settleChange(ctx context.Context, tx *db.Tx, in intent, status int, body any, collectionVersion int64) error {
	if err := saveResult(ctx, tx, in, status, body); err != nil {
		return err
	}
	if err := s.publisher.Publish(ctx, tx, in.UserID, in.CollectionID, collectionVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// fail ends a mutation that failed with err. A domain failure is stored as
// the mutation's result and committed, so a retry replays it, and returned.
// Any other error is returned as is and leaves tx to roll back.
func fail(ctx context.Context, tx *db.Tx, in intent, err error) error {
	var status int
	var body any
	conflict, isConflict := errors.AsType[*ConflictError](err)
	switch {
	case isConflict:
		status, body = http.StatusConflict, conflict.Conflict
	case errors.Is(err, ErrBlobNotStaged):
		status, body = http.StatusConflict, errorBody{Error: "blob is not staged"}
	case errors.Is(err, ErrReceiptUsed):
		status, body = http.StatusConflict, errorBody{Error: ErrReceiptUsed.Error()}
	case errors.Is(err, ErrNotFound):
		status, body = http.StatusNotFound, errorBody{Error: "not found"}
	default:
		return err
	}
	if settleErr := settle(ctx, tx, in, status, body); settleErr != nil {
		return settleErr
	}
	return err
}

func conflictWith(current Object) *ConflictError {
	return &ConflictError{Conflict: Conflict{
		Error: "version conflict", CurrentVersion: current.Version, CurrentBlobKey: current.BlobKey,
	}}
}

// lockCollection locks the mutation's collection, or returns ErrNotFound
// unless the user owns it.
func lockCollection(ctx context.Context, tx *db.Tx, in intent) error {
	if !ids.IsUUID(in.CollectionID) {
		return ErrNotFound
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM collections WHERE id = $1 AND user_id = $2`+tx.Dialect().ForUpdate(),
		in.CollectionID, in.UserID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// lockObject locks and returns the mutation's object, or returns ErrNotFound.
func lockObject(ctx context.Context, tx *db.Tx, in intent) (Object, error) {
	if !ids.IsUUID(in.ObjectID) {
		return Object{}, ErrNotFound
	}
	current, err := scanObject(tx.QueryRowContext(ctx, `SELECT `+objectColumns+` FROM objects
		WHERE id = $1 AND collection_id = $2 AND user_id = $3`+tx.Dialect().ForUpdate(), in.ObjectID, in.CollectionID, in.UserID))
	if errors.Is(err, sql.ErrNoRows) {
		return Object{}, ErrNotFound
	}
	return current, err
}

// Blob is the blob a create or update points its object at. StagedBlob and
// ReceiptBlob are the two ways a mutation can obtain one.
type Blob struct {
	key     string
	size    int64
	receipt bool
}

// StagedBlob is a blob the user uploaded to this server, which recorded it in
// the ledger as staged. The mutation claims it only while it is still staged
// and less than 24 hours old, and fails with ErrBlobNotStaged otherwise.
func StagedBlob(key string) Blob { return Blob{key: key} }

// ReceiptBlob is a blob a storage service holds, as a receipt the caller has
// already verified describes it. The server never saw the upload, so the
// mutation records the key in the ledger itself, already claimed, and fails
// with ErrReceiptUsed when the ledger knows the key.
func ReceiptBlob(key string, size int64) Blob { return Blob{key: key, size: size, receipt: true} }

// take reserves the blob for the mutation in tx and returns its size.
func (b Blob) take(ctx context.Context, tx *db.Tx, userID string) (int64, error) {
	if !b.receipt {
		return stagedBlobSize(ctx, tx, userID, b.key)
	}
	// Two mutations spending the same receipt race on the primary key: the
	// loser waits for the winner's transaction and then inserts nothing.
	var inserted int
	now := db.Timestamp(time.Now())
	err := tx.QueryRowContext(ctx, `INSERT INTO blob_ledger
		(blob_key, user_id, size_bytes, state, created_at, state_changed_at)
		VALUES ($1, $2, $3, 'claimed', $4, $4)
		ON CONFLICT (blob_key) DO NOTHING RETURNING 1`, b.key, userID, b.size, now).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrReceiptUsed
	}
	return b.size, err
}

// stagedBlobSize locks the user's staged blob and returns its size, or
// returns ErrBlobNotStaged.
func stagedBlobSize(ctx context.Context, tx *db.Tx, userID, blobKey string) (int64, error) {
	var size int64
	err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM blob_ledger
		WHERE blob_key = $1 AND user_id = $2 AND state = 'staged'
		AND created_at > $3`+tx.Dialect().ForUpdate(), blobKey, userID, db.Timestamp(time.Now().Add(-24*time.Hour))).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrBlobNotStaged
	}
	return size, err
}

func bumpCollectionVersion(ctx context.Context, tx *db.Tx, collectionID string) (int64, error) {
	var collectionVersion int64
	err := tx.QueryRowContext(ctx, `UPDATE collections SET current_version = current_version + 1
		WHERE id = $1 RETURNING current_version`, collectionID).Scan(&collectionVersion)
	return collectionVersion, err
}

// releaseBlob moves a superseded blob from claimed to retained. A claimed
// blob is never eligible for garbage collection; a retained one still reads
// back from disk, so it can serve as a merge ancestor, and starts the
// 365-day retention clock.
func releaseBlob(ctx context.Context, tx *db.Tx, blobKey *string, now string) error {
	if blobKey == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE blob_ledger SET state = 'retained', object_id = NULL,
		object_version = NULL, state_changed_at = $2 WHERE blob_key = $1 AND state = 'claimed'`, *blobKey, now)
	return err
}

// Reservation is the in-progress claim a create Mutation ID holds while its
// request runs. A non-nil Replay means the ID already completed a create:
// Replay is the answer, and the caller must neither stage ciphertext nor call
// CreateReserved.
type Reservation struct {
	Replay   *MutationResponse
	mutation Mutation
	prepared bool
}

// ReserveCreate records the in-progress claim for a create Mutation ID, or
// reports the outcome the ID already carries: a completed create as
// Reservation.Replay, a failed one as its error. A caller that has to stage
// ciphertext before it can name a blob key reserves the ID first: recovery
// then sees a pending claim instead of a missing one while the create
// commits, and a replay of a completed create is answered without staging a
// second blob. Pass the reservation on to CreateReserved.
func (s *Store) ReserveCreate(ctx context.Context, m Mutation) (Reservation, error) {
	in := createIntent(m)
	prepared, err := s.prepare(ctx, in)
	if err != nil {
		return Reservation{}, err
	}
	if prepared || !in.recorded() {
		return Reservation{mutation: in.Mutation, prepared: prepared}, nil
	}
	tx, stored, err := s.begin(ctx, in, false)
	if err != nil {
		return Reservation{}, err
	}
	// A row that has since expired leaves nothing to replay, so the create
	// runs as if the ID were fresh.
	if stored == nil {
		tx.Discard()
		return Reservation{mutation: in.Mutation}, nil
	}
	response, err := stored.mutationResponse(true)
	if err != nil {
		return Reservation{}, err
	}
	return Reservation{Replay: &response, mutation: in.Mutation}, nil
}

// ReplayCreate returns the outcome a create Mutation ID already carries,
// without claiming the ID: a completed create as the response with replayed
// set, a failed one as its error. replayed is false when the ID carries no
// result, so a create would run. A caller whose blob still needs checking
// asks first, so a retry gets its original answer whatever the blob now is.
func (s *Store) ReplayCreate(ctx context.Context, m Mutation) (response MutationResponse, replayed bool, err error) {
	stored, found, err := s.storedResult(ctx, createIntent(m))
	if err != nil || !found {
		return MutationResponse{}, false, err
	}
	response, err = stored.mutationResponse(true)
	return response, true, err
}

// ReplayUpdate is ReplayCreate for an update to version.
func (s *Store) ReplayUpdate(ctx context.Context, m Mutation, version int64) (response MutationResponse, replayed bool, err error) {
	stored, found, err := s.storedResult(ctx, intent{Mutation: m, kind: kindUpdate, requestedVersion: &version})
	if err != nil || !found {
		return MutationResponse{}, false, err
	}
	response, err = stored.mutationResponse(false)
	return response, true, err
}

// storedResult returns the result the intent's Mutation ID carries, and
// whether it carries one. A pending result counts: the request that claimed
// the ID is still running.
func (s *Store) storedResult(ctx context.Context, in intent) (storedResult, bool, error) {
	if in.MutationID == "" {
		return storedResult{}, false, nil
	}
	tx, stored, err := s.begin(ctx, in, false)
	if err != nil {
		return storedResult{}, false, err
	}
	if tx != nil {
		tx.Discard()
		return storedResult{}, false, nil
	}
	return *stored, true, nil
}

// Create records a new object for blob.
func (s *Store) Create(ctx context.Context, m Mutation, blob Blob) (MutationResponse, error) {
	reservation, err := s.ReserveCreate(ctx, m)
	if err != nil {
		return MutationResponse{}, err
	}
	if reservation.Replay != nil {
		return *reservation.Replay, nil
	}
	return s.CreateReserved(ctx, reservation, blob)
}

// CreateReserved records a new object for blob under the claim ReserveCreate
// took.
func (s *Store) CreateReserved(ctx context.Context, reservation Reservation, blob Blob) (MutationResponse, error) {
	in := createIntent(reservation.mutation)
	tx, stored, err := s.begin(ctx, in, reservation.prepared)
	if err != nil {
		return MutationResponse{}, err
	}
	if stored != nil {
		return stored.mutationResponse(true)
	}
	defer tx.Discard()
	if err := lockCollection(ctx, tx, in); err != nil {
		return MutationResponse{}, fail(ctx, tx, in, err)
	}
	size, err := blob.take(ctx, tx, in.UserID)
	if err != nil {
		return MutationResponse{}, fail(ctx, tx, in, err)
	}
	collectionVersion, err := bumpCollectionVersion(ctx, tx, in.CollectionID)
	if err != nil {
		return MutationResponse{}, err
	}
	objectID := uuid.NewV7().String()
	now := db.Timestamp(time.Now())
	o, err := scanObject(tx.QueryRowContext(ctx, `INSERT INTO objects
		(id, collection_id, user_id, version, change_seq, blob_key, size_bytes, created_at, updated_at)
		VALUES ($1, $2, $3, 1, $4, $5, $6, $7, $7) RETURNING `+objectColumns,
		objectID, in.CollectionID, in.UserID, collectionVersion, blob.key, size, now))
	if err != nil {
		return MutationResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE blob_ledger SET state = 'claimed', collection_id = $2,
		object_id = $3, object_version = 1, state_changed_at = $4 WHERE blob_key = $1`, blob.key, in.CollectionID, objectID, now); err != nil {
		return MutationResponse{}, err
	}
	// The stored result names the object the create made.
	in.ObjectID = objectID
	response := MutationResponse{Object: o, CollectionVersion: collectionVersion, Replayed: new(false)}
	if err := s.settleChange(ctx, tx, in, http.StatusCreated, response, collectionVersion); err != nil {
		return MutationResponse{}, err
	}
	return response, nil
}

// Update points the object at blob. version must be exactly one more than the
// object's current version.
func (s *Store) Update(ctx context.Context, m Mutation, blob Blob, version int64) (MutationResponse, error) {
	in := intent{Mutation: m, kind: kindUpdate, requestedVersion: &version}
	prepared, err := s.prepare(ctx, in)
	if err != nil {
		return MutationResponse{}, err
	}
	tx, stored, err := s.begin(ctx, in, prepared)
	if err != nil {
		return MutationResponse{}, err
	}
	if stored != nil {
		return stored.mutationResponse(false)
	}
	defer tx.Discard()
	if err := lockCollection(ctx, tx, in); err != nil {
		return MutationResponse{}, fail(ctx, tx, in, err)
	}
	current, err := lockObject(ctx, tx, in)
	if err != nil {
		return MutationResponse{}, fail(ctx, tx, in, err)
	}
	if version != current.Version+1 {
		return MutationResponse{}, fail(ctx, tx, in, conflictWith(current))
	}
	size, err := blob.take(ctx, tx, in.UserID)
	if err != nil {
		return MutationResponse{}, fail(ctx, tx, in, err)
	}
	collectionVersion, err := bumpCollectionVersion(ctx, tx, in.CollectionID)
	if err != nil {
		return MutationResponse{}, err
	}
	now := db.Timestamp(time.Now())
	o, err := scanObject(tx.QueryRowContext(ctx, `UPDATE objects SET version = $2, change_seq = $3,
		blob_key = $4, size_bytes = $5, updated_at = $6 WHERE id = $1 RETURNING `+objectColumns,
		in.ObjectID, version, collectionVersion, blob.key, size, now))
	if err != nil {
		return MutationResponse{}, err
	}
	if err := releaseBlob(ctx, tx, current.BlobKey, now); err != nil {
		return MutationResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE blob_ledger SET state = 'claimed', collection_id = $2,
		object_id = $3, object_version = $4, state_changed_at = $5 WHERE blob_key = $1`, blob.key, in.CollectionID, in.ObjectID, version, now); err != nil {
		return MutationResponse{}, err
	}
	response := MutationResponse{Object: o, CollectionVersion: collectionVersion}
	if err := s.settleChange(ctx, tx, in, http.StatusOK, response, collectionVersion); err != nil {
		return MutationResponse{}, err
	}
	return response, nil
}

// Delete turns the object into a tombstone. With expectedVersion set, the
// object must currently be at that version. Deleting a tombstone again
// answers with the tombstone and changes nothing.
func (s *Store) Delete(ctx context.Context, m Mutation, expectedVersion *int64) (DeleteResponse, error) {
	in := intent{Mutation: m, kind: kindDelete, requestedVersion: expectedVersion}
	prepared, err := s.prepare(ctx, in)
	if err != nil {
		return DeleteResponse{}, err
	}
	tx, stored, err := s.begin(ctx, in, prepared)
	if err != nil {
		return DeleteResponse{}, err
	}
	if stored != nil {
		return stored.deleteResponse()
	}
	defer tx.Discard()
	if err := lockCollection(ctx, tx, in); err != nil {
		return DeleteResponse{}, fail(ctx, tx, in, err)
	}
	current, err := lockObject(ctx, tx, in)
	if err != nil {
		return DeleteResponse{}, fail(ctx, tx, in, err)
	}
	// The race guard applies to tombstones too: a stale ?version must 409
	// before the re-delete no-op below.
	if expectedVersion != nil && *expectedVersion != current.Version {
		return DeleteResponse{}, fail(ctx, tx, in, conflictWith(current))
	}
	if current.Deleted {
		var collectionVersion int64
		if err := tx.QueryRowContext(ctx,
			`SELECT current_version FROM collections WHERE id = $1`, in.CollectionID).Scan(&collectionVersion); err != nil {
			return DeleteResponse{}, err
		}
		response := DeleteResponse{
			Object: DeletedObject{
				ID:        current.ID,
				Version:   current.Version,
				ChangeSeq: current.ChangeSeq,
				Deleted:   true,
			},
			CollectionVersion: collectionVersion,
		}
		if err := settle(ctx, tx, in, http.StatusOK, response); err != nil {
			return DeleteResponse{}, err
		}
		return response, nil
	}
	collectionVersion, err := bumpCollectionVersion(ctx, tx, in.CollectionID)
	if err != nil {
		return DeleteResponse{}, err
	}
	var deleted DeletedObject
	now := db.Timestamp(time.Now())
	if err := tx.QueryRowContext(ctx, `UPDATE objects SET deleted = true, version = $2,
		change_seq = $3, updated_at = $4 WHERE id = $1 RETURNING id, version, change_seq, deleted`,
		in.ObjectID, current.Version+1, collectionVersion, now).Scan(&deleted.ID, &deleted.Version, &deleted.ChangeSeq, &deleted.Deleted); err != nil {
		return DeleteResponse{}, err
	}
	// Release the claim the way an update does, or the tombstone would hold
	// its ciphertext on disk forever.
	if err := releaseBlob(ctx, tx, current.BlobKey, now); err != nil {
		return DeleteResponse{}, err
	}
	response := DeleteResponse{Object: deleted, CollectionVersion: collectionVersion}
	if err := s.settleChange(ctx, tx, in, http.StatusOK, response, collectionVersion); err != nil {
		return DeleteResponse{}, err
	}
	return response, nil
}

// RecoverCreate returns the outcome of the create recorded under mutationID:
// the stored response with Replayed set, ErrMutationPending while it runs,
// or ErrNotFound when there is no such create or it failed.
func (s *Store) RecoverCreate(ctx context.Context, userID, collectionID, mutationID string) (MutationResponse, error) {
	if err := s.CheckCollection(ctx, userID, collectionID); err != nil {
		return MutationResponse{}, err
	}
	var kind string
	var raw []byte
	err := s.database.QueryRowContext(ctx, `SELECT kind, result FROM mutation_results
		WHERE user_id = $1 AND mutation_id = $2 AND collection_id = $3`, userID, mutationID, collectionID).Scan(&kind, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return MutationResponse{}, ErrNotFound
	}
	if err != nil {
		return MutationResponse{}, err
	}
	if mutationKind(kind) != kindCreate {
		return MutationResponse{}, ErrNotFound
	}
	stored, err := decodeStoredResult(raw)
	if err != nil {
		return MutationResponse{}, err
	}
	switch {
	case stored.Status == http.StatusProcessing:
		return MutationResponse{}, ErrMutationPending
	case stored.Status == http.StatusNotFound, stored.Status == http.StatusConflict:
		return MutationResponse{}, ErrNotFound
	case stored.Status < http.StatusOK || stored.Status >= http.StatusMultipleChoices:
		// No status this server or its predecessor records lands here; an
		// unknown one recovers as an empty success, as it always has.
		return MutationResponse{}, nil
	}
	return stored.decodeMutationResponse(true)
}
