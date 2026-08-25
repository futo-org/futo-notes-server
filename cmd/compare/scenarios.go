package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"futo-notes-server/internal/config"
	appdb "futo-notes-server/internal/db"
)

const (
	phUserA       = "00000000-0000-0000-0000-000000000001"
	phUserB       = "00000000-0000-0000-0000-000000000002"
	phCollectionA = "00000000-0000-0000-0000-000000000010"
	phCollectionB = "00000000-0000-0000-0000-000000000011"
	phObjectOne   = "00000000-0000-0000-0000-000000000101"
	phObjectTwo   = "00000000-0000-0000-0000-000000000102"
	phObjectThree = "00000000-0000-0000-0000-000000000103"
	phObjectSSE   = "00000000-0000-0000-0000-000000000104"
	phBlobOne     = "{{blob_one}}"
	phBlobTwo     = "{{blob_two}}"
	phBlobThree   = "{{blob_three}}"
	phBlobDelete  = "{{blob_delete}}"
	phBlobInline  = "{{blob_inline}}"
	phBlobUpdate  = "{{blob_update}}"
	phBlobBatch1  = "{{blob_batch_one}}"
	phBlobBatch2  = "{{blob_batch_two}}"
	phTokenA      = "{{token_a}}"
	phTokenA2     = "{{token_a_2}}"
	phTokenB      = "{{token_b}}"
)

func (r *runner) run(ctx context.Context, cfg runConfig) error {
	if err := r.validateScenarios(); err != nil {
		return err
	}
	if r.opts.scenarios["concurrency"] && (!r.opts.engineParity || cfg.authMode != "dev") {
		return fmt.Errorf("concurrency scenario requires -engine-parity -mode dev")
	}
	if r.enabled("capability") {
		r.scenarioCapability(ctx)
	}
	if cfg.authMode == "dev" {
		if !r.scenarioAuthDev(ctx) {
			return fmt.Errorf("dev login failed; dependent scenarios cannot run")
		}
	} else {
		if !r.scenarioAuthPassword(ctx) {
			return fmt.Errorf("password login failed; dependent scenarios cannot run")
		}
	}
	if !r.needsDataScenarios() {
		return nil
	}
	if !r.setupCollection(ctx) {
		return fmt.Errorf("collection setup failed; dependent scenarios cannot run")
	}
	if r.enabled("collections") {
		r.scenarioCollections(ctx)
	}
	if r.enabled("blobs") || r.enabled("objects") || r.enabled("blob-objects") || r.enabled("ownership") || r.enabled("sse") {
		if !r.setupPrimaryBlob(ctx) {
			return fmt.Errorf("primary blob setup failed; dependent scenarios cannot run")
		}
	}
	if r.enabled("blobs") {
		r.scenarioBlobs(ctx)
	}
	if r.enabled("objects") || r.enabled("blob-objects") || r.enabled("ownership") || r.enabled("sse") {
		if err := r.scenarioObjects(ctx); err != nil {
			return err
		}
	}
	if r.enabled("blob-objects") {
		r.scenarioBlobObjects(ctx)
	}
	if cfg.authMode == "dev" && r.enabled("ownership") {
		r.scenarioOwnership(ctx)
	}
	if r.enabled("sse") {
		r.scenarioSSE(ctx)
	}
	if r.enabled("objects") || r.enabled("blob-objects") {
		r.scenarioAcceptedTail(ctx)
	}
	if r.enabled("collections") {
		r.scenarioCollectionDelete(ctx)
	}
	if r.opts.engineParity && cfg.authMode == "dev" && r.enabled("concurrency") {
		r.scenarioSQLiteConcurrency(ctx)
	}
	return nil
}

func (r *runner) validateScenarios() error {
	valid := map[string]bool{
		"capability": true, "auth": true, "collections": true, "blobs": true,
		"objects": true, "blob-objects": true, "ownership": true, "sse": true,
		"concurrency": true,
	}
	for name := range r.opts.scenarios {
		if !valid[name] {
			return fmt.Errorf("unknown scenario %q", name)
		}
	}
	return nil
}

func (r *runner) needsDataScenarios() bool {
	if len(r.opts.scenarios) == 0 {
		return true
	}
	for _, name := range []string{"collections", "blobs", "objects", "blob-objects", "ownership", "sse", "concurrency"} {
		if r.enabled(name) {
			return true
		}
	}
	return false
}

func (r *runner) scenarioCapability(ctx context.Context) {
	r.step(ctx, "capability", "discovery", requestSpec{
		Method: http.MethodGet, Path: "/", WantStatus: http.StatusOK,
		Allow: allowCapabilityVersion,
	})
	r.step(ctx, "capability", "health", requestSpec{Method: http.MethodGet, Path: "/health", WantStatus: http.StatusOK})
}

func (r *runner) scenarioAuthDev(ctx context.Context) bool {
	first := r.step(ctx, "auth", "dev login new user", jsonRequest(http.MethodPost, "/api/auth/dev/login",
		`{"email":"  COMPARE-A@Example.COM  "}`, "", http.StatusOK,
		binding{Path: "user.id", Placeholder: phUserA}, binding{Path: "token", Placeholder: phTokenA}))
	if !first.OK {
		return false
	}
	second := r.step(ctx, "auth", "dev relogin upserts name", jsonRequest(http.MethodPost, "/api/auth/dev/login",
		`{"email":"compare-a@example.com","name":"  Renamed A  "}`, "", http.StatusOK,
		binding{Path: "user.id", Placeholder: phUserA}, binding{Path: "token", Placeholder: phTokenA2}))
	if !second.OK {
		return false
	}
	r.step(ctx, "auth", "dev missing email", jsonRequest(http.MethodPost, "/api/auth/dev/login", `{}`, "", http.StatusBadRequest))
	r.step(ctx, "auth", "dev invalid json", rawJSONRequest(http.MethodPost, "/api/auth/dev/login", []byte("{"), "", http.StatusBadRequest))
	r.step(ctx, "auth", "whoami bearer", requestSpec{Method: http.MethodGet, Path: "/api/auth", Auth: phTokenA2, WantStatus: http.StatusOK})
	r.step(ctx, "auth", "whoami cookie", requestSpec{Method: http.MethodGet, Path: "/api/auth", Cookie: phTokenA2, WantStatus: http.StatusOK})
	r.step(ctx, "auth", "password route absent", jsonRequest(http.MethodPost, "/api/auth/password/login", `{"password":"irrelevant"}`, phTokenA2, http.StatusNotFound))
	r.step(ctx, "auth", "logout", requestSpec{Method: http.MethodPost, Path: "/api/auth/logout", Auth: phTokenA2, WantStatus: http.StatusNoContent})
	r.step(ctx, "auth", "post logout bearer", requestSpec{Method: http.MethodGet, Path: "/api/auth", Auth: phTokenA2, WantStatus: http.StatusUnauthorized})
	r.step(ctx, "auth", "garbage bearer", requestSpec{
		Method: http.MethodGet, Path: "/api/auth",
		Auth: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", WantStatus: http.StatusUnauthorized,
	})
	r.step(ctx, "auth", "malformed authorization", requestSpec{
		Method: http.MethodGet, Path: "/api/auth", Headers: map[string]string{"Authorization": "Garbage credentials"}, WantStatus: http.StatusUnauthorized,
		Allow: allowMalformedAuthorization,
	})
	// The original token remains valid and is used by the data scenarios.
	original := r.step(ctx, "auth", "original session remains valid", requestSpec{Method: http.MethodGet, Path: "/api/auth", Auth: phTokenA, WantStatus: http.StatusOK})
	return original.OK
}

func (r *runner) scenarioAuthPassword(ctx context.Context) bool {
	login := r.step(ctx, "auth", "password correct", jsonRequest(http.MethodPost, "/api/auth/password/login",
		`{"password":"`+comparisonPassword+`"}`, "", http.StatusOK,
		binding{Path: "user.id", Placeholder: phUserA}, binding{Path: "token", Placeholder: phTokenA}))
	if !login.OK {
		return false
	}
	r.step(ctx, "auth", "dev route absent", jsonRequest(http.MethodPost, "/api/auth/dev/login",
		`{"email":"not-mounted@example.com"}`, phTokenA, http.StatusNotFound))
	whoami := r.step(ctx, "auth", "whoami", requestSpec{Method: http.MethodGet, Path: "/api/auth", Auth: phTokenA, WantStatus: http.StatusOK})
	if !whoami.OK {
		return false
	}
	wrong := jsonRequest(http.MethodPost, "/api/auth/password/login", `{"password":"wrong"}`, "", http.StatusUnauthorized)
	for attempt := 2; attempt <= 10; attempt++ {
		r.step(ctx, "auth", fmt.Sprintf("password wrong attempt %02d", attempt), wrong)
	}
	rateLimited := jsonRequest(http.MethodPost, "/api/auth/password/login", `{"password":"wrong"}`, "", http.StatusTooManyRequests)
	r.step(ctx, "auth", "password attempt 11 rate limited", rateLimited)
	return true
}

func (r *runner) setupCollection(ctx context.Context) bool {
	pair := r.step(ctx, "collections", "claim", requestSpec{
		Method: http.MethodPost, Path: "/api/collections", Auth: phTokenA, WantStatus: http.StatusCreated,
		Binds: []binding{{Path: "collection.id", Placeholder: phCollectionA}},
	})
	return pair.OK
}

func (r *runner) scenarioCollections(ctx context.Context) {
	r.step(ctx, "collections", "claim again", requestSpec{Method: http.MethodPost, Path: "/api/collections", Auth: phTokenA, WantStatus: http.StatusOK})
	r.step(ctx, "collections", "list", requestSpec{Method: http.MethodGet, Path: "/api/collections", Auth: phTokenA, WantStatus: http.StatusOK})
	r.step(ctx, "collections", "get", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA, Auth: phTokenA, WantStatus: http.StatusOK})
	r.step(ctx, "collections", "get unknown", requestSpec{Method: http.MethodGet, Path: "/api/collections/ffffffff-ffff-ffff-ffff-ffffffffffff", Auth: phTokenA, WantStatus: http.StatusNotFound})
	r.step(ctx, "collections", "key initially null", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA + "/key", Auth: phTokenA, WantStatus: http.StatusOK})
	r.step(ctx, "collections", "put key", jsonRequest(http.MethodPut, "/api/collections/"+phCollectionA+"/key",
		`{"key_salt":"c2FsdA","key_kdf":{"name":"scrypt","cost":16384},"encrypted_vault_key":"d3JhcHBlZA"}`, phTokenA, http.StatusOK))
	r.step(ctx, "collections", "get key", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA + "/key", Auth: phTokenA, WantStatus: http.StatusOK})
}

func (r *runner) setupPrimaryBlob(ctx context.Context) bool {
	return r.step(ctx, "blobs", "stage primary", blobRequest([]byte("primary encrypted payload"), phBlobOne)).OK
}

func (r *runner) scenarioBlobs(ctx context.Context) {
	pair := r.step(ctx, "blobs", "download round trip", requestSpec{Method: http.MethodGet, Path: "/api/blobs/" + phBlobOne, Auth: phTokenA, WantStatus: http.StatusOK})
	r.expectBytes("blobs/download round trip payload", pair, []byte("primary encrypted payload"))
	r.step(ctx, "blobs", "bad key owner shape", requestSpec{Method: http.MethodGet, Path: "/api/blobs/not-a-user/not-a-blob", Auth: phTokenA, WantStatus: http.StatusNotFound})
	r.step(ctx, "blobs", "stage delete candidate", blobRequest([]byte("delete me"), phBlobDelete))
	r.step(ctx, "blobs", "delete staged", requestSpec{Method: http.MethodDelete, Path: "/api/blobs/" + phBlobDelete, Auth: phTokenA, WantStatus: http.StatusNoContent})
	r.step(ctx, "blobs", "deleted blob absent", requestSpec{Method: http.MethodGet, Path: "/api/blobs/" + phBlobDelete, Auth: phTokenA, WantStatus: http.StatusNotFound})
	if r.opts.large {
		r.step(ctx, "blobs", "oversize upload", requestSpec{
			Method: http.MethodPost, Path: "/api/blobs", Body: bytes.Repeat([]byte{'x'}, 104857601),
			ContentType: "application/octet-stream", Auth: phTokenA, WantStatus: http.StatusRequestEntityTooLarge,
		})
	}
}

func (r *runner) scenarioObjects(ctx context.Context) error {
	created := r.step(ctx, "objects", "create blob-first", jsonRequest(http.MethodPost, "/api/collections/"+phCollectionA+"/objects",
		`{"blob_key":"`+phBlobOne+`","size_bytes":25}`, phTokenA, http.StatusCreated,
		binding{Path: "object.id", Placeholder: phObjectOne}))
	if !created.OK {
		return fmt.Errorf("object creation diverged; dependent object scenarios cannot run")
	}
	r.step(ctx, "objects", "list", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA + "/objects", Auth: phTokenA, WantStatus: http.StatusOK})
	if err := r.seedPullLimitFixtures(ctx); err != nil {
		return fmt.Errorf("seeding pull-limit fixtures: %w", err)
	}
	r.step(ctx, "objects", "list since and limit clamp", requestSpec{
		Method: http.MethodGet, Path: "/api/collections/" + phCollectionA + "/objects?sinceVersion=0&limit=5000",
		Auth: phTokenA, WantStatus: http.StatusOK, CheckJSON: pullLimitProblems,
	})
	r.step(ctx, "objects", "get", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA + "/objects/" + phObjectOne, Auth: phTokenA, WantStatus: http.StatusOK})
	r.step(ctx, "blobs", "stage update", blobRequest([]byte("replacement payload"), phBlobTwo))
	r.step(ctx, "objects", "update", jsonRequest(http.MethodPut, "/api/collections/"+phCollectionA+"/objects/"+phObjectOne,
		`{"version":2,"blob_key":"`+phBlobTwo+`","size_bytes":19}`, phTokenA, http.StatusOK))
	r.step(ctx, "blobs", "stage stale candidate", blobRequest([]byte("stale replacement"), phBlobThree))
	r.step(ctx, "objects", "stale update conflict", jsonRequest(http.MethodPut, "/api/collections/"+phCollectionA+"/objects/"+phObjectOne,
		`{"version":2,"blob_key":"`+phBlobThree+`","size_bytes":17}`, phTokenA, http.StatusConflict))
	r.step(ctx, "objects", "delete wrong version", requestSpec{Method: http.MethodDelete, Path: "/api/collections/" + phCollectionA + "/objects/" + phObjectOne + "?version=1", Auth: phTokenA, WantStatus: http.StatusConflict})
	r.step(ctx, "objects", "delete", requestSpec{Method: http.MethodDelete, Path: "/api/collections/" + phCollectionA + "/objects/" + phObjectOne + "?version=2", Auth: phTokenA, WantStatus: http.StatusOK})
	return nil
}

// seedPullLimitFixtures inserts identical, deterministic rows into both
// scratch databases. Creating 1,000 extra objects through the public API would
// add thousands of unrelated identity bindings; direct fixtures keep this one
// check focused on the list contract while still exercising each server's
// query and response path.
func (r *runner) seedPullLimitFixtures(ctx context.Context) error {
	createdAt := time.Now().UTC()
	for _, fixture := range []struct {
		target      *target
		databaseURL string
		blobDir     string
	}{{r.ts, r.env.tsDatabaseURL, r.env.tsBlob}, {r.goTarget, r.env.goDatabaseURL, r.env.goBlob}} {
		userID, userBound := fixture.target.identities[phUserA]
		collectionID, collectionBound := fixture.target.identities[phCollectionA]
		if !userBound || !collectionBound {
			return fmt.Errorf("%s identities are not bound", fixture.target.name)
		}

		database, err := appdb.Open(config.Config{
			DatabaseURL: fixture.databaseURL,
			BlobDir:     fixture.blobDir,
		})
		if err != nil {
			return fmt.Errorf("opening %s fixture database: %w", fixture.target.name, err)
		}
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			database.Close()
			return fmt.Errorf("beginning %s fixture transaction: %w", fixture.target.name, err)
		}
		createdValue := any(createdAt)
		if database.Dialect().Engine() == appdb.SQLite {
			createdValue = appdb.Timestamp(createdAt)
		}
		var insertErr error
		for sequence := 2; sequence <= 1001; sequence++ {
			id := fmt.Sprintf("10000000-0000-0000-0000-%012x", sequence)
			_, insertErr = tx.ExecContext(ctx, `INSERT INTO objects
				(id, collection_id, user_id, version, deleted, blob_key, size_bytes, created_at, updated_at, change_seq)
				VALUES ($1, $2, $3, 1, false, NULL, NULL, $4, $4, $5)`,
				id, collectionID, userID, createdValue, sequence)
			if insertErr != nil {
				break
			}
		}
		if insertErr == nil {
			_, insertErr = tx.ExecContext(ctx,
				`UPDATE collections SET current_version = 1001 WHERE id = $1 AND user_id = $2`,
				collectionID, userID)
		}
		if insertErr != nil {
			_ = tx.Rollback()
			database.Close()
			return fmt.Errorf("writing %s fixtures: %w", fixture.target.name, insertErr)
		}
		if err := tx.Commit(); err != nil {
			database.Close()
			return fmt.Errorf("committing %s fixtures: %w", fixture.target.name, err)
		}
		if err := database.Close(); err != nil {
			return fmt.Errorf("closing %s fixture database: %w", fixture.target.name, err)
		}
	}
	return nil
}

func pullLimitProblems(value any) []string {
	var problems []string
	rawObjects, ok := jsonPath(value, "objects")
	objects, objectsOK := rawObjects.([]any)
	if !ok || !objectsOK {
		problems = append(problems, "objects is not an array")
	} else if len(objects) != 1000 {
		problems = append(problems, fmt.Sprintf("limit=5000 returned %d objects, want clamped 1000", len(objects)))
	}
	hasMore, ok := jsonPath(value, "hasMore")
	if !ok || hasMore != true {
		problems = append(problems, fmt.Sprintf("hasMore=%v, want true", hasMore))
	}
	nextCursor, ok := jsonInt(value, "nextCursor")
	if !ok || nextCursor != 1000 {
		problems = append(problems, fmt.Sprintf("nextCursor=%v, want 1000", nextCursor))
	}
	return problems
}

func (r *runner) scenarioBlobObjects(ctx context.Context) {
	create := requestSpec{
		Method: http.MethodPost, Path: "/api/collections/" + phCollectionA + "/blob-objects", Body: []byte("inline object payload"),
		ContentType: "application/octet-stream", Auth: phTokenA, WantStatus: http.StatusCreated,
		Headers: map[string]string{"Mutation-Id": "compare-create-inline"},
		Binds:   []binding{{Path: "object.id", Placeholder: phObjectTwo}, {Path: "object.blob_key", Placeholder: phBlobInline}},
	}
	if !r.step(ctx, "blob-objects", "inline create", create).OK {
		return
	}
	r.step(ctx, "blob-objects", "inline create exact replay", create)
	r.step(ctx, "blob-objects", "recover create", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA + "/create-mutations/compare-create-inline", Auth: phTokenA, WantStatus: http.StatusOK})
	r.step(ctx, "blob-objects", "recover unknown", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA + "/create-mutations/compare-unknown", Auth: phTokenA, WantStatus: http.StatusNotFound})
	r.step(ctx, "blob-objects", "inline update", requestSpec{
		Method: http.MethodPut, Path: "/api/collections/" + phCollectionA + "/blob-objects/" + phObjectTwo + "?version=2",
		Body: []byte("inline updated payload"), ContentType: "application/octet-stream", Auth: phTokenA, WantStatus: http.StatusOK,
		Headers: map[string]string{"Mutation-Id": "compare-update-inline"}, Binds: []binding{{Path: "object.blob_key", Placeholder: phBlobUpdate}},
	})

	batch := append(batchEntryFrame(0, "compare-batch-create", 0, []byte("batch create payload")),
		batchEntryFrame(1, phObjectTwo, 3, []byte("batch update payload"))...)
	r.step(ctx, "blob-objects", "batch mixed create update", requestSpec{
		Method: http.MethodPost, Path: "/api/collections/" + phCollectionA + "/blob-objects/batch", Body: batch,
		ContentType: "application/octet-stream", Auth: phTokenA, WantStatus: http.StatusOK,
		Binds: []binding{
			{Path: "results.0.object.id", Placeholder: phObjectThree}, {Path: "results.0.object.blob_key", Placeholder: phBlobBatch1},
			{Path: "results.1.object.blob_key", Placeholder: phBlobBatch2},
		},
	})
	r.step(ctx, "blob-objects", "batch malformed frame", requestSpec{
		Method: http.MethodPost, Path: "/api/collections/" + phCollectionA + "/blob-objects/batch", Body: []byte{0, 0},
		ContentType: "application/octet-stream", Auth: phTokenA, WantStatus: http.StatusBadRequest,
	})
	many := []byte{}
	for index := range 201 {
		many = append(many, batchEntryFrame(0, fmt.Sprintf("batch-%03d", index), 0, []byte{'x'})...)
	}
	r.step(ctx, "blob-objects", "batch over 200 entries", requestSpec{
		Method: http.MethodPost, Path: "/api/collections/" + phCollectionA + "/blob-objects/batch", Body: many,
		ContentType: "application/octet-stream", Auth: phTokenA, WantStatus: http.StatusBadRequest,
	})
	if r.opts.large {
		r.step(ctx, "blob-objects", "batch request exceeds 32 MiB cap", requestSpec{
			Method: http.MethodPost, Path: "/api/collections/" + phCollectionA + "/blob-objects/batch",
			Body: bytes.Repeat([]byte{'x'}, 33554433), ContentType: "application/octet-stream", Auth: phTokenA,
			WantStatus: http.StatusRequestEntityTooLarge,
		})
	}

	missingKey := phUserA + "/ffffffff-ffff-ffff-ffff-ffffffffffff"
	r.step(ctx, "blob-objects", "batch download present and missing", jsonRequestFrames("/api/blobs/batch",
		[]string{phBlobInline, missingKey}, phTokenA))
	if r.opts.large {
		r.scenarioBatchOmitted(ctx)
	}

}

func (r *runner) scenarioBatchOmitted(ctx context.Context) {
	largeA := bytes.Repeat([]byte{'a'}, 17*1024*1024)
	largeB := bytes.Repeat([]byte{'b'}, 17*1024*1024)
	r.step(ctx, "blobs", "stage batch-cap blob one", blobRequest(largeA, "{{blob_large_one}}"))
	r.step(ctx, "blobs", "stage batch-cap blob two", blobRequest(largeB, "{{blob_large_two}}"))
	r.step(ctx, "blob-objects", "batch download omitted at cap", jsonRequestFrames("/api/blobs/batch",
		[]string{"{{blob_large_one}}", "{{blob_large_two}}"}, phTokenA))
}

func (r *runner) scenarioOwnership(ctx context.Context) {
	if !r.step(ctx, "ownership", "login user B", jsonRequest(http.MethodPost, "/api/auth/dev/login",
		`{"email":"compare-b@example.com","name":"Compare B"}`, "", http.StatusOK,
		binding{Path: "user.id", Placeholder: phUserB}, binding{Path: "token", Placeholder: phTokenB})).OK {
		return
	}
	r.step(ctx, "ownership", "collection hidden", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA, Auth: phTokenB, WantStatus: http.StatusNotFound})
	r.step(ctx, "ownership", "collection objects hidden", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA + "/objects", Auth: phTokenB, WantStatus: http.StatusNotFound})
	r.step(ctx, "ownership", "object hidden", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA + "/objects/" + phObjectOne, Auth: phTokenB, WantStatus: http.StatusNotFound})
	r.step(ctx, "ownership", "blob hidden", requestSpec{Method: http.MethodGet, Path: "/api/blobs/" + phBlobOne, Auth: phTokenB, WantStatus: http.StatusNotFound})
	r.step(ctx, "ownership", "claim user B collection", requestSpec{
		Method: http.MethodPost, Path: "/api/collections", Auth: phTokenB, WantStatus: http.StatusCreated,
		Binds: []binding{{Path: "collection.id", Placeholder: phCollectionB}},
	})
}

func (r *runner) scenarioSSE(ctx context.Context) {
	tsCollector, tsErr := openSSE(ctx, r.ts, phTokenA)
	goCollector, goErr := openSSE(ctx, r.goTarget, phTokenA)
	if tsErr != nil || goErr != nil {
		r.diverge(r.name+"/sse/connect", fmt.Sprintf("%s=%v %s=%v", r.ts.name, tsErr, r.goTarget.name, goErr))
		return
	}
	defer tsCollector.close()
	defer goCollector.close()
	r.step(ctx, "sse", "mutation burst", requestSpec{
		Method: http.MethodPost, Path: "/api/collections/" + phCollectionA + "/blob-objects", Body: []byte("sse payload"),
		ContentType: "application/octet-stream", Auth: phTokenA, WantStatus: http.StatusCreated,
		Headers: map[string]string{"Mutation-Id": "compare-sse-create"}, Binds: []binding{
			{Path: "object.id", Placeholder: phObjectSSE}, {Path: "object.blob_key", Placeholder: "{{blob_sse}}"},
		},
	})
	tsState, tsErr := tsCollector.waitState(5 * time.Second)
	goState, goErr := goCollector.waitState(5 * time.Second)
	label := r.name + "/sse/coalesced checkpoint"
	r.result.Steps++
	if tsErr != nil || goErr != nil || !mapsEqual(tsState, goState) {
		r.diverge(label, fmt.Sprintf("%s=%v err=%v %s=%v err=%v", r.ts.name, tsState, tsErr, r.goTarget.name, goState, goErr))
	} else {
		r.result.Matched++
		fmt.Printf("PASS  %s\n", label)
	}
}

// Accepted deviations that mutate only one side run after every parity
// checkpoint so their intentionally different state cannot create cascades.
func (r *runner) scenarioAcceptedTail(ctx context.Context) {
	if r.enabled("objects") {
		r.step(ctx, "objects", "re-delete tombstone", requestSpec{
			Method: http.MethodDelete, Path: "/api/collections/" + phCollectionA + "/objects/" + phObjectOne + "?version=1",
			Auth: phTokenA, WantStatus: http.StatusOK,
			Allow: allowTombstoneRedelete,
		})
	}
	if r.enabled("blob-objects") {
		r.step(ctx, "blob-objects", "legacy mutation id", requestSpec{
			Method: http.MethodPost, Path: "/api/collections/" + phCollectionA + "/blob-objects", Body: []byte("legacy mutation payload"),
			ContentType: "application/octet-stream", Auth: phTokenA, Headers: map[string]string{"Mutation-Id": "legacy mutation id"},
			Allow: allowLegacyMutationID,
		})
	}
}

func (r *runner) scenarioCollectionDelete(ctx context.Context) {
	r.step(ctx, "collections", "delete", requestSpec{Method: http.MethodDelete, Path: "/api/collections/" + phCollectionA, Auth: phTokenA, WantStatus: http.StatusNoContent})
	r.step(ctx, "collections", "post-delete collection", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA, Auth: phTokenA, WantStatus: http.StatusNotFound})
	r.step(ctx, "collections", "post-delete objects", requestSpec{Method: http.MethodGet, Path: "/api/collections/" + phCollectionA + "/objects", Auth: phTokenA, WantStatus: http.StatusNotFound})
}

func blobRequest(body []byte, placeholder string) requestSpec {
	return requestSpec{
		Method: http.MethodPost, Path: "/api/blobs", Body: body, ContentType: "application/octet-stream", Auth: phTokenA,
		WantStatus: http.StatusCreated, Binds: []binding{{Path: "key", Placeholder: placeholder}},
	}
}

func jsonRequest(method, path, body, authToken string, wantStatus int, binds ...binding) requestSpec {
	return rawJSONRequest(method, path, []byte(body), authToken, wantStatus, binds...)
}

func rawJSONRequest(method, path string, body []byte, authToken string, wantStatus int, binds ...binding) requestSpec {
	return requestSpec{Method: method, Path: path, Body: body, ContentType: "application/json", Auth: authToken, WantStatus: wantStatus, Binds: binds}
}

func jsonRequestFrames(path string, keys []string, authToken string) requestSpec {
	body, _ := json.Marshal(map[string]any{"keys": keys})
	return requestSpec{Method: http.MethodPost, Path: path, Body: body, ContentType: "application/json", Auth: authToken, WantStatus: http.StatusOK, Frames: true}
}

func (r *runner) expectBytes(name string, pair responsePair, expected []byte) {
	if !bytes.Equal(pair.TS.Body, expected) || !bytes.Equal(pair.Go.Body, expected) {
		r.diverge(r.name+"/"+name, fmt.Sprintf("expected %d bytes; %s=%s %s=%s", len(expected), r.ts.name, preview(pair.TS.Body), r.goTarget.name, preview(pair.Go.Body)))
	}
}

func mapsEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
