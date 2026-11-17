package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/hostedauth"
)

// A retry under a Mutation-Id answers with the stored outcome before looking
// at the receipt, which may have expired (24h) or be anything at all since
// the first attempt. Only a mutation that runs has its receipt verified.
func TestMutationReplayComesBeforeReceiptVerification(t *testing.T) {
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
	objectsPath := "/api/collections/" + claimed.Collection.ID + "/objects"

	expiredSigner, err := hostedauth.NewReceiptSigner(h.keys["us-east"], "us-east",
		hostedauth.WithClock(func() time.Time { return time.Now().Add(-25 * time.Hour) }))
	if err != nil {
		t.Fatal(err)
	}
	expired := func(key string) string {
		t.Helper()
		token, err := expiredSigner.Sign(userID, key, 1)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	retries := func(key string) map[string]map[string]any {
		return map[string]map[string]any{
			"expired receipt": {"receipt": expired(key)},
			"garbage receipt": {"receipt": "not.a.jwt"},
			"no receipt":      {},
		}
	}
	expectSame := func(name string, first, retry response) {
		t.Helper()
		if retry.status != first.status || retry.body != first.body {
			t.Fatalf("%s = %d %s, want the original %d %s", name, retry.status, retry.body, first.status, first.body)
		}
	}

	receipt, key := h.receipt("us-east", userID, 10)
	first := app.do(http.MethodPost, objectsPath, map[string]string{"receipt": receipt}, "Mutation-Id", "replay-create")
	if first.status != http.StatusCreated {
		t.Fatalf("create = %d %s", first.status, first.body)
	}
	var created objectBody
	first.json(t, &created)
	wantReplay := strings.Replace(first.body, `"replayed":false`, `"replayed":true`, 1)
	for name, body := range retries(key) {
		retry := app.do(http.MethodPost, objectsPath, body, "Mutation-Id", "replay-create")
		if retry.status != http.StatusCreated || retry.body != wantReplay {
			t.Fatalf("create retry with %s = %d %s, want 201 %s", name, retry.status, retry.body, wantReplay)
		}
	}

	objectPath := objectsPath + "/" + created.Object.ID
	update, updateKey := h.receipt("us-east", userID, 20)
	first = app.do(http.MethodPut, objectPath, map[string]any{"version": 2, "receipt": update}, "Mutation-Id", "replay-update")
	if first.status != http.StatusOK {
		t.Fatalf("update = %d %s", first.status, first.body)
	}
	for name, body := range retries(updateKey) {
		body["version"] = 2
		expectSame("update retry with "+name, first, app.do(http.MethodPut, objectPath, body, "Mutation-Id", "replay-update"))
	}

	first = app.do(http.MethodPost, objectsPath, map[string]string{"receipt": receipt}, "Mutation-Id", "replay-used")
	if first.status != http.StatusConflict || first.body != `{"error":"receipt already used"}`+"\n" {
		t.Fatalf("create with a spent receipt = %d %s", first.status, first.body)
	}
	for name, body := range retries(key) {
		expectSame("spent-receipt retry with "+name, first, app.do(http.MethodPost, objectsPath, body, "Mutation-Id", "replay-used"))
	}

	stale, staleKey := h.receipt("us-east", userID, 30)
	first = app.do(http.MethodPut, objectPath, map[string]any{"version": 2, "receipt": stale}, "Mutation-Id", "replay-conflict")
	if first.status != http.StatusConflict || !strings.Contains(first.body, `"currentVersion":2`) {
		t.Fatalf("stale update = %d %s", first.status, first.body)
	}
	for name, body := range retries(staleKey) {
		body["version"] = 2
		expectSame("conflict retry with "+name, first, app.do(http.MethodPut, objectPath, body, "Mutation-Id", "replay-conflict"))
	}

	invalid := `{"error":"invalid receipt"}` + "\n"
	fresh, freshKey := h.receipt("us-east", userID, 40)
	if res := app.do(http.MethodPost, objectsPath, map[string]string{"receipt": expired(freshKey)}, "Mutation-Id", "fresh-create"); res.status != http.StatusBadRequest || res.body != invalid {
		t.Fatalf("fresh create with an expired receipt = %d %s, want 400", res.status, res.body)
	}
	if res := app.do(http.MethodPut, objectPath, map[string]any{"version": 3, "receipt": expired(freshKey)}, "Mutation-Id", "fresh-update"); res.status != http.StatusBadRequest || res.body != invalid {
		t.Fatalf("fresh update with an expired receipt = %d %s, want 400", res.status, res.body)
	}
	// A rejected receipt leaves the Mutation-Id unclaimed, so the client can
	// retry it with a valid one.
	if res := app.do(http.MethodPost, objectsPath, map[string]string{"receipt": fresh}, "Mutation-Id", "fresh-create"); res.status != http.StatusCreated {
		t.Fatalf("create after a rejected receipt = %d %s, want 201", res.status, res.body)
	}
}
