package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/oidc"
)

// testWebhookSecret is the Standard Webhooks secret the harness's fake Polar
// signs deliveries with: the whsec_ prefix over a base64 key.
const testWebhookSecret = "whsec_cG9sYXItd2ViaG9vay1rZXk="

// quotaHarness is an oidc-mode server with an account already signed in —
// the hosted service, where the Storage Quota is enforced and Entitlement
// gates writes. The dev UI is on so the recurring jobs can be driven over
// HTTP the way the /dev page drives them.
type quotaHarness struct {
	handoffHarness
	token        string
	collectionID string

	// polar is the fake Polar the checkout and portal routes call. Nothing
	// in the harness reaches the real API.
	polar *fakePolar
}

// newQuotaHarness is a hosted server with an entitled account that has
// claimed its vault, which is what every write below needs.
func newQuotaHarness(t *testing.T) quotaHarness {
	t.Helper()
	h := newHostedHarness(t)
	h.deliver(t, "msg_harness_active", "subscription.active", "active")

	claimed := h.do(t, http.MethodPost, "/api/collections", "")
	if claimed.StatusCode != http.StatusCreated {
		t.Fatalf("claim collection = %d %q", claimed.StatusCode, claimed.Body)
	}
	var claim struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	if err := json.Unmarshal([]byte(claimed.Body), &claim); err != nil {
		t.Fatal(err)
	}
	h.collectionID = claim.Collection.ID
	return h
}

// newHostedHarness is the same server with the account signed in but nothing
// subscribed and no vault claimed, which is where a first run starts.
func newHostedHarness(t *testing.T) quotaHarness {
	t.Helper()
	issuerURL, claims := newFakeIssuer(t, handoffClientID, handoffClientSecret)
	provider, err := oidc.Discover(context.Background(), issuerURL)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	root := t.TempDir()
	fakePolar := newFakePolar(t)
	cfg := config.Config{
		DatabaseURL:      "sqlite:" + filepath.Join(root, "notes.db"),
		AuthMode:         "oidc",
		CookieSecure:     false,
		DevUI:            true,
		BlobDir:          filepath.Join(root, "blobs"),
		OIDCIssuer:       issuerURL,
		OIDCClientID:     handoffClientID,
		OIDCClientSecret: handoffClientSecret,
		OIDCRedirectURI:  handoffRedirectURI,

		PolarBaseURL:       fakePolar.URL,
		PolarAccessToken:   "polar_oat_test",
		PolarProductID:     testPolarProductID,
		PolarDiscountID:    testPolarDiscountID,
		PolarWebhookSecret: testWebhookSecret,
	}
	database, err := db.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	hub := events.NewHub()
	server := httptest.NewServer(routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, hub,
		events.NewPublisher(database.Dialect(), hub), provider))
	t.Cleanup(server.Close)

	h := quotaHarness{handoffHarness: handoffHarness{
		url: server.URL, client: serverClient(server), claims: claims, database: database,
	}, polar: fakePolar}

	minted := h.mint(t)
	if callback := h.login(t, minted.Ticket, "quota-code"); callback.StatusCode != http.StatusOK {
		t.Fatalf("oidc callback = %d %q", callback.StatusCode, callback.Body)
	}
	redeemed := h.poll(t, minted.Ticket)
	if redeemed.StatusCode != http.StatusOK {
		t.Fatalf("handoff poll = %d %q", redeemed.StatusCode, redeemed.Body)
	}
	var login loginBody
	if err := json.Unmarshal([]byte(redeemed.Body), &login); err != nil {
		t.Fatal(err)
	}
	h.token = login.Token
	return h
}

func (h quotaHarness) do(t *testing.T, method, path, body string) routeTestResponse {
	t.Helper()
	return doRequest(t, h.client, method, h.url+path, body, h.token, nil)
}

type billingBody struct {
	Plan struct {
		StorageQuotaBytes int64 `json:"storage_quota_bytes"`
		BlobMaxBytes      int64 `json:"blob_max_bytes"`
	} `json:"plan"`
	Usage struct {
		BytesUsed int64 `json:"bytes_used"`
	} `json:"usage"`
}

func (h quotaHarness) billing(t *testing.T) billingBody {
	t.Helper()
	response := h.do(t, http.MethodGet, "/api/billing", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/billing = %d %q", response.StatusCode, response.Body)
	}
	var body billingBody
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func (h quotaHarness) assertBytesUsed(t *testing.T, want int64, when string) {
	t.Helper()
	if got := h.billing(t).Usage.BytesUsed; got != want {
		t.Fatalf("bytes_used %s = %d, want %d", when, got, want)
	}
}

// setQuota narrows the one plan so a test can reach its ceiling without
// uploading ten gigabytes.
func (h quotaHarness) setQuota(t *testing.T, bytes int64) {
	t.Helper()
	if _, err := h.database.ExecContext(context.Background(),
		`UPDATE plans SET storage_quota_bytes = $1 WHERE id = 'default'`, bytes); err != nil {
		t.Fatal(err)
	}
}

func (h quotaHarness) createObject(t *testing.T, ciphertext string) string {
	t.Helper()
	response := h.do(t, http.MethodPost, "/api/collections/"+h.collectionID+"/blob-objects", ciphertext)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create blob object = %d %q", response.StatusCode, response.Body)
	}
	var created struct {
		Object struct {
			ID string `json:"id"`
		} `json:"object"`
	}
	if err := json.Unmarshal([]byte(response.Body), &created); err != nil {
		t.Fatal(err)
	}
	return created.Object.ID
}

func TestBillingStatusReportsThePlanAndUsage(t *testing.T) {
	h := newQuotaHarness(t)

	body := h.billing(t)
	if body.Plan.StorageQuotaBytes != 10000000000 {
		t.Errorf("storage_quota_bytes = %d, want 10000000000", body.Plan.StorageQuotaBytes)
	}
	if body.Plan.BlobMaxBytes != 104857600 {
		t.Errorf("blob_max_bytes = %d, want 104857600", body.Plan.BlobMaxBytes)
	}
	if body.Usage.BytesUsed != 0 {
		t.Errorf("bytes_used = %d, want 0", body.Usage.BytesUsed)
	}
}

func TestStorageUsageRisesOnClaimIsFlatOnRetainAndFallsAfterGarbageCollection(t *testing.T) {
	h := newQuotaHarness(t)
	h.assertBytesUsed(t, 0, "before any upload")

	objectID := h.createObject(t, strings.Repeat("a", 1000))
	h.assertBytesUsed(t, 1000, "after a claim")

	updated := h.do(t, http.MethodPut,
		"/api/collections/"+h.collectionID+"/blob-objects/"+objectID+"?version=2", strings.Repeat("b", 700))
	if updated.StatusCode != http.StatusOK {
		t.Fatalf("update blob object = %d %q", updated.StatusCode, updated.Body)
	}
	// The superseded blob is retained as a merge ancestor, so it still counts:
	// the rise is the new claim alone.
	h.assertBytesUsed(t, 1700, "after an update retained the old blob")

	deleted := h.do(t, http.MethodDelete, "/api/collections/"+h.collectionID+"/objects/"+objectID, "")
	if deleted.StatusCode != http.StatusOK {
		t.Fatalf("delete object = %d %q", deleted.StatusCode, deleted.Body)
	}
	h.assertBytesUsed(t, 1700, "after a soft delete retained the blob")

	// Retained blobs are reclaimed 365 days after they stop being current.
	if _, err := h.database.ExecContext(context.Background(),
		`UPDATE blob_ledger SET state_changed_at = '2020-01-01T00:00:00.000Z' WHERE state = 'retained'`); err != nil {
		t.Fatal(err)
	}
	collected := h.do(t, http.MethodPost, "/dev/jobs/blob-gc", "")
	if collected.StatusCode != http.StatusOK {
		t.Fatalf("blob gc = %d %q", collected.StatusCode, collected.Body)
	}
	h.assertBytesUsed(t, 0, "after garbage collection reclaimed both blobs")
}

func TestDeletingACollectionGivesBackItsBytes(t *testing.T) {
	h := newQuotaHarness(t)
	h.createObject(t, strings.Repeat("a", 400))
	h.assertBytesUsed(t, 400, "after a claim")

	deleted := h.do(t, http.MethodDelete, "/api/collections/"+h.collectionID, "")
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("delete collection = %d %q", deleted.StatusCode, deleted.Body)
	}
	h.assertBytesUsed(t, 0, "after the collection delete made its blobs purgeable")
}

func TestStagedBytesDoNotCountUntilClaimed(t *testing.T) {
	h := newQuotaHarness(t)

	staged := h.do(t, http.MethodPost, "/api/blobs", strings.Repeat("a", 250))
	if staged.StatusCode != http.StatusCreated {
		t.Fatalf("stage blob = %d %q", staged.StatusCode, staged.Body)
	}
	h.assertBytesUsed(t, 0, "while the blob is only staged")

	var uploaded struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(staged.Body), &uploaded); err != nil {
		t.Fatal(err)
	}
	created := h.do(t, http.MethodPost, "/api/collections/"+h.collectionID+"/objects",
		`{"blob_key":"`+uploaded.Key+`","size_bytes":250}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("claim staged blob = %d %q", created.StatusCode, created.Body)
	}
	h.assertBytesUsed(t, 250, "after the object claimed it")
}

func TestUploadPastTheStorageQuotaIsRefused(t *testing.T) {
	h := newQuotaHarness(t)
	h.setQuota(t, 1000)
	h.createObject(t, strings.Repeat("a", 900))

	refused := h.do(t, http.MethodPost, "/api/blobs", strings.Repeat("b", 200))
	if refused.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("over-quota upload = %d %q, want 507", refused.StatusCode, refused.Body)
	}
	var body struct {
		Error      string `json:"error"`
		Code       string `json:"code"`
		QuotaBytes int64  `json:"quota_bytes"`
		BytesUsed  int64  `json:"bytes_used"`
	}
	if err := json.Unmarshal([]byte(refused.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "quota_exceeded" {
		t.Errorf("code = %q, want quota_exceeded", body.Code)
	}
	if body.QuotaBytes != 1000 || body.BytesUsed != 900 {
		t.Errorf("body names quota %d and usage %d, want 1000 and 900", body.QuotaBytes, body.BytesUsed)
	}

	// Filling the quota exactly is still allowed.
	if fits := h.do(t, http.MethodPost, "/api/blobs", strings.Repeat("b", 100)); fits.StatusCode != http.StatusCreated {
		t.Fatalf("upload that exactly fills the quota = %d %q, want 201", fits.StatusCode, fits.Body)
	}

	inline := h.do(t, http.MethodPost, "/api/collections/"+h.collectionID+"/blob-objects", strings.Repeat("c", 200))
	if inline.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("over-quota inline create = %d %q, want 507", inline.StatusCode, inline.Body)
	}
}

func TestBatchUploadPastTheStorageQuotaIsRefusedPerEntry(t *testing.T) {
	h := newQuotaHarness(t)
	h.setQuota(t, 1000)

	batch := string(batchFrame(0, "batch-fits", 0, []byte(strings.Repeat("a", 900)))) +
		string(batchFrame(0, "batch-overflows", 0, []byte(strings.Repeat("b", 200))))
	response := h.do(t, http.MethodPost, "/api/collections/"+h.collectionID+"/blob-objects/batch", batch)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("batch = %d %q", response.StatusCode, response.Body)
	}
	var body struct {
		Results []struct {
			Status     string `json:"status"`
			QuotaBytes int64  `json:"quota_bytes"`
			BytesUsed  int64  `json:"bytes_used"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(body.Results))
	}
	if body.Results[0].Status != "created" {
		t.Errorf("first entry = %q, want created", body.Results[0].Status)
	}
	if body.Results[1].Status != "quota_exceeded" {
		t.Errorf("second entry = %q, want quota_exceeded", body.Results[1].Status)
	}
	if body.Results[1].QuotaBytes != 1000 || body.Results[1].BytesUsed != 900 {
		t.Errorf("refused entry names quota %d and usage %d, want 1000 and 900",
			body.Results[1].QuotaBytes, body.Results[1].BytesUsed)
	}
	h.assertBytesUsed(t, 900, "after the batch")
}

func TestPerBlobCapComesFromThePlan(t *testing.T) {
	h := newQuotaHarness(t)
	if _, err := h.database.ExecContext(context.Background(),
		`UPDATE plans SET blob_max_bytes = 64 WHERE id = 'default'`); err != nil {
		t.Fatal(err)
	}

	tooLarge := h.do(t, http.MethodPost, "/api/blobs", strings.Repeat("a", 65))
	if tooLarge.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload past the plan's per-blob cap = %d %q, want 413", tooLarge.StatusCode, tooLarge.Body)
	}
	if fits := h.do(t, http.MethodPost, "/api/blobs", strings.Repeat("a", 64)); fits.StatusCode != http.StatusCreated {
		t.Fatalf("upload at the plan's per-blob cap = %d %q, want 201", fits.StatusCode, fits.Body)
	}

	batch := string(batchFrame(0, "batch-too-large", 0, []byte(strings.Repeat("a", 65))))
	response := h.do(t, http.MethodPost, "/api/collections/"+h.collectionID+"/blob-objects/batch", batch)
	if !strings.Contains(response.Body, `"too_large"`) {
		t.Fatalf("batch entry past the plan's per-blob cap = %d %q, want too_large", response.StatusCode, response.Body)
	}
}

func TestUsageReconciliationCorrectsADriftedCounter(t *testing.T) {
	h := newQuotaHarness(t)
	h.createObject(t, strings.Repeat("a", 500))
	h.assertBytesUsed(t, 500, "after a claim")

	if _, err := h.database.ExecContext(context.Background(),
		`UPDATE user_usage SET bytes_used = 999999`); err != nil {
		t.Fatal(err)
	}
	h.assertBytesUsed(t, 999999, "after deliberately drifting the counter")

	reconciled := h.do(t, http.MethodPost, "/dev/jobs/usage", "")
	if reconciled.StatusCode != http.StatusOK {
		t.Fatalf("usage reconciliation = %d %q", reconciled.StatusCode, reconciled.Body)
	}
	if !strings.Contains(reconciled.Body, `"corrected":1`) {
		t.Fatalf("usage reconciliation body = %q, want one correction", reconciled.Body)
	}
	h.assertBytesUsed(t, 500, "after reconciliation recomputed it from the ledger")
}

func TestSelfHostedDeploymentsHaveNoStorageQuota(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		DatabaseURL:  "sqlite:" + filepath.Join(root, "notes.db"),
		AuthMode:     "dev",
		CookieSecure: false,
		BlobDir:      filepath.Join(root, "blobs"),
	}
	database, err := db.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	hub := events.NewHub()
	server := httptest.NewServer(routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, hub,
		events.NewPublisher(database.Dialect(), hub), nil))
	defer server.Close()
	client := serverClient(server)

	login := doRequest(t, client, http.MethodPost, server.URL+"/api/auth/dev/login",
		`{"email":"selfhosted@example.invalid","name":"Self Hosted"}`, "", nil)
	var loggedIn struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(login.Body), &loggedIn); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(context.Background(),
		`UPDATE plans SET storage_quota_bytes = 1 WHERE id = 'default'`); err != nil {
		t.Fatal(err)
	}

	staged := doRequest(t, client, http.MethodPost, server.URL+"/api/blobs", strings.Repeat("a", 500), loggedIn.Token, nil)
	if staged.StatusCode != http.StatusCreated {
		t.Fatalf("self-hosted upload past the plan quota = %d %q, want 201", staged.StatusCode, staged.Body)
	}
	if billing := doRequest(t, client, http.MethodGet, server.URL+"/api/billing", "", loggedIn.Token, nil); billing.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /api/billing off oidc = %d %q, want 404", billing.StatusCode, billing.Body)
	}
}

// stage uploads ciphertext through the two-call path and returns its key.
func (h quotaHarness) stage(t *testing.T, ciphertext string) string {
	t.Helper()
	response := h.do(t, http.MethodPost, "/api/blobs", ciphertext)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("stage blob = %d %q", response.StatusCode, response.Body)
	}
	var uploaded struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(response.Body), &uploaded); err != nil {
		t.Fatal(err)
	}
	return uploaded.Key
}

// TestStagingAheadCannotWalkPastTheQuota is the bypass the upload-time check
// alone allows: staged bytes never move the counter, so any number of blobs
// pass an upload check taken against an empty account. The claim is where the
// bytes become the account's, so that is where the refusal has to land.
func TestStagingAheadCannotWalkPastTheQuota(t *testing.T) {
	h := newQuotaHarness(t)
	h.setQuota(t, 1000)

	// Every one of these passes the upload check: the counter is still zero.
	keys := make([]string, 5)
	for i := range keys {
		keys[i] = h.stage(t, strings.Repeat("a", 900))
	}

	claim := func(key string) routeTestResponse {
		return h.do(t, http.MethodPost, "/api/collections/"+h.collectionID+"/objects",
			`{"blob_key":"`+key+`","size_bytes":900}`)
	}
	if first := claim(keys[0]); first.StatusCode != http.StatusCreated {
		t.Fatalf("first claim = %d %q, want 201", first.StatusCode, first.Body)
	}
	h.assertBytesUsed(t, 900, "after the first claim")

	for _, key := range keys[1:] {
		refused := claim(key)
		if refused.StatusCode != http.StatusInsufficientStorage {
			t.Fatalf("claim past the quota = %d %q, want 507", refused.StatusCode, refused.Body)
		}
		if !strings.Contains(refused.Body, `"quota_exceeded"`) {
			t.Fatalf("refused claim body = %q, want quota_exceeded", refused.Body)
		}
	}
	h.assertBytesUsed(t, 900, "after four claims the quota had no room for")
}

// TestAQuotaRefusalLeavesTheMutationIdReusable checks that a refused claim
// does not spend its Mutation-Id. A quota refusal is transient: once the
// account has room, the same retry has to be able to succeed.
func TestAQuotaRefusalLeavesTheMutationIdReusable(t *testing.T) {
	h := newQuotaHarness(t)
	// Staged under the seeded quota, then the ceiling drops beneath it, which
	// is what an account close to full looks like by the time it claims.
	key := h.stage(t, strings.Repeat("a", 900))
	h.setQuota(t, 500)

	claim := func() routeTestResponse {
		request, err := http.NewRequest(http.MethodPost,
			h.url+"/api/collections/"+h.collectionID+"/objects",
			strings.NewReader(`{"blob_key":"`+key+`","size_bytes":900}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+h.token)
		request.Header.Set("Mutation-Id", "retry-after-room")
		response, err := h.client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return routeTestResponse{StatusCode: response.StatusCode, Body: string(body)}
	}

	if refused := claim(); refused.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("claim over quota = %d %q, want 507", refused.StatusCode, refused.Body)
	}
	h.setQuota(t, 1000)
	accepted := claim()
	if accepted.StatusCode != http.StatusCreated {
		t.Fatalf("retry after the quota rose = %d %q, want 201; the refusal spent the Mutation-Id",
			accepted.StatusCode, accepted.Body)
	}
	h.assertBytesUsed(t, 900, "after the retry succeeded")
}
