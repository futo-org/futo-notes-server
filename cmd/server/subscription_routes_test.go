package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/polar"
)

// newSelfHostedServer is a dev-mode server on SQLite: no OIDC, no Polar, and
// therefore none of the hosted surface.
func newSelfHostedServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
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
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	hub := events.NewHub()
	server := httptest.NewServer(routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, hub,
		events.NewPublisher(database.Dialect(), hub), nil))
	t.Cleanup(server.Close)
	return server, serverClient(server)
}

func capabilityBilling(t *testing.T, client *http.Client, url string) bool {
	t.Helper()
	response := doRequest(t, client, http.MethodGet, url+"/", "", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d %q", response.StatusCode, response.Body)
	}
	var body struct {
		Billing *bool `json:"billing"`
	}
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Billing == nil {
		t.Fatalf("capability has no billing field: %s", response.Body)
	}
	return *body.Billing
}

func TestCapabilityReportsBillingOnlyOnTheHostedService(t *testing.T) {
	hosted := newQuotaHarness(t)
	if !capabilityBilling(t, hosted.client, hosted.url) {
		t.Error("hosted capability billing = false, want true so the app offers sign-up")
	}

	server, client := newSelfHostedServer(t)
	if capabilityBilling(t, client, server.URL) {
		t.Error("self-hosted capability billing = true, want false")
	}
}

func TestASelfHostedServerHasNoHostedBillingRoutes(t *testing.T) {
	server, client := newSelfHostedServer(t)
	token := devLoginToken(t, client, server.URL)

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/billing"},
		{http.MethodPost, "/api/billing/polar/webhook"},
	} {
		response := doRequest(t, client, route.method, server.URL+route.path, "{}", token, nil)
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s off the hosted service = %d %q, want 404",
				route.method, route.path, response.StatusCode, response.Body)
		}
	}

	// A delivery signed with the right secret is still refused, because a
	// self-hosted server mounts no webhook for it to reach.
	header, err := polar.Sign(testWebhookSecret, "msg_1", time.Now(), []byte(`{"type":"subscription.active"}`))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/billing/polar/webhook",
		strings.NewReader(`{"type":"subscription.active"}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		request.Header[name] = values
	}
	if response := readResponse(t, client, request); response.StatusCode == http.StatusOK {
		t.Errorf("a self-hosted server accepted a Polar delivery: %d %q", response.StatusCode, response.Body)
	}
}

func devLoginToken(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	response := doRequest(t, client, http.MethodPost, url+"/api/auth/dev/login",
		`{"email":"self-hosted@example.invalid","name":"Self Hosted"}`, "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dev login = %d %q", response.StatusCode, response.Body)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(response.Body), &login); err != nil {
		t.Fatal(err)
	}
	return login.Token
}

// sendWebhook is the fake Polar: it signs body with the harness's webhook
// secret and posts it to the real, unauthenticated route.
func (h quotaHarness) sendWebhook(t *testing.T, deliveryID string, at time.Time, body string) routeTestResponse {
	t.Helper()
	header, err := polar.Sign(testWebhookSecret, deliveryID, at, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, h.url+"/api/billing/polar/webhook", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		request.Header[name] = values
	}
	return readResponse(t, h.client, request)
}

// subscriptionEvent is a Polar-shaped delivery body for externalID.
func subscriptionEvent(eventType, status, externalID string) string {
	periodEnd := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	body, err := polar.Delivery(polar.Event{
		Type: eventType,
		Subscription: polar.Subscription{
			ID:                 "sub_1",
			Status:             status,
			CustomerID:         "cus_1",
			ExternalCustomerID: externalID,
			CurrentPeriodEnd:   &periodEnd,
		},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// entitlementBody is what GET /api/billing adds once a subscription exists.
type entitlementBody struct {
	Entitled   bool    `json:"entitled"`
	State      string  `json:"state"`
	GraceUntil *string `json:"grace_until"`
}

func (h quotaHarness) entitlement(t *testing.T) entitlementBody {
	t.Helper()
	response := h.do(t, http.MethodGet, "/api/billing", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/billing = %d %q", response.StatusCode, response.Body)
	}
	var body entitlementBody
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// deliver signs and sends an event for the harness account, and requires the
// route to have applied it.
func (h quotaHarness) deliver(t *testing.T, deliveryID, eventType, status string) {
	t.Helper()
	response := h.sendWebhook(t, deliveryID, time.Now(),
		subscriptionEvent(eventType, status, h.claims.Sub))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("webhook %s = %d %q", eventType, response.StatusCode, response.Body)
	}
	if !strings.Contains(response.Body, `"applied"`) {
		t.Fatalf("webhook %s was not applied: %q", eventType, response.Body)
	}
}

func TestBillingStatusReportsEntitlementBeforeAndAfterASubscription(t *testing.T) {
	h := newHostedHarness(t)

	before := h.entitlement(t)
	if before.Entitled {
		t.Error("entitled = true before any subscription")
	}
	if before.State != "none" {
		t.Errorf("state = %q before any subscription, want \"none\"", before.State)
	}
	if before.GraceUntil != nil {
		t.Errorf("grace_until = %v before any subscription, want null", *before.GraceUntil)
	}

	h.deliver(t, "msg_active", "subscription.active", "active")

	after := h.entitlement(t)
	if !after.Entitled || after.State != "active" {
		t.Fatalf("after an active event: entitled = %v, state = %q", after.Entitled, after.State)
	}
	if after.GraceUntil != nil {
		t.Errorf("grace_until = %v while paid up, want null", *after.GraceUntil)
	}

	// The plan and usage the earlier step added are still there beside it.
	if h.billing(t).Plan.StorageQuotaBytes != 10000000000 {
		t.Error("the entitlement fields displaced the plan")
	}
}

func TestBillingStatusReportsTheGracePeriodAfterAFailedPayment(t *testing.T) {
	h := newHostedHarness(t)
	h.deliver(t, "msg_past_due", "subscription.past_due", "past_due")

	status := h.entitlement(t)
	if !status.Entitled || status.State != "past_due" {
		t.Fatalf("entitled = %v, state = %q, want a past-due account still writing",
			status.Entitled, status.State)
	}
	if status.GraceUntil == nil {
		t.Fatal("grace_until = null after a failed payment")
	}
	graceUntil, err := time.Parse(db.TimestampFormat, *status.GraceUntil)
	if err != nil {
		t.Fatalf("grace_until %q is not a server timestamp: %v", *status.GraceUntil, err)
	}
	if until := time.Until(graceUntil); until < 6*24*time.Hour || until > 7*24*time.Hour {
		t.Errorf("grace_until is %s away, want about seven days", until)
	}
}

// gatedWrites is every route the Entitlement gate covers: the ones that
// mutate collection contents or stage blobs, plus the vault key write and
// the vault delete.
func (h quotaHarness) gatedWrites(objectID string) []struct {
	name, method, path, body string
} {
	collection := "/api/collections/" + h.collectionID
	return []struct{ name, method, path, body string }{
		{"object create", http.MethodPost, collection + "/objects", `{"content":"x"}`},
		{"object update", http.MethodPut, collection + "/objects/" + objectID + "?version=2", `{"content":"y"}`},
		{"object delete", http.MethodDelete, collection + "/objects/" + objectID + "?version=2", ""},
		{"blob object create", http.MethodPost, collection + "/blob-objects", "ciphertext"},
		{"blob object update", http.MethodPut, collection + "/blob-objects/" + objectID + "?version=2", "ciphertext"},
		{"batch create", http.MethodPost, collection + "/blob-objects/batch", "\x00\x00\x00\x00"},
		{"blob stage", http.MethodPost, "/api/blobs", "ciphertext"},
		{"vault key write", http.MethodPut, collection + "/key",
			`{"key_salt":"s","key_kdf":{"n":1},"encrypted_vault_key":"k"}`},
		// Listed last: entitled, this one really would delete the vault.
		{"vault delete", http.MethodDelete, collection, ""},
	}
}

// ungatedReads is the surface a lapsed account keeps: pulls, downloads,
// batch fetch, capability, auth, and billing itself.
func (h quotaHarness) ungatedReads(objectID string) []struct {
	name, method, path, body string
} {
	collection := "/api/collections/" + h.collectionID
	return []struct{ name, method, path, body string }{
		{"list collections", http.MethodGet, "/api/collections", ""},
		{"get collection", http.MethodGet, collection, ""},
		{"pull objects", http.MethodGet, collection + "/objects", ""},
		{"get object", http.MethodGet, collection + "/objects/" + objectID, ""},
		{"get vault key", http.MethodGet, collection + "/key", ""},
		{"batch fetch blobs", http.MethodPost, "/api/blobs/batch", `{"keys":[]}`},
		{"current user", http.MethodGet, "/api/auth", ""},
		{"billing status", http.MethodGet, "/api/billing", ""},
	}
}

func TestALapsedAccountReadsEverythingAndWritesNothing(t *testing.T) {
	h := newQuotaHarness(t)
	objectID := h.createObject(t, "first version")
	h.deliver(t, "msg_revoked", "subscription.revoked", "canceled")

	if h.entitlement(t).Entitled {
		t.Fatal("entitled = true after the subscription was revoked")
	}

	for _, write := range h.gatedWrites(objectID) {
		response := h.do(t, write.method, write.path, write.body)
		if response.StatusCode != http.StatusPaymentRequired {
			t.Errorf("%s = %d %q, want 402", write.name, response.StatusCode, response.Body)
			continue
		}
		if !strings.Contains(response.Body, `"subscription_required"`) {
			t.Errorf("%s body = %q, want the subscription_required code", write.name, response.Body)
		}
	}

	for _, read := range h.ungatedReads(objectID) {
		response := h.do(t, read.method, read.path, read.body)
		if response.StatusCode == http.StatusPaymentRequired {
			t.Errorf("%s = 402 %q, want a lapsed account to keep reading", read.name, response.Body)
		}
	}

	// Downloading the ciphertext already stored is the read that matters most.
	download := h.do(t, http.MethodGet, "/api/blobs/"+h.blobKeyOf(t, objectID), "")
	if download.StatusCode != http.StatusOK {
		t.Errorf("blob download = %d %q, want a lapsed account to keep its data",
			download.StatusCode, download.Body)
	}
}

// blobKeyOf reads the "<user>/<blob>" key the object's current version
// points at, which is the path its ciphertext downloads from.
func (h quotaHarness) blobKeyOf(t *testing.T, objectID string) string {
	t.Helper()
	response := h.do(t, http.MethodGet, "/api/collections/"+h.collectionID+"/objects/"+objectID, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get object = %d %q", response.StatusCode, response.Body)
	}
	var body struct {
		Object struct {
			BlobKey *string `json:"blob_key"`
		} `json:"object"`
	}
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Object.BlobKey == nil {
		t.Fatalf("object %s has no blob key", objectID)
	}
	return *body.Object.BlobKey
}

func TestAnActiveSubscriptionUnlocksWrites(t *testing.T) {
	h := newHostedHarness(t)

	// The vault claim is not a write to collection contents, so a
	// never-subscribed account can still get to the point of pulling.
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

	blocked := h.do(t, http.MethodPost, "/api/collections/"+h.collectionID+"/blob-objects", "ciphertext")
	if blocked.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("write before subscribing = %d %q, want 402", blocked.StatusCode, blocked.Body)
	}

	h.deliver(t, "msg_first_active", "subscription.active", "active")

	unlocked := h.do(t, http.MethodPost, "/api/collections/"+h.collectionID+"/blob-objects", "ciphertext")
	if unlocked.StatusCode != http.StatusCreated {
		t.Fatalf("write after subscribing = %d %q, want 201", unlocked.StatusCode, unlocked.Body)
	}
}

// expireGracePeriod moves the stored deadline into the past, which is how
// these tests reach the end of a seven-day window without sleeping through
// it. The same nudge is what the quota tests use to age blob-ledger rows.
func (h quotaHarness) expireGracePeriod(t *testing.T) {
	t.Helper()
	past := db.Timestamp(time.Now().Add(-time.Minute))
	result, err := h.database.ExecContext(context.Background(),
		`UPDATE subscriptions SET grace_until = $1`, past)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("expiring the Grace Period touched %d rows (%v)", rows, err)
	}
}

func TestAPastDueAccountWritesUntilTheGracePeriodEnds(t *testing.T) {
	h := newQuotaHarness(t)
	write := func() routeTestResponse {
		return h.do(t, http.MethodPost, "/api/collections/"+h.collectionID+"/blob-objects", "ciphertext")
	}

	h.deliver(t, "msg_past_due", "subscription.past_due", "past_due")
	if response := write(); response.StatusCode != http.StatusCreated {
		t.Fatalf("write inside the Grace Period = %d %q, want 201", response.StatusCode, response.Body)
	}

	h.expireGracePeriod(t)
	response := write()
	if response.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("write after the Grace Period = %d %q, want 402", response.StatusCode, response.Body)
	}
	if !strings.Contains(response.Body, `"subscription_required"`) {
		t.Fatalf("body = %q, want the subscription_required code", response.Body)
	}
	if status := h.entitlement(t); status.Entitled || status.State != "past_due" {
		t.Fatalf("status = %+v, want past_due and not entitled", status)
	}

	// Recovering re-opens writes and clears the deadline.
	h.deliver(t, "msg_recovered", "subscription.active", "active")
	if response := write(); response.StatusCode != http.StatusCreated {
		t.Fatalf("write after recovery = %d %q, want 201", response.StatusCode, response.Body)
	}
	if status := h.entitlement(t); status.GraceUntil != nil {
		t.Fatalf("grace_until = %v after recovery, want null", *status.GraceUntil)
	}
}

func TestABadlySignedDeliveryChangesNothing(t *testing.T) {
	h := newHostedHarness(t)
	body := subscriptionEvent("subscription.active", "active", h.claims.Sub)

	forged, err := polar.Sign("whsec_c29tZWJvZHktZWxzZXMta2V5", "msg_forged", time.Now(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, h.url+"/api/billing/polar/webhook", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range forged {
		request.Header[name] = values
	}
	response := readResponse(t, h.client, request)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged delivery = %d %q, want 401", response.StatusCode, response.Body)
	}
	// The webhook route has to be the one refusing this, not the session
	// middleware that guards the rest of /api/ — both answer 401.
	if !strings.Contains(response.Body, `"invalid_signature"`) {
		t.Fatalf("forged delivery body = %q, want the webhook route's invalid_signature", response.Body)
	}
	if status := h.entitlement(t); status.Entitled || status.State != "none" {
		t.Fatalf("a forged delivery moved the record: %+v", status)
	}

	// A signed delivery whose body was edited afterwards is refused too.
	edited := h.sendWebhook(t, "msg_edited", time.Now(), body)
	if edited.StatusCode != http.StatusOK {
		t.Fatalf("control delivery = %d %q", edited.StatusCode, edited.Body)
	}
	tampered, err := polar.Sign(testWebhookSecret, "msg_tampered", time.Now(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequest(http.MethodPost, h.url+"/api/billing/polar/webhook",
		strings.NewReader(subscriptionEvent("subscription.revoked", "canceled", h.claims.Sub)))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range tampered {
		request.Header[name] = values
	}
	if response := readResponse(t, h.client, request); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("edited delivery = %d %q, want 401", response.StatusCode, response.Body)
	}
	if status := h.entitlement(t); !status.Entitled || status.State != "active" {
		t.Fatalf("an edited delivery moved the record: %+v", status)
	}
}

func TestReplayingADeliveryIsANoOp(t *testing.T) {
	h := newHostedHarness(t)
	body := subscriptionEvent("subscription.past_due", "past_due", h.claims.Sub)
	at := time.Now()

	first := h.sendWebhook(t, "msg_replay", at, body)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first delivery = %d %q", first.StatusCode, first.Body)
	}
	before := h.entitlement(t)

	// The very same bytes, headers included, as Polar's retry would send.
	second := h.sendWebhook(t, "msg_replay", at, body)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("replayed delivery = %d %q", second.StatusCode, second.Body)
	}
	after := h.entitlement(t)

	if before.Entitled != after.Entitled || before.State != after.State {
		t.Fatalf("replay moved the record from %+v to %+v", before, after)
	}
	if before.GraceUntil == nil || after.GraceUntil == nil || *before.GraceUntil != *after.GraceUntil {
		t.Fatalf("replay moved the Grace Period deadline from %v to %v",
			before.GraceUntil, after.GraceUntil)
	}
}

func TestUnknownEventTypesAreAcknowledgedAndIgnored(t *testing.T) {
	h := newHostedHarness(t)
	h.deliver(t, "msg_active", "subscription.active", "active")

	for _, eventType := range []string{"order.paid", "benefit_grant.created", "checkout.updated"} {
		response := h.sendWebhook(t, "msg_"+eventType, time.Now(),
			subscriptionEvent(eventType, "canceled", h.claims.Sub))
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s = %d %q, want 200 so Polar stops retrying", eventType, response.StatusCode, response.Body)
		}
		if !strings.Contains(response.Body, `"ignored"`) {
			t.Errorf("%s body = %q, want it reported as ignored", eventType, response.Body)
		}
	}
	if status := h.entitlement(t); !status.Entitled || status.State != "active" {
		t.Fatalf("an ignored event moved the record: %+v", status)
	}
}

func TestADeliveryForAnUnknownCustomerIsAcknowledged(t *testing.T) {
	h := newHostedHarness(t)

	response := h.sendWebhook(t, "msg_stranger", time.Now(),
		subscriptionEvent("subscription.active", "active", "somebody-elses-sub"))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delivery for an unknown customer = %d %q, want 200", response.StatusCode, response.Body)
	}
	if !strings.Contains(response.Body, `"unknown_customer"`) {
		t.Errorf("body = %q, want it reported as an unknown customer", response.Body)
	}
	if status := h.entitlement(t); status.Entitled {
		t.Fatal("a delivery for another customer entitled this account")
	}
}

func TestTheDevCardSendsARealSignedDelivery(t *testing.T) {
	h := newHostedHarness(t)
	session := &http.Cookie{Name: "session", Value: h.token}

	before := doRequest(t, h.client, http.MethodGet, h.url+"/dev/subscription/last", "", "", session)
	if before.StatusCode != http.StatusNotFound {
		t.Fatalf("last webhook before any delivery = %d %q, want 404", before.StatusCode, before.Body)
	}

	sent := doRequest(t, h.client, http.MethodPost, h.url+"/dev/subscription",
		`{"type":"subscription.active","status":"active"}`, "", session)
	if sent.StatusCode != http.StatusOK || !strings.Contains(sent.Body, `"applied"`) {
		t.Fatalf("dev delivery = %d %q, want the webhook route to apply it", sent.StatusCode, sent.Body)
	}
	if status := h.entitlement(t); !status.Entitled || status.State != "active" {
		t.Fatalf("status after the dev delivery = %+v", status)
	}

	after := doRequest(t, h.client, http.MethodGet, h.url+"/dev/subscription/last", "", "", session)
	if after.StatusCode != http.StatusOK {
		t.Fatalf("last webhook = %d %q", after.StatusCode, after.Body)
	}
	var last struct {
		ReceivedAt string `json:"received_at"`
		Event      struct {
			Type string `json:"type"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(after.Body), &last); err != nil {
		t.Fatal(err)
	}
	if last.Event.Type != "subscription.active" || last.ReceivedAt == "" {
		t.Fatalf("last webhook = %+v", last)
	}

	// Without a session the dev sender has no account to address.
	anonymous := doRequest(t, h.client, http.MethodPost, h.url+"/dev/subscription",
		`{"type":"subscription.active","status":"active"}`, "", nil)
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Errorf("dev delivery without a session = %d %q, want 401", anonymous.StatusCode, anonymous.Body)
	}
}
