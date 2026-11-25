package api_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"futo-notes-server/internal/hosted/config"
)

// These tests reach the documented responses the feature tests do not, so
// that specCheck sees every one of them (TestMain fails a run that misses
// one).

// absentID is a well-formed id that names nothing.
const absentID = "0195e2a1-0000-7000-8000-0000000000ff"

type specOperation struct {
	method, path string
	op           *openapi3.Operation
}

// secured reports whether the operation names a security scheme.
func (o specOperation) secured() bool {
	return o.op.Security != nil && len(*o.op.Security) > 0
}

// specOperations lists the spec's operations, with path parameters filled
// with ids that name nothing.
func specOperations(t *testing.T) []specOperation {
	t.Helper()
	fill := strings.NewReplacer("{id}", absentID, "{objectId}", absentID, "{mutationId}", "absent-mutation")
	var ops []specOperation
	for path, item := range loadSpec(t).doc.Paths.Map() {
		for method, op := range item.Operations() {
			ops = append(ops, specOperation{method: method, path: fill.Replace(path), op: op})
		}
	}
	slices.SortFunc(ops, func(a, b specOperation) int { return strings.Compare(a.method+a.path, b.method+b.path) })
	return ops
}

// TestSpecAuthenticationOnEveryOperation sends every operation that names a
// security scheme without a session and with a dead one: each must answer
// 401, in its surface's shape, which specCheck checks.
func TestSpecAuthenticationOnEveryOperation(t *testing.T) {
	h := newHarness(t)
	anonymous := h.client()
	dead := h.client()
	dead.bearer = "not-a-session"
	for _, o := range specOperations(t) {
		if !o.secured() {
			continue
		}
		for name, c := range map[string]*client{"no session": anonymous, "a dead session": dead} {
			if res := c.do(o.method, o.path, nil); res.status != http.StatusUnauthorized {
				t.Errorf("%s %s with %s = %d %s, want 401", o.method, o.path, name, res.status, res.body)
			}
		}
	}
}

// TestSpecPendingDeletionOnEveryGatedOperation sends every session-secured
// operation as a user whose deletion is pending: exactly the ones that
// document the pending-deletion 403 (the PendingDeletion response; other
// 403s mean something else) must answer it.
func TestSpecPendingDeletionOnEveryGatedOperation(t *testing.T) {
	h := newHarness(t)
	app, _ := h.appLogin(h.newSub())
	app.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
	gated := 0
	for _, o := range specOperations(t) {
		// Restoring and signing out end the state under test; other tests
		// show both work while a deletion is pending.
		if !o.secured() || o.op.OperationID == "restoreAccount" || o.op.OperationID == "logout" {
			continue
		}
		res := app.do(o.method, o.path, nil)
		pending := res.status == http.StatusForbidden && strings.Contains(res.body, `"code":"account_pending_deletion"`)
		if forbidden := o.op.Responses.Status(http.StatusForbidden); forbidden == nil || forbidden.Ref != "#/components/responses/PendingDeletion" {
			if pending {
				t.Errorf("%s %s while pending deletion = 403, which it does not document", o.method, o.path)
			}
			continue
		}
		gated++
		if !pending {
			t.Errorf("%s %s while pending deletion = %d %s, want the pending-deletion 403", o.method, o.path, res.status, res.body)
		}
	}
	if gated == 0 {
		t.Fatal("no operation documents the pending-deletion 403")
	}
}

// TestSpecSyncSurface walks the sync operations through their documented
// successes and failures.
func TestSpecSyncSurface(t *testing.T) {
	h := newHarness(t)
	app, tokens := h.appLogin(h.newSub())
	userID := tokens.User.ID
	app.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	app.expect(http.MethodPost, "/api/collections", nil, http.StatusCreated).json(t, &claimed)
	app.expect(http.MethodPost, "/api/collections", nil, http.StatusOK)
	collection := "/api/collections/" + claimed.Collection.ID
	absent := "/api/collections/" + absentID
	app.expect(http.MethodGet, collection, nil, http.StatusOK)

	expect := func(res response, status int, body string) {
		t.Helper()
		if res.status != status || res.body != body+"\n" {
			t.Fatalf("response = %d %s, want %d %s", res.status, res.body, status, body)
		}
	}

	// Keys.
	expect(app.do(http.MethodGet, collection+"/key", nil), http.StatusOK, `{"key":null}`)
	key := map[string]any{"key_salt": "salt", "key_kdf": map[string]any{"name": "argon2id"}, "encrypted_vault_key": "vault"}
	app.expect(http.MethodPut, collection+"/key", key, http.StatusOK)
	app.expect(http.MethodGet, collection+"/key", nil, http.StatusOK)
	expect(app.do(http.MethodPut, collection+"/key", map[string]any{}), http.StatusBadRequest, `{"error":"key_salt is required"}`)
	rotation := map[string]any{"key_salt": "salt2", "key_kdf": map[string]any{"name": "argon2id"}, "encrypted_vault_key": "vault2",
		"previous_key_updated_at": "2000-01-01T00:00:00.000Z"}
	if res := app.do(http.MethodPut, collection+"/key", rotation); res.status != http.StatusConflict || !strings.Contains(res.body, `"currentKey"`) {
		t.Fatalf("stale rotation = %d %s", res.status, res.body)
	}

	// Objects.
	objects := collection + "/objects"
	receipt, _ := h.receipt("us-east", userID, 3)
	var created objectBody
	res := app.do(http.MethodPost, objects, map[string]string{"receipt": receipt}, "Mutation-Id", "spec-create")
	if res.status != http.StatusCreated {
		t.Fatalf("create = %d %s", res.status, res.body)
	}
	res.json(t, &created)
	object := objects + "/" + created.Object.ID
	expect(app.do(http.MethodPost, objects, map[string]string{"receipt": receipt}, "Mutation-Id", "not a mutation id"),
		http.StatusBadRequest, `{"error":"invalid Mutation-Id"}`)
	app.expect(http.MethodGet, objects, nil, http.StatusOK)
	if res := app.expect(http.MethodGet, objects+"?sinceVersion=0&limit=1", nil, http.StatusOK); !strings.Contains(res.body, `"hasMore":false`) {
		t.Fatalf("paged pull = %s", res.body)
	}
	expect(app.do(http.MethodGet, objects+"?limit=0", nil), http.StatusBadRequest, `{"error":"invalid limit"}`)
	app.expect(http.MethodGet, object, nil, http.StatusOK)

	// Create-mutation recovery.
	if res := app.expect(http.MethodGet, collection+"/create-mutations/spec-create", nil, http.StatusOK); !strings.Contains(res.body, `"replayed":true`) {
		t.Fatalf("recovered = %s", res.body)
	}
	expect(app.do(http.MethodGet, collection+"/create-mutations/not!valid", nil), http.StatusBadRequest, `{"error":"invalid Mutation-Id"}`)
	expect(app.do(http.MethodGet, collection+"/create-mutations/never-used", nil), http.StatusNotFound, `{"error":"not found"}`)

	// A Mutation-Id names one intent.
	mismatch := `{"error":"Mutation-Id reused for different intent"}`
	update, _ := h.receipt("us-east", userID, 4)
	expect(app.do(http.MethodPut, object, map[string]any{"version": 2, "receipt": update}, "Mutation-Id", "spec-create"),
		http.StatusConflict, mismatch)
	expect(app.do(http.MethodDelete, object, nil, "Mutation-Id", "spec-create"), http.StatusConflict, mismatch)

	// The first request under a Mutation-Id is still running: its claim is
	// recorded and its result is not.
	pending := `{"error":"mutation still in progress"}`
	claim := func(mutationID, kind string, objectID, version any) {
		t.Helper()
		h.exec(`INSERT INTO mutation_results (user_id, mutation_id, kind, collection_id, object_id, requested_version, result)
			VALUES ($1, $2, $3, $4, $5, $6, '{"status":102,"body":{"error":"mutation still in progress"}}')`,
			userID, mutationID, kind, claimed.Collection.ID, objectID, version)
	}
	claim("spec-pending-create", "create", nil, nil)
	claim("spec-pending-update", "update", created.Object.ID, 2)
	claim("spec-pending-delete", "delete", created.Object.ID, nil)
	expect(app.do(http.MethodPost, objects, map[string]string{"receipt": update}, "Mutation-Id", "spec-pending-create"), http.StatusConflict, pending)
	expect(app.do(http.MethodGet, collection+"/create-mutations/spec-pending-create", nil), http.StatusConflict, pending)
	expect(app.do(http.MethodPut, object, map[string]any{"version": 2, "receipt": update}, "Mutation-Id", "spec-pending-update"),
		http.StatusConflict, pending)
	expect(app.do(http.MethodDelete, object, nil, "Mutation-Id", "spec-pending-delete"), http.StatusConflict, pending)

	// Deletes.
	expect(app.do(http.MethodDelete, object+"?version=0", nil), http.StatusBadRequest, `{"error":"invalid version"}`)
	if res := app.do(http.MethodDelete, object+"?version=5", nil); res.status != http.StatusConflict || !strings.Contains(res.body, `"version conflict"`) {
		t.Fatalf("stale delete = %d %s", res.status, res.body)
	}
	app.expect(http.MethodDelete, object, nil, http.StatusOK)

	// Whatever is not the caller's is not found.
	fresh, _ := h.receipt("us-east", userID, 5)
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, absent, nil},
		{http.MethodDelete, absent, nil},
		{http.MethodGet, absent + "/key", nil},
		{http.MethodPut, absent + "/key", key},
		{http.MethodGet, absent + "/objects", nil},
		{http.MethodPost, absent + "/objects", map[string]string{"receipt": fresh}},
		{http.MethodGet, objects + "/" + absentID, nil},
		{http.MethodPut, objects + "/" + absentID, map[string]any{"version": 2, "receipt": fresh}},
		{http.MethodDelete, objects + "/" + absentID, nil},
		{http.MethodGet, absent + "/create-mutations/spec-create", nil},
	} {
		if res := app.do(req.method, req.path, req.body); res.status != http.StatusNotFound || res.body != `{"error":"not found"}`+"\n" {
			t.Errorf("%s %s = %d %s, want 404", req.method, req.path, res.status, res.body)
		}
	}

	app.expect(http.MethodDelete, collection, nil, http.StatusNoContent)
}

// TestSpecSignInFailures covers the sign-in responses the happy paths do
// not reach.
func TestSpecSignInFailures(t *testing.T) {
	h := newHarness(t)
	state := func(start response) string {
		t.Helper()
		if start.status != http.StatusFound {
			t.Fatalf("start = %d %s", start.status, start.body)
		}
		location, err := url.Parse(start.header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		return location.Query().Get("state")
	}
	callback := func(params url.Values) string { return "/api/auth/oidc/callback?" + params.Encode() }

	// A callback with neither a code nor an error is malformed: a 400 for a
	// dashboard sign-in, invalid_request back to an app.
	browser := h.client()
	s := state(browser.do(http.MethodGet, "/api/auth/oidc/start", nil))
	expectHostedError(t, browser.do(http.MethodGet, callback(url.Values{"state": {s}}), nil), http.StatusBadRequest, "sign_in_not_completed")
	browser = h.client()
	s = state(browser.do(http.MethodGet, "/api/auth/oidc/start?"+url.Values{
		"redirect_uri": {"http://127.0.0.1:43123/callback"}, "code_challenge": {challenge}, "state": {"app-state"},
	}.Encode(), nil))
	res := browser.do(http.MethodGet, callback(url.Values{"state": {s}}), nil)
	if res.status != http.StatusFound || res.header.Get("Location") != "http://127.0.0.1:43123/callback?error=invalid_request&state=app-state" {
		t.Fatalf("app callback with no code = %d %v", res.status, res.header)
	}

	// A code the provider rejects cannot complete the sign-in.
	browser = h.client()
	s = state(browser.do(http.MethodGet, "/api/auth/oidc/start", nil))
	expectHostedError(t, browser.do(http.MethodGet, callback(url.Values{"state": {s}, "code": {"unknown-code"}}), nil),
		http.StatusBadGateway, "provider_error")

	// The provider's error ends a dashboard sign-in here, and goes back to
	// an app.
	browser = h.client()
	s = state(browser.do(http.MethodGet, "/api/auth/oidc/start", nil))
	expectHostedError(t, browser.do(http.MethodGet, callback(url.Values{"state": {s}, "error": {"access_denied"}}), nil),
		http.StatusBadRequest, "sign_in_not_completed")
	browser = h.client()
	s = state(browser.do(http.MethodGet, "/api/auth/oidc/start?"+url.Values{
		"redirect_uri": {"http://127.0.0.1:43123/callback"}, "code_challenge": {challenge}, "state": {"app-state"},
	}.Encode(), nil))
	res = browser.do(http.MethodGet, callback(url.Values{"state": {s}, "error": {"access_denied"}}), nil)
	if res.status != http.StatusFound || res.header.Get("Location") != "http://127.0.0.1:43123/callback?error=access_denied&state=app-state" {
		t.Fatalf("app callback with a provider error = %d %v", res.status, res.header)
	}

	// No sign-in starts while the provider cannot be discovered.
	provider := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(provider.Close)
	down := newHarnessWith(t, func(c *config.Config) { c.OIDC.Issuer = provider.URL })
	expectHostedError(t, down.client().do(http.MethodGet, "/api/auth/oidc/start", nil), http.StatusServiceUnavailable, "provider_unavailable")
}

func TestSpecReadiness(t *testing.T) {
	h := newHarness(t)
	h.client().expect(http.MethodGet, "/readyz", nil, http.StatusOK)
	h.server.BeginDrain()
	h.client().expect(http.MethodGet, "/readyz", nil, http.StatusServiceUnavailable)
}
