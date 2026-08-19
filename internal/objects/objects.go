// Package objects owns versioned object metadata and coordinates its blob ledger.
package objects

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"
	"uuid"
)

const objectColumns = `id, collection_id, version, change_seq, deleted,
	blob_key, size_bytes, created_at, updated_at`

var mutationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

var ErrNotFound = errors.New("not found")

type Object struct {
	ID           string    `json:"id"`
	CollectionID string    `json:"collection_id"`
	Version      string    `json:"version"`
	ChangeSeq    string    `json:"change_seq"`
	Deleted      bool      `json:"deleted"`
	BlobKey      *string   `json:"blob_key"`
	SizeBytes    *string   `json:"size_bytes"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type DeletedObject struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	ChangeSeq string `json:"change_seq"`
	Deleted   bool   `json:"deleted"`
}

type MutationResponse struct {
	Object            Object `json:"object"`
	CollectionVersion int64  `json:"collectionVersion"`
	Replayed          *bool  `json:"replayed,omitempty"`
}

type DeleteResponse struct {
	Object            DeletedObject `json:"object"`
	CollectionVersion int64         `json:"collectionVersion"`
}

type Conflict struct {
	Error          string  `json:"error"`
	CurrentVersion int64   `json:"currentVersion"`
	CurrentBlobKey *string `json:"currentBlobKey"`
}

type ResultCode int

const (
	OK ResultCode = iota
	NotFound
	VersionConflict
	BlobNotStaged
	MutationMismatch
	MutationPending
)

type MutationOutcome struct {
	Code     ResultCode
	Response MutationResponse
	Conflict Conflict
}

type DeleteOutcome struct {
	Code     ResultCode
	Response DeleteResponse
	Conflict Conflict
}

type scanner interface{ Scan(...any) error }

func scanObject(row scanner) (Object, error) {
	var o Object
	var version, changeSeq int64
	var blobKey sql.NullString
	var size sql.NullInt64
	if err := row.Scan(&o.ID, &o.CollectionID, &version, &changeSeq, &o.Deleted,
		&blobKey, &size, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return Object{}, err
	}
	o.Version = strconv.FormatInt(version, 10)
	o.ChangeSeq = strconv.FormatInt(changeSeq, 10)
	if blobKey.Valid {
		o.BlobKey = &blobKey.String
	}
	if size.Valid {
		s := strconv.FormatInt(size.Int64, 10)
		o.SizeBytes = &s
	}
	o.CreatedAt = o.CreatedAt.UTC()
	o.UpdatedAt = o.UpdatedAt.UTC()
	return o, nil
}

func ValidMutationID(id string) bool { return mutationIDPattern.MatchString(id) }

func ValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range []byte(s) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

func ValidBlobKey(userID, key string) bool {
	return len(userID) == 36 && len(key) == 73 && key[:36] == userID && key[36] == '/' && ValidUUID(key[37:])
}

func Exists(ctx context.Context, db *sql.DB, userID, collectionID string) (bool, error) {
	if !ValidUUID(collectionID) {
		return false, nil
	}
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM collections WHERE id = $1 AND user_id = $2`, collectionID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func List(ctx context.Context, db *sql.DB, userID, collectionID string, since int64, limit *int) ([]Object, bool, error) {
	found, err := Exists(ctx, db, userID, collectionID)
	if err != nil || !found {
		if err == nil {
			err = ErrNotFound
		}
		return nil, false, err
	}
	query := `SELECT ` + objectColumns + ` FROM objects
		WHERE collection_id = $1 AND user_id = $2 AND change_seq > $3 ORDER BY change_seq, id`
	args := []any{collectionID, userID, since}
	if limit != nil {
		query += ` LIMIT $4`
		args = append(args, *limit+1)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
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

func Get(ctx context.Context, db *sql.DB, userID, collectionID, objectID string) (*Object, error) {
	if !ValidUUID(collectionID) || !ValidUUID(objectID) {
		return nil, nil
	}
	o, err := scanObject(db.QueryRowContext(ctx, `SELECT `+objectColumns+` FROM objects
		WHERE id = $1 AND collection_id = $2 AND user_id = $3`, objectID, collectionID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

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
			pendingBody, marshalErr := json.Marshal(map[string]string{"error": "mutation still in progress"})
			return storedResult{Status: 102, Body: pendingBody}, marshalErr
		}
		return stored, err
	}
	if stored.Status != 0 && len(stored.Body) != 0 {
		return stored, nil
	}
	// Accept unwrapped result bodies so durable outcomes written by an older
	// deployment remain recoverable after upgrading.
	stored.Body = append(json.RawMessage(nil), raw...)
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return stored, err
	}
	switch body.Error {
	case "not found":
		stored.Status = 404
	case "version conflict", "blob is not staged", "Mutation-Id reused for different intent":
		stored.Status = 409
	default:
		stored.Status = 200
	}
	return stored, nil
}

type mutationIntent struct {
	Kind             string
	CollectionID     string
	ObjectID         *string
	RequestedVersion *int64
}

func lockMutation(ctx context.Context, tx *sql.Tx, userID, mutationID string) error {
	if mutationID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, userID+":"+mutationID)
	return err
}

func replay(ctx context.Context, tx *sql.Tx, userID, mutationID string, intent mutationIntent) (*storedResult, bool, error) {
	if mutationID == "" {
		return nil, false, nil
	}
	var kind, collectionID string
	var objectID sql.NullString
	var requestedVersion sql.NullInt64
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT kind, collection_id, object_id, requested_version, result
		FROM mutation_results WHERE user_id = $1 AND mutation_id = $2`, userID, mutationID).
		Scan(&kind, &collectionID, &objectID, &requestedVersion, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	matches := kind == intent.Kind && collectionID == intent.CollectionID
	if intent.Kind != "create" {
		matches = matches && nullableStringEqual(objectID, intent.ObjectID) && nullableIntEqual(requestedVersion, intent.RequestedVersion)
	}
	if !matches {
		return nil, true, nil
	}
	stored, err := decodeStoredResult(raw)
	if err != nil {
		return nil, false, err
	}
	return &stored, false, nil
}

func prepareMutation(ctx context.Context, db *sql.DB, userID, mutationID string, intent mutationIntent) (bool, error) {
	if mutationID == "" || !ValidUUID(intent.CollectionID) ||
		(intent.ObjectID != nil && !ValidUUID(*intent.ObjectID)) {
		return false, nil
	}
	pendingBody, err := json.Marshal(map[string]string{"error": "mutation still in progress"})
	if err != nil {
		return false, err
	}
	pending, err := json.Marshal(storedResult{Status: 102, Body: pendingBody})
	if err != nil {
		return false, err
	}
	var inserted int
	err = db.QueryRowContext(ctx, `INSERT INTO mutation_results
		(user_id, mutation_id, kind, collection_id, object_id, requested_version, result)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, mutation_id) DO NOTHING RETURNING 1`, userID, mutationID,
		intent.Kind, intent.CollectionID, intent.ObjectID, intent.RequestedVersion, pending).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func nullableStringEqual(got sql.NullString, want *string) bool {
	return got.Valid == (want != nil) && (!got.Valid || got.String == *want)
}

func nullableIntEqual(got sql.NullInt64, want *int64) bool {
	return got.Valid == (want != nil) && (!got.Valid || got.Int64 == *want)
}

func saveResult(ctx context.Context, tx *sql.Tx, userID, mutationID string, intent mutationIntent, status int, body any, resultingObjectID *string) error {
	if mutationID == "" || !ValidUUID(intent.CollectionID) ||
		(intent.ObjectID != nil && !ValidUUID(*intent.ObjectID)) {
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
	objectID := intent.ObjectID
	if intent.Kind == "create" {
		objectID = resultingObjectID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mutation_results
		(user_id, mutation_id, kind, collection_id, object_id, requested_version, result)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, mutation_id) DO UPDATE SET object_id = EXCLUDED.object_id,
		requested_version = EXCLUDED.requested_version, result = EXCLUDED.result`, userID, mutationID, intent.Kind,
		intent.CollectionID, objectID, intent.RequestedVersion, storedJSON)
	return err
}

func lockCollection(ctx context.Context, tx *sql.Tx, userID, collectionID string) (bool, error) {
	if !ValidUUID(collectionID) {
		return false, nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM collections WHERE id = $1 AND user_id = $2 FOR UPDATE`, collectionID, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func stagedBlobSize(ctx context.Context, tx *sql.Tx, userID, blobKey string) (int64, bool, error) {
	var size int64
	err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM blob_ledger
		WHERE blob_key = $1 AND user_id = $2 AND state = 'staged'
		AND created_at > now() - interval '24 hours' FOR UPDATE`, blobKey, userID).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return size, err == nil, err
}

func decodeMutationResponse(stored *storedResult, create bool) (MutationResponse, error) {
	var response MutationResponse
	if err := json.Unmarshal(stored.Body, &response); err != nil {
		return response, err
	}
	if create {
		replayed := true
		response.Replayed = &replayed
	}
	return response, nil
}

func mutationOutcomeFromStored(stored *storedResult, create bool) (MutationOutcome, error) {
	switch stored.Status {
	case 404:
		return MutationOutcome{Code: NotFound}, nil
	case 409:
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(stored.Body, &body); err != nil {
			return MutationOutcome{}, err
		}
		if body.Error == "blob is not staged" {
			return MutationOutcome{Code: BlobNotStaged}, nil
		}
		var conflict Conflict
		if err := json.Unmarshal(stored.Body, &conflict); err != nil {
			return MutationOutcome{}, err
		}
		return MutationOutcome{Code: VersionConflict, Conflict: conflict}, nil
	case 102:
		return MutationOutcome{Code: MutationPending}, nil
	default:
		response, err := decodeMutationResponse(stored, create)
		if err != nil {
			return MutationOutcome{}, err
		}
		return MutationOutcome{Code: OK, Response: response}, nil
	}
}

func Create(ctx context.Context, db *sql.DB, userID, collectionID, mutationID, blobKey string) (MutationOutcome, error) {
	intent := mutationIntent{Kind: "create", CollectionID: collectionID}
	prepared, err := prepareMutation(ctx, db, userID, mutationID, intent)
	if err != nil {
		return MutationOutcome{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MutationOutcome{}, err
	}
	defer tx.Rollback()
	if err := lockMutation(ctx, tx, userID, mutationID); err != nil {
		return MutationOutcome{}, err
	}
	stored, mismatch, err := replay(ctx, tx, userID, mutationID, intent)
	if err != nil {
		return MutationOutcome{}, err
	}
	if mismatch {
		return MutationOutcome{Code: MutationMismatch}, nil
	}
	if prepared && stored != nil && stored.Status == 102 {
		stored = nil
	}
	if stored != nil {
		outcome, err := mutationOutcomeFromStored(stored, true)
		if err != nil {
			return MutationOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return MutationOutcome{}, err
		}
		return outcome, nil
	}
	found, err := lockCollection(ctx, tx, userID, collectionID)
	if err != nil {
		return MutationOutcome{}, err
	}
	if !found {
		if err := saveResult(ctx, tx, userID, mutationID, intent, 404, map[string]string{"error": "not found"}, nil); err != nil {
			return MutationOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return MutationOutcome{}, err
		}
		return MutationOutcome{Code: NotFound}, nil
	}
	size, staged, err := stagedBlobSize(ctx, tx, userID, blobKey)
	if err != nil {
		return MutationOutcome{}, err
	}
	if !staged {
		body := map[string]string{"error": "blob is not staged"}
		if err := saveResult(ctx, tx, userID, mutationID, intent, 409, body, nil); err != nil {
			return MutationOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return MutationOutcome{}, err
		}
		return MutationOutcome{Code: BlobNotStaged}, nil
	}
	var collectionVersion int64
	if err := tx.QueryRowContext(ctx, `UPDATE collections SET current_version = current_version + 1
		WHERE id = $1 RETURNING current_version`, collectionID).Scan(&collectionVersion); err != nil {
		return MutationOutcome{}, err
	}
	objectID := uuid.NewV7().String()
	o, err := scanObject(tx.QueryRowContext(ctx, `INSERT INTO objects
		(id, collection_id, user_id, version, change_seq, blob_key, size_bytes)
		VALUES ($1, $2, $3, 1, $4, $5, $6) RETURNING `+objectColumns,
		objectID, collectionID, userID, collectionVersion, blobKey, size))
	if err != nil {
		return MutationOutcome{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE blob_ledger SET state = 'claimed', collection_id = $2,
		object_id = $3, object_version = 1, state_changed_at = now() WHERE blob_key = $1`, blobKey, collectionID, objectID); err != nil {
		return MutationOutcome{}, err
	}
	replayed := false
	response := MutationResponse{Object: o, CollectionVersion: collectionVersion, Replayed: &replayed}
	if err := saveResult(ctx, tx, userID, mutationID, intent, 201, response, &objectID); err != nil {
		return MutationOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return MutationOutcome{}, err
	}
	return MutationOutcome{Code: OK, Response: response}, nil
}

func resultCode(status int) ResultCode {
	switch status {
	case 404:
		return NotFound
	case 409:
		return VersionConflict
	default:
		return OK
	}
}

func Update(ctx context.Context, db *sql.DB, userID, collectionID, objectID, mutationID, blobKey string, version int64) (MutationOutcome, error) {
	intent := mutationIntent{Kind: "update", CollectionID: collectionID, ObjectID: &objectID, RequestedVersion: &version}
	prepared, err := prepareMutation(ctx, db, userID, mutationID, intent)
	if err != nil {
		return MutationOutcome{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MutationOutcome{}, err
	}
	defer tx.Rollback()
	if err := lockMutation(ctx, tx, userID, mutationID); err != nil {
		return MutationOutcome{}, err
	}
	stored, mismatch, err := replay(ctx, tx, userID, mutationID, intent)
	if err != nil {
		return MutationOutcome{}, err
	}
	if mismatch {
		return MutationOutcome{Code: MutationMismatch}, nil
	}
	if prepared && stored != nil && stored.Status == 102 {
		stored = nil
	}
	if stored != nil {
		outcome, err := mutationOutcomeFromStored(stored, false)
		if err != nil {
			return MutationOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return MutationOutcome{}, err
		}
		return outcome, nil
	}
	found, err := lockCollection(ctx, tx, userID, collectionID)
	if err != nil {
		return MutationOutcome{}, err
	}
	if !found {
		return finishMutationNotFound(ctx, tx, userID, mutationID, intent)
	}
	if !ValidUUID(objectID) {
		return finishMutationNotFound(ctx, tx, userID, mutationID, intent)
	}
	current, err := scanObject(tx.QueryRowContext(ctx, `SELECT `+objectColumns+` FROM objects
		WHERE id = $1 AND collection_id = $2 AND user_id = $3 FOR UPDATE`, objectID, collectionID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return finishMutationNotFound(ctx, tx, userID, mutationID, intent)
	}
	if err != nil {
		return MutationOutcome{}, err
	}
	currentVersion, _ := strconv.ParseInt(current.Version, 10, 64)
	if version != currentVersion+1 {
		conflict := Conflict{Error: "version conflict", CurrentVersion: currentVersion, CurrentBlobKey: current.BlobKey}
		if err := saveResult(ctx, tx, userID, mutationID, intent, 409, conflict, nil); err != nil {
			return MutationOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return MutationOutcome{}, err
		}
		return MutationOutcome{Code: VersionConflict, Conflict: conflict}, nil
	}
	size, staged, err := stagedBlobSize(ctx, tx, userID, blobKey)
	if err != nil {
		return MutationOutcome{}, err
	}
	if !staged {
		if err := saveResult(ctx, tx, userID, mutationID, intent, 409, map[string]string{"error": "blob is not staged"}, nil); err != nil {
			return MutationOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return MutationOutcome{}, err
		}
		return MutationOutcome{Code: BlobNotStaged}, nil
	}
	var collectionVersion int64
	if err := tx.QueryRowContext(ctx, `UPDATE collections SET current_version = current_version + 1 WHERE id = $1 RETURNING current_version`, collectionID).Scan(&collectionVersion); err != nil {
		return MutationOutcome{}, err
	}
	o, err := scanObject(tx.QueryRowContext(ctx, `UPDATE objects SET version = $2, change_seq = $3,
		blob_key = $4, size_bytes = $5, updated_at = now() WHERE id = $1 RETURNING `+objectColumns,
		objectID, version, collectionVersion, blobKey, size))
	if err != nil {
		return MutationOutcome{}, err
	}
	if current.BlobKey != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE blob_ledger SET state = 'retained', object_id = NULL,
			object_version = NULL, state_changed_at = now() WHERE blob_key = $1 AND state = 'claimed'`, *current.BlobKey); err != nil {
			return MutationOutcome{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE blob_ledger SET state = 'claimed', collection_id = $2,
		object_id = $3, object_version = $4, state_changed_at = now() WHERE blob_key = $1`, blobKey, collectionID, objectID, version); err != nil {
		return MutationOutcome{}, err
	}
	response := MutationResponse{Object: o, CollectionVersion: collectionVersion}
	if err := saveResult(ctx, tx, userID, mutationID, intent, 200, response, nil); err != nil {
		return MutationOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return MutationOutcome{}, err
	}
	return MutationOutcome{Code: OK, Response: response}, nil
}

func finishMutationNotFound(ctx context.Context, tx *sql.Tx, userID, mutationID string, intent mutationIntent) (MutationOutcome, error) {
	if err := saveResult(ctx, tx, userID, mutationID, intent, 404, map[string]string{"error": "not found"}, nil); err != nil {
		return MutationOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return MutationOutcome{}, err
	}
	return MutationOutcome{Code: NotFound}, nil
}

func Delete(ctx context.Context, db *sql.DB, userID, collectionID, objectID, mutationID string, expectedVersion *int64) (DeleteOutcome, error) {
	intent := mutationIntent{Kind: "delete", CollectionID: collectionID, ObjectID: &objectID, RequestedVersion: expectedVersion}
	prepared, err := prepareMutation(ctx, db, userID, mutationID, intent)
	if err != nil {
		return DeleteOutcome{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteOutcome{}, err
	}
	defer tx.Rollback()
	if err := lockMutation(ctx, tx, userID, mutationID); err != nil {
		return DeleteOutcome{}, err
	}
	stored, mismatch, err := replay(ctx, tx, userID, mutationID, intent)
	if err != nil {
		return DeleteOutcome{}, err
	}
	if mismatch {
		return DeleteOutcome{Code: MutationMismatch}, nil
	}
	if prepared && stored != nil && stored.Status == 102 {
		stored = nil
	}
	if stored != nil {
		if stored.Status == 102 {
			if err := tx.Commit(); err != nil {
				return DeleteOutcome{}, err
			}
			return DeleteOutcome{Code: MutationPending}, nil
		}
		if stored.Status == 409 {
			var conflict Conflict
			if err := json.Unmarshal(stored.Body, &conflict); err != nil {
				return DeleteOutcome{}, err
			}
			if err := tx.Commit(); err != nil {
				return DeleteOutcome{}, err
			}
			return DeleteOutcome{Code: VersionConflict, Conflict: conflict}, nil
		}
		var response DeleteResponse
		if err := json.Unmarshal(stored.Body, &response); err != nil {
			return DeleteOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return DeleteOutcome{}, err
		}
		return DeleteOutcome{Code: resultCode(stored.Status), Response: response}, nil
	}
	found, err := lockCollection(ctx, tx, userID, collectionID)
	if err != nil {
		return DeleteOutcome{}, err
	}
	if !found || !ValidUUID(objectID) {
		return finishDeleteNotFound(ctx, tx, userID, mutationID, intent)
	}
	current, err := scanObject(tx.QueryRowContext(ctx, `SELECT `+objectColumns+` FROM objects
		WHERE id = $1 AND collection_id = $2 AND user_id = $3 FOR UPDATE`, objectID, collectionID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return finishDeleteNotFound(ctx, tx, userID, mutationID, intent)
	}
	if err != nil {
		return DeleteOutcome{}, err
	}
	currentVersion, _ := strconv.ParseInt(current.Version, 10, 64)
	// A tombstone can't lose the edit-vs-delete race, so a stale version is fine there.
	if expectedVersion != nil && !current.Deleted && *expectedVersion != currentVersion {
		conflict := Conflict{Error: "version conflict", CurrentVersion: currentVersion, CurrentBlobKey: current.BlobKey}
		if err := saveResult(ctx, tx, userID, mutationID, intent, 409, conflict, nil); err != nil {
			return DeleteOutcome{}, err
		}
		if err := tx.Commit(); err != nil {
			return DeleteOutcome{}, err
		}
		return DeleteOutcome{Code: VersionConflict, Conflict: conflict}, nil
	}
	var collectionVersion int64
	if err := tx.QueryRowContext(ctx, `UPDATE collections SET current_version = current_version + 1 WHERE id = $1 RETURNING current_version`, collectionID).Scan(&collectionVersion); err != nil {
		return DeleteOutcome{}, err
	}
	newVersion := currentVersion + 1
	var deleted DeletedObject
	var version, changeSeq int64
	if err := tx.QueryRowContext(ctx, `UPDATE objects SET deleted = true, version = $2,
		change_seq = $3, updated_at = now() WHERE id = $1 RETURNING id, version, change_seq, deleted`,
		objectID, newVersion, collectionVersion).Scan(&deleted.ID, &version, &changeSeq, &deleted.Deleted); err != nil {
		return DeleteOutcome{}, err
	}
	deleted.Version, deleted.ChangeSeq = strconv.FormatInt(version, 10), strconv.FormatInt(changeSeq, 10)
	if current.BlobKey != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE blob_ledger SET object_version = $2, state_changed_at = now()
			WHERE blob_key = $1 AND state = 'claimed'`, *current.BlobKey, newVersion); err != nil {
			return DeleteOutcome{}, err
		}
	}
	response := DeleteResponse{Object: deleted, CollectionVersion: collectionVersion}
	if err := saveResult(ctx, tx, userID, mutationID, intent, 200, response, nil); err != nil {
		return DeleteOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeleteOutcome{}, err
	}
	return DeleteOutcome{Code: OK, Response: response}, nil
}

func finishDeleteNotFound(ctx context.Context, tx *sql.Tx, userID, mutationID string, intent mutationIntent) (DeleteOutcome, error) {
	if err := saveResult(ctx, tx, userID, mutationID, intent, 404, map[string]string{"error": "not found"}, nil); err != nil {
		return DeleteOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeleteOutcome{}, err
	}
	return DeleteOutcome{Code: NotFound}, nil
}

func RecoverCreate(ctx context.Context, db *sql.DB, userID, collectionID, mutationID string) (MutationOutcome, error) {
	found, err := Exists(ctx, db, userID, collectionID)
	if err != nil || !found {
		return MutationOutcome{Code: NotFound}, err
	}
	var kind string
	var raw []byte
	err = db.QueryRowContext(ctx, `SELECT kind, result FROM mutation_results
		WHERE user_id = $1 AND mutation_id = $2 AND collection_id = $3`, userID, mutationID, collectionID).Scan(&kind, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return MutationOutcome{Code: NotFound}, nil
	}
	if err != nil {
		return MutationOutcome{}, err
	}
	if kind != "create" {
		return MutationOutcome{Code: NotFound}, nil
	}
	stored, err := decodeStoredResult(raw)
	if err != nil {
		return MutationOutcome{}, err
	}
	if stored.Status == 102 {
		return MutationOutcome{Code: MutationPending}, nil
	}
	if stored.Status < 200 || stored.Status >= 300 {
		return MutationOutcome{Code: resultCode(stored.Status)}, nil
	}
	response, err := decodeMutationResponse(&stored, true)
	if err != nil {
		return MutationOutcome{}, err
	}
	return MutationOutcome{Code: OK, Response: response}, nil
}
