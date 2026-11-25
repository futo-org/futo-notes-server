package api_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/hostedauth"
)

// batchUser is a signed-in app with a region and a collection, writing
// objects with receipts.
type batchUser struct {
	h          *harness
	app        *client
	userID     string
	collection string
}

func newBatchUser(t *testing.T, h *harness) *batchUser {
	t.Helper()
	app, tokens := h.appLogin(h.newSub())
	app.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	app.expect(http.MethodPost, "/api/collections", nil, http.StatusCreated).json(t, &claimed)
	return &batchUser{h: h, app: app, userID: tokens.User.ID, collection: claimed.Collection.ID}
}

func (u *batchUser) objectsPath() string { return "/api/collections/" + u.collection + "/objects" }

// receipt signs a fresh receipt for one of u's uploads.
func (u *batchUser) receipt() string {
	token, _ := u.h.receipt("us-east", u.userID, 7)
	return token
}

// entryResult is one batch result with its body left encoded.
type entryResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// batch posts entries and returns their results.
func (u *batchUser) batch(entries ...map[string]any) []entryResult {
	u.h.t.Helper()
	res := u.app.do(http.MethodPost, u.objectsPath()+"/batch", map[string]any{"entries": entries})
	if res.status != http.StatusOK {
		u.h.t.Fatalf("batch = %d %s", res.status, res.body)
	}
	var body struct {
		Results []entryResult `json:"results"`
	}
	res.json(u.h.t, &body)
	if len(body.Results) != len(entries) {
		u.h.t.Fatalf("batch of %d entries answered %d results: %s", len(entries), len(body.Results), res.body)
	}
	return body.Results
}

// single sends the single create (objectID "") or update an entry stands
// for.
func (u *batchUser) single(mutationID, objectID string, body map[string]any) response {
	u.h.t.Helper()
	if objectID == "" {
		return u.app.do(http.MethodPost, u.objectsPath(), body, "Mutation-Id", mutationID)
	}
	return u.app.do(http.MethodPut, u.objectsPath()+"/"+objectID, body, "Mutation-Id", mutationID)
}

// expectSameOutcome fails unless the batch result is exactly the single
// call's answer: its status, and its body byte for byte.
func expectSameOutcome(t *testing.T, name string, single response, batched entryResult) {
	t.Helper()
	if batched.Status != single.status || string(batched.Body) != strings.TrimSuffix(single.body, "\n") {
		t.Fatalf("%s: batch entry = %d %s, single call = %d %s", name, batched.Status, batched.Body, single.status, single.body)
	}
}

// entry is the batch entry for the single call with mutationID and body.
func entry(mutationID, objectID string, body map[string]any) map[string]any {
	e := map[string]any{"mutation_id": mutationID}
	if objectID != "" {
		e["object_id"] = objectID
	}
	maps.Copy(e, body)
	return e
}

// A batch entry answers exactly what the single create or update with the
// same Mutation-Id and body answers, whichever of the two ran first.
func TestBatchEntriesMatchSingleCalls(t *testing.T) {
	h := newHarness(t)
	u := newBatchUser(t, h)
	other := newBatchUser(t, h)

	// A create made by a batch replays through the single call, and the
	// other way round; both replays answer the same bytes.
	first := u.batch(entry("batch-create", "", map[string]any{"receipt": u.receipt()}))[0]
	var created objectBody
	if err := json.Unmarshal(first.Body, &created); err != nil || first.Status != http.StatusCreated || created.Replayed == nil || *created.Replayed {
		t.Fatalf("batch create = %d %s", first.Status, first.Body)
	}
	replay := u.single("batch-create", "", map[string]any{"receipt": u.receipt()})
	if want := strings.Replace(string(first.Body), `"replayed":false`, `"replayed":true`, 1); strings.TrimSuffix(replay.body, "\n") != want {
		t.Fatalf("single replay of a batch create = %d %s, want 201 %s", replay.status, replay.body, want)
	}
	expectSameOutcome(t, "batch replay of a batch create", replay, u.batch(entry("batch-create", "", nil))[0])

	single := u.single("single-create", "", map[string]any{"receipt": u.receipt()})
	if single.status != http.StatusCreated {
		t.Fatalf("single create = %d %s", single.status, single.body)
	}
	replay = u.single("single-create", "", map[string]any{"receipt": u.receipt()})
	expectSameOutcome(t, "batch replay of a single create", replay, u.batch(entry("single-create", "", map[string]any{"receipt": u.receipt()}))[0])

	// Updates replay across the two the same way.
	objectID := created.Object.ID
	updated := u.batch(entry("batch-update", objectID, map[string]any{"version": 2, "receipt": u.receipt()}))[0]
	if updated.Status != http.StatusOK {
		t.Fatalf("batch update = %d %s", updated.Status, updated.Body)
	}
	expectSameOutcome(t, "single replay of a batch update", u.single("batch-update", objectID, map[string]any{"version": 2}), updated)

	// Failures: each pair runs from the same state under fresh Mutation-Ids.
	spent := u.receipt()
	if res := u.single("spend", "", map[string]any{"receipt": spent}); res.status != http.StatusCreated {
		t.Fatalf("create = %d %s", res.status, res.body)
	}
	expiredSigner, err := hostedauth.NewReceiptSigner(h.keys["us-east"], "us-east",
		hostedauth.WithClock(func() time.Time { return time.Now().Add(-25 * time.Hour) }))
	if err != nil {
		t.Fatal(err)
	}
	expired, err := expiredSigner.Sign(u.userID, u.userID+"/0195e2a1-0000-7000-8000-00000000e001", 1)
	if err != nil {
		t.Fatal(err)
	}
	otherRegion, _ := h.receipt("eu-central", u.userID, 1)
	otherUser, _ := h.receipt("us-east", other.userID, 1)
	cases := []struct {
		name     string
		objectID string
		body     map[string]any
		status   int
	}{
		{"expired receipt", "", map[string]any{"receipt": expired}, http.StatusBadRequest},
		{"garbage receipt", "", map[string]any{"receipt": "not.a.jwt"}, http.StatusBadRequest},
		{"no receipt", "", map[string]any{}, http.StatusBadRequest},
		{"receipt from another region", "", map[string]any{"receipt": otherRegion}, http.StatusBadRequest},
		{"receipt of another user", "", map[string]any{"receipt": otherUser}, http.StatusBadRequest},
		{"spent receipt", "", map[string]any{"receipt": spent}, http.StatusConflict},
		{"spent receipt on an update", objectID, map[string]any{"version": 3, "receipt": spent}, http.StatusConflict},
		{"stale version", objectID, map[string]any{"version": 2, "receipt": u.receipt()}, http.StatusConflict},
		{"absent object", absentID, map[string]any{"version": 2, "receipt": u.receipt()}, http.StatusNotFound},
		{"version zero", objectID, map[string]any{"version": 0, "receipt": u.receipt()}, http.StatusBadRequest},
		{"no version", objectID, map[string]any{"receipt": u.receipt()}, http.StatusBadRequest},
		{"another user's object", objectID, map[string]any{"version": 3, "receipt": u.receipt()}, http.StatusNotFound},
	}
	for i, tc := range cases {
		caller := u
		if tc.name == "another user's object" {
			caller = other
			tc.body["receipt"] = other.receipt()
		}
		id := "failure-" + string(rune('a'+i))
		res := caller.single(id+"-single", tc.objectID, tc.body)
		if res.status != tc.status {
			t.Fatalf("%s: single call = %d %s, want %d", tc.name, res.status, res.body, tc.status)
		}
		expectSameOutcome(t, tc.name, res, caller.batch(entry(id+"-batch", tc.objectID, tc.body))[0])
	}

	// A Mutation-Id reused for another intent is refused the same way.
	res := u.single("batch-create", objectID, map[string]any{"version": 3, "receipt": u.receipt()})
	expectSameOutcome(t, "reused Mutation-Id", res,
		u.batch(entry("batch-update", objectID, map[string]any{"version": 3, "receipt": u.receipt()}))[0])
	// An invalid Mutation-Id answers the single call's header error.
	res = u.single("not valid", "", map[string]any{"receipt": u.receipt()})
	expectSameOutcome(t, "invalid Mutation-Id", res, u.batch(entry("not valid", "", map[string]any{"receipt": u.receipt()}))[0])
}

// Entries run independently, in order, each its own mutation: one failing
// does not stop the others, and retrying the whole batch replays the entries
// that have a result and runs the rest.
func TestBatchEntriesAreIndependentAndReplay(t *testing.T) {
	h := newHarness(t)
	u := newBatchUser(t, h)
	base := u.batch(entry("seed", "", map[string]any{"receipt": u.receipt()}))[0]
	var seed objectBody
	if err := json.Unmarshal(base.Body, &seed); err != nil {
		t.Fatal(err)
	}

	entries := []map[string]any{
		entry("one", "", map[string]any{"receipt": u.receipt()}),
		entry("bad-receipt", "", map[string]any{"receipt": "not.a.jwt"}),
		entry("update", seed.Object.ID, map[string]any{"version": 2, "receipt": u.receipt()}),
		entry("stale", seed.Object.ID, map[string]any{"version": 2, "receipt": u.receipt()}),
		entry("two", "", map[string]any{"receipt": u.receipt()}),
		{"receipt": u.receipt()},
		entry("create-with-version", "", map[string]any{"version": 1, "receipt": u.receipt()}),
	}
	results := u.batch(entries...)
	wantStatus := []int{http.StatusCreated, http.StatusBadRequest, http.StatusOK, http.StatusConflict, http.StatusCreated,
		http.StatusBadRequest, http.StatusBadRequest}
	wantVersion := map[int]int64{0: 2, 2: 3, 4: 4}
	for i, r := range results {
		if r.Status != wantStatus[i] {
			t.Fatalf("entry %d = %d %s, want %d", i, r.Status, r.Body, wantStatus[i])
		}
		if version, ok := wantVersion[i]; ok {
			var o objectBody
			if err := json.Unmarshal(r.Body, &o); err != nil || o.CollectionVersion != version {
				t.Fatalf("entry %d = %s, want collectionVersion %d: each entry is its own mutation", i, r.Body, version)
			}
		}
	}
	for i, want := range map[int]string{
		1: `{"code":"receipt_invalid","error":"invalid receipt"}`,
		3: `{"error":"version conflict",`,
		5: `{"error":"invalid Mutation-Id"}`,
		6: `{"error":"invalid version"}`,
	} {
		if !strings.HasPrefix(string(results[i].Body), want) {
			t.Fatalf("entry %d = %s, want %s", i, results[i].Body, want)
		}
	}

	// The retry answers every entry that has a result with it, receipts
	// unchecked; the rejected receipt recorded nothing, so that entry runs
	// again, now with a good receipt.
	entries[1]["receipt"] = u.receipt()
	for _, i := range []int{0, 2, 3, 4} {
		entries[i]["receipt"] = "expired-or-anything"
	}
	retried := u.batch(entries...)
	for _, i := range []int{2, 3, 5, 6} {
		if retried[i].Status != results[i].Status || string(retried[i].Body) != string(results[i].Body) {
			t.Fatalf("retried entry %d = %d %s, want the original %d %s", i, retried[i].Status, retried[i].Body, results[i].Status, results[i].Body)
		}
	}
	for _, i := range []int{0, 4} {
		want := strings.Replace(string(results[i].Body), `"replayed":false`, `"replayed":true`, 1)
		if retried[i].Status != http.StatusCreated || string(retried[i].Body) != want {
			t.Fatalf("retried create %d = %d %s, want 201 %s", i, retried[i].Status, retried[i].Body, want)
		}
	}
	var late objectBody
	if err := json.Unmarshal(retried[1].Body, &late); err != nil || retried[1].Status != http.StatusCreated || late.CollectionVersion != 5 {
		t.Fatalf("retried rejected entry = %d %s, want a fresh create at collectionVersion 5", retried[1].Status, retried[1].Body)
	}
	var list struct {
		Objects []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	u.app.expect(http.MethodGet, u.objectsPath(), nil, http.StatusOK).json(t, &list)
	if len(list.Objects) != 4 {
		t.Fatalf("collection holds %d objects, want the seed and three creates", len(list.Objects))
	}
}

func TestBatchRequestValidation(t *testing.T) {
	h := newHarness(t)
	u := newBatchUser(t, h)
	other := newBatchUser(t, h)
	path := u.objectsPath() + "/batch"
	many := make([]map[string]any, 201)
	for i := range many {
		many[i] = entry("m", "", map[string]any{"receipt": "r"})
	}
	cases := []struct {
		name   string
		body   any
		status int
		want   string
	}{
		{"not json", "entries", http.StatusBadRequest, `{"error":"invalid json"}`},
		{"unknown field", map[string]any{"entries": []map[string]any{{"mutation_id": "m", "objectId": absentID, "receipt": "r"}}},
			http.StatusBadRequest, `{"error":"invalid json"}`},
		{"unknown top-level field", map[string]any{"entries": []map[string]any{entry("m", "", nil)}, "atomic": true},
			http.StatusBadRequest, `{"error":"invalid json"}`},
		{"no entries", map[string]any{}, http.StatusBadRequest, `{"error":"batch must contain at least one entry"}`},
		{"empty entries", map[string]any{"entries": []any{}}, http.StatusBadRequest, `{"error":"batch must contain at least one entry"}`},
		{"201 entries", map[string]any{"entries": many}, http.StatusBadRequest, `{"error":"too many entries (max 200)"}`},
		{"over 256 KiB", map[string]any{"entries": []map[string]any{entry("m", "", map[string]any{"receipt": strings.Repeat("r", 256<<10)})}},
			http.StatusRequestEntityTooLarge, `{"error":"request too large"}`},
	}
	for _, tc := range cases {
		var res response
		if s, ok := tc.body.(string); ok {
			res = u.app.do(http.MethodPost, path, json.RawMessage(`"`+s+`"`))
		} else {
			res = u.app.do(http.MethodPost, path, tc.body)
		}
		if res.status != tc.status || res.body != tc.want+"\n" {
			t.Fatalf("%s = %d %s, want %d %s", tc.name, res.status, res.body, tc.status, tc.want)
		}
	}

	// Another user's collection, and one that does not exist, are not found.
	for _, collection := range []string{other.collection, absentID} {
		res := u.app.do(http.MethodPost, "/api/collections/"+collection+"/objects/batch",
			map[string]any{"entries": []map[string]any{entry("m", "", map[string]any{"receipt": u.receipt()})}})
		if res.status != http.StatusNotFound || res.body != `{"error":"not found"}`+"\n" {
			t.Fatalf("batch into %s = %d %s, want 404", collection, res.status, res.body)
		}
	}

	// The route does not shadow the object routes: "batch" is just an id
	// naming nothing there.
	if res := u.app.do(http.MethodGet, path, nil); res.status != http.StatusNotFound || res.body != `{"error":"not found"}`+"\n" {
		t.Fatalf("GET %s = %d %s, want the object 404", path, res.status, res.body)
	}

	// 200 entries is the limit, not past it.
	full := make([]map[string]any, 200)
	for i := range full {
		full[i] = entry("full-"+string(rune('A'+i%26))+strings.Repeat("x", i/26), "", map[string]any{"receipt": u.receipt()})
	}
	for i, r := range u.batch(full...) {
		if r.Status != http.StatusCreated {
			t.Fatalf("entry %d of 200 = %d %s", i, r.Status, r.Body)
		}
	}
}
