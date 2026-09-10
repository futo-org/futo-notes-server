package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"futo-notes-server/internal/polar"
)

// The identity the rehearsal signs in as. The sub is what the account is
// known by at Polar, so the signed webhook below names this same string.
const (
	personSub   = "rehearsal-sub-1"
	personEmail = "rehearsal@" + allowedDomain
	personName  = "Rehearsal Person"
)

// outsiderEmail is on no allowed domain, so a login carrying it must be
// refused and must create no account.
const outsiderEmail = "outsider@example.test"

// ciphertext is the body of the one object the rehearsal writes. The server
// stores blobs opaquely, so any bytes will do; the length is what shows up
// as Storage Usage.
const ciphertext = "hosted-rehearsal-ciphertext"

// The one Plan's limits, as migration 013 seeds them: 10 GB decimal of
// Storage Quota and the 100 MiB per-blob cap the server enforced as a
// constant before Plans existed. A hosted account has to read back exactly
// these, so the rehearsal names them rather than repeating the digits.
const (
	storageQuotaBytes int64 = 10000000000
	blobMaxBytes      int64 = 104857600
)

// session carries everything the walk needs: the app's HTTP client, the
// server under test, and the two fakes it can inspect and steer.
type session struct {
	client *http.Client
	server *serverProcess
	issuer *fakeIssuer
	polar  *fakePolar

	// databaseURL is the scratch database, which the quota step edits the
	// Plan row in directly.
	databaseURL string

	// token is the bearer credential the Login Hand-off produced, which is
	// what the app authenticates every later call with.
	token string
	// spentTicket is the ticket the app already redeemed.
	spentTicket string
	// collectionID is the account's Vault.
	collectionID string
}

// walk is the hosted path in the order a person goes through it. Each step
// returns an error naming what it expected, so a failure reads as a sentence
// rather than a diff.
func walk(s *session) error {
	steps := []struct {
		name string
		run  func() error
	}{
		{"the capability document advertises hosted mode", s.checkCapability},
		{"a Login Hand-off signs the app in", s.completeHandoff},
		{"a redeemed ticket cannot be redeemed twice", s.checkTicketSpent},
		{"a login off the allowed domain is refused", s.checkAllowlist},
		{"billing reads as unsubscribed, on the plan, using nothing", s.checkBillingBeforePaying},
		{"claiming the Vault is not gated", s.claimVault},
		{"a write is refused without an Entitlement", s.checkWriteRefused},
		{"a checkout carries the product, the discount, and the success page", s.startCheckout},
		{"the success page is served without a session", s.checkDonePage},
		{"a signed Polar delivery makes the account entitled", s.applySubscription},
		{"the same write is now accepted", s.checkWriteAccepted},
		{"billing reports entitled, active, and the bytes stored", s.checkBillingAfterPaying},
		{"the customer portal reuses the registered customer", s.openPortal},
		{"an entitled account asking to check out gets its status", s.checkoutWhileEntitled},
		{"a write past the Storage Quota is refused", s.checkQuota},
		{"a revoked subscription stops writes again", s.revokeSubscription},
	}
	for _, check := range steps {
		if err := check.run(); err != nil {
			return fmt.Errorf("%s: %w", check.name, err)
		}
		step("%s", check.name)
	}
	return nil
}

// --- steps ---

func (s *session) checkCapability() error {
	var capability struct {
		AuthMode string `json:"auth_mode"`
		Signup   string `json:"signup"`
		Billing  bool   `json:"billing"`
	}
	// Fetched without credentials: the capability document is what an app
	// reads before it has any, to decide whether to offer the sign-up path.
	response, err := http.Get(s.server.origin + "/")
	if err != nil {
		return err
	}
	body, err := readAll(response)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the capability document = %d %q, want 200", response.StatusCode, truncate(body))
	}
	if err := json.Unmarshal(body, &capability); err != nil {
		return err
	}
	if capability.AuthMode != "oidc" {
		return fmt.Errorf("auth_mode = %q, want oidc", capability.AuthMode)
	}
	if !capability.Billing {
		return fmt.Errorf("billing = false, want true: the app decides whether to show the sign-up path from this")
	}
	if capability.Signup != "open" {
		return fmt.Errorf("signup = %q, want open", capability.Signup)
	}
	return nil
}

// completeHandoff is the whole login: the app mints a ticket, opens the URL
// in a browser, and polls until the ticket hands back a session.
func (s *session) completeHandoff() error {
	s.issuer.signInAs(personSub, personEmail, personName)

	ticket, loginURL, err := s.mintHandoff()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(loginURL, s.server.origin+"/api/auth/oidc/start?") {
		return fmt.Errorf("login URL = %q, want the OIDC start route on this server", loginURL)
	}

	// Nothing has happened in the browser yet, so the app is told to wait.
	if status, _, err := s.redeem(ticket); err != nil {
		return err
	} else if status != http.StatusAccepted {
		return fmt.Errorf("polling a ticket before login = %d, want 202", status)
	}

	if err := s.openInBrowser(loginURL); err != nil {
		return err
	}

	status, body, err := s.redeem(ticket)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("redeeming a completed ticket = %d %q, want 200", status, body)
	}
	var login struct {
		User struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &login); err != nil {
		return err
	}
	if login.User.Email != personEmail {
		return fmt.Errorf("signed in as %q, want %q", login.User.Email, personEmail)
	}
	if login.Token == "" {
		return fmt.Errorf("the hand-off returned no token, so the app has nothing to authenticate with")
	}
	s.token = login.Token
	s.spentTicket = ticket
	return nil
}

// checkTicketSpent proves the hand-off body is handed over exactly once. An
// app that polls again — or anyone who reads the ticket out of a log — finds
// the same 404 an unknown ticket gets.
func (s *session) checkTicketSpent() error {
	status, body, err := s.redeem(s.spentTicket)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("redeeming a spent ticket a second time = %d %q, want 404", status, body)
	}
	return nil
}

// checkAllowlist proves the internal-MVP gate: a login from an email domain
// that is not on HOSTED_ALLOWED_EMAIL_DOMAINS is refused, the app is not left
// polling a login that will never arrive, and no account is created.
func (s *session) checkAllowlist() error {
	s.issuer.signInAs("rehearsal-outsider", outsiderEmail, "Outsider")
	defer s.issuer.signInAs(personSub, personEmail, personName)

	ticket, loginURL, err := s.mintHandoff()
	if err != nil {
		return err
	}
	response, err := s.client.Get(loginURL)
	if err != nil {
		return err
	}
	body, err := readAll(response)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusForbidden {
		return fmt.Errorf("a login off the allowed domain = %d %q, want 403", response.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("signup_not_open")) {
		return fmt.Errorf("refusal body = %q, want the signup_not_open code", body)
	}

	status, _, err := s.redeem(ticket)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("polling a refused ticket = %d, want 404 rather than a wait that never ends", status)
	}
	return nil
}

func (s *session) checkBillingBeforePaying() error {
	status, err := s.billing()
	if err != nil {
		return err
	}
	if status.Entitled {
		return fmt.Errorf("a brand-new account reads as entitled")
	}
	if status.State != "none" {
		return fmt.Errorf("state = %q, want none", status.State)
	}
	if status.Plan.StorageQuotaBytes != storageQuotaBytes {
		return fmt.Errorf("storage_quota_bytes = %d, want %d", status.Plan.StorageQuotaBytes, storageQuotaBytes)
	}
	if status.Plan.BlobMaxBytes != blobMaxBytes {
		return fmt.Errorf("blob_max_bytes = %d, want %d", status.Plan.BlobMaxBytes, blobMaxBytes)
	}
	if status.Usage.BytesUsed != 0 {
		return fmt.Errorf("bytes_used = %d, want 0", status.Usage.BytesUsed)
	}
	return nil
}

// claimVault checks the deliberate hole in the Entitlement gate: a lapsed
// account still has to find its Vault in order to pull from it, so claiming
// a collection is open while writing into one is not.
func (s *session) claimVault() error {
	response, body, err := s.do(http.MethodPost, "/api/collections", nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("claiming a collection while unsubscribed = %d %q, want 201", response.StatusCode, body)
	}
	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	if err := json.Unmarshal(body, &claimed); err != nil {
		return err
	}
	s.collectionID = claimed.Collection.ID
	return nil
}

func (s *session) checkWriteRefused() error {
	response, body, err := s.writeObject()
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusPaymentRequired {
		return fmt.Errorf("writing while unsubscribed = %d %q, want 402", response.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("subscription_required")) {
		return fmt.Errorf("refusal body = %q, want the subscription_required code", body)
	}
	return nil
}

func (s *session) startCheckout() error {
	var checkout struct {
		URL string `json:"url"`
	}
	if err := s.postJSON("/api/billing/checkout", &checkout); err != nil {
		return err
	}
	if checkout.URL == "" {
		return fmt.Errorf("the checkout returned no URL for the app to open")
	}

	request, err := s.polar.lastRequest("/v1/checkouts/")
	if err != nil {
		return err
	}
	products, _ := request["products"].([]any)
	if len(products) != 1 || products[0] != productID {
		return fmt.Errorf("checkout products = %v, want [%s]", request["products"], productID)
	}
	// The launch flip is dropping POLAR_DISCOUNT_ID. While it is set, every
	// checkout has to carry it, or FUTO people are charged during the MVP.
	if request["discount_id"] != discountID {
		return fmt.Errorf("checkout discount_id = %v, want %s", request["discount_id"], discountID)
	}
	wantSuccess := s.server.origin + "/billing/done"
	if request["success_url"] != wantSuccess {
		return fmt.Errorf("checkout success_url = %v, want %s", request["success_url"], wantSuccess)
	}
	if request["customer_id"] == "" || request["customer_id"] == nil {
		return fmt.Errorf("checkout carried no customer_id, so the webhook could not name the account back")
	}
	return nil
}

func (s *session) checkDonePage() error {
	// Polar returns a browser that holds no app session, so this page has to
	// be reachable with no credentials at all.
	response, err := http.Get(s.server.origin + "/billing/done")
	if err != nil {
		return err
	}
	body, err := readAll(response)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the success page = %d, want 200", response.StatusCode)
	}
	if !bytes.Contains(body, []byte("FUTO Notes")) {
		return fmt.Errorf("the success page does not mention FUTO Notes: %q", truncate(body))
	}
	return nil
}

// applySubscription delivers a signed subscription.created the way Polar
// would, and checks the server applied it.
func (s *session) applySubscription() error {
	return s.deliverSubscription("subscription.created", "active")
}

func (s *session) checkWriteAccepted() error {
	response, body, err := s.writeObject()
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("writing while entitled = %d %q, want 201", response.StatusCode, body)
	}
	return nil
}

func (s *session) checkBillingAfterPaying() error {
	status, err := s.billing()
	if err != nil {
		return err
	}
	if !status.Entitled {
		return fmt.Errorf("entitled = false after an active subscription")
	}
	if status.State != "active" {
		return fmt.Errorf("state = %q, want active", status.State)
	}
	if status.Usage.BytesUsed != int64(len(ciphertext)) {
		return fmt.Errorf("bytes_used = %d, want %d", status.Usage.BytesUsed, len(ciphertext))
	}
	return nil
}

func (s *session) openPortal() error {
	var portal struct {
		URL string `json:"url"`
	}
	if err := s.getJSON("/api/billing/portal", &portal); err != nil {
		return err
	}
	if portal.URL == "" {
		return fmt.Errorf("the portal returned no URL for the app to open")
	}
	// Polar refuses a second customer for an external id it has seen, so the
	// account has to be registered exactly once across checkout and portal.
	if calls := s.polar.callCount("/v1/customers/"); calls != 1 {
		return fmt.Errorf("the server registered %d Polar customers for one account, want 1", calls)
	}
	return nil
}

func (s *session) checkoutWhileEntitled() error {
	var answer struct {
		URL      string `json:"url"`
		Entitled bool   `json:"entitled"`
		State    string `json:"state"`
	}
	if err := s.postJSON("/api/billing/checkout", &answer); err != nil {
		return err
	}
	if answer.URL != "" {
		return fmt.Errorf("an entitled account was handed a second checkout URL")
	}
	if !answer.Entitled || answer.State != "active" {
		return fmt.Errorf("checkout answered %+v, want the billing status", answer)
	}
	return nil
}

// checkQuota shrinks the Plan to just under what is already stored and
// checks the next upload is refused with quota_exceeded rather than the 413
// an oversized blob gets. The Plan is edited in the database because there is
// no route that sets it and the deployed instance has one plan.
func (s *session) checkQuota() error {
	if err := s.setStorageQuota(int64(len(ciphertext))); err != nil {
		return err
	}
	defer func() { _ = s.setStorageQuota(storageQuotaBytes) }()

	response, body, err := s.do(http.MethodPost, "/api/blobs", []byte("one byte too many"))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusInsufficientStorage {
		return fmt.Errorf("an upload past the Storage Quota = %d %q, want 507", response.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("quota_exceeded")) {
		return fmt.Errorf("refusal body = %q, want the quota_exceeded code", body)
	}
	return nil
}

// revokeSubscription is the lapse: Polar reports the subscription revoked,
// and the account stops writing without losing anything it already stored.
func (s *session) revokeSubscription() error {
	if err := s.deliverSubscription("subscription.revoked", "canceled"); err != nil {
		return err
	}
	response, body, err := s.writeObject()
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusPaymentRequired {
		return fmt.Errorf("writing after a revoked subscription = %d %q, want 402", response.StatusCode, body)
	}

	// Reads survive the lapse. A lapsed card must never strand a device.
	read, readBody, err := s.do(http.MethodGet, "/api/collections/"+s.collectionID+"/objects", nil)
	if err != nil {
		return err
	}
	if read.StatusCode != http.StatusOK {
		return fmt.Errorf("listing objects after a lapse = %d %q, want 200", read.StatusCode, readBody)
	}
	return nil
}

// --- helpers ---

func (s *session) mintHandoff() (ticket, loginURL string, err error) {
	var minted struct {
		Ticket string `json:"ticket"`
		URL    string `json:"url"`
	}
	response, body, err := s.do(http.MethodPost, "/api/auth/handoff", nil)
	if err != nil {
		return "", "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("minting a hand-off = %d %q, want 200", response.StatusCode, body)
	}
	if err := json.Unmarshal(body, &minted); err != nil {
		return "", "", err
	}
	if minted.Ticket == "" {
		return "", "", fmt.Errorf("the mint returned no ticket")
	}
	return minted.Ticket, minted.URL, nil
}

func (s *session) redeem(ticket string) (int, []byte, error) {
	response, body, err := s.do(http.MethodGet, "/api/auth/handoff/"+ticket, nil)
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, body, nil
}

// openInBrowser follows the login URL the way a browser would: to the
// provider, back to the callback, and onto the "go back to the app" page.
func (s *session) openInBrowser(loginURL string) error {
	response, err := s.client.Get(loginURL)
	if err != nil {
		return err
	}
	body, err := readAll(response)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("completing the login in a browser = %d %q, want 200", response.StatusCode, truncate(body))
	}
	if !bytes.Contains(body, []byte("FUTO Notes")) {
		return fmt.Errorf("the browser did not land on the go-back-to-the-app page: %q", truncate(body))
	}
	return nil
}

type billingStatus struct {
	Entitled bool   `json:"entitled"`
	State    string `json:"state"`
	Plan     struct {
		StorageQuotaBytes int64 `json:"storage_quota_bytes"`
		BlobMaxBytes      int64 `json:"blob_max_bytes"`
	} `json:"plan"`
	Usage struct {
		BytesUsed int64 `json:"bytes_used"`
	} `json:"usage"`
}

func (s *session) billing() (billingStatus, error) {
	var status billingStatus
	err := s.getJSON("/api/billing", &status)
	return status, err
}

// writeObject is the gated write the rehearsal runs on both sides of the
// Entitlement gate: one inline blob object, which stages ciphertext and
// claims it in a single call.
func (s *session) writeObject() (*http.Response, []byte, error) {
	return s.do(http.MethodPost, "/api/collections/"+s.collectionID+"/blob-objects", []byte(ciphertext))
}

// deliverSubscription signs a Polar-shaped delivery with the configured
// webhook secret and puts it through the real webhook route, which is the
// only way the subscription record ever changes.
func (s *session) deliverSubscription(eventType, status string) error {
	now := time.Now()
	periodEnd := now.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	delivery, err := json.Marshal(map[string]any{
		"type": eventType,
		"data": map[string]any{
			"id":                 "polar_sub_rehearsal",
			"status":             status,
			"customer_id":        "polar_cus_1",
			"current_period_end": periodEnd,
			"customer":           map[string]any{"external_id": personSub},
		},
	})
	if err != nil {
		return err
	}
	header, err := polar.Sign(webhookSecret, "rehearsal-"+eventType, now, delivery)
	if err != nil {
		return err
	}

	request, err := http.NewRequest(http.MethodPost, s.server.origin+"/api/billing/polar/webhook", bytes.NewReader(delivery))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		request.Header[name] = values
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	body, err := readAll(response)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the webhook answered %d %q, want 200", response.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"applied"`)) {
		return fmt.Errorf("the webhook answered %q, want applied: the delivery did not reach an account", body)
	}
	return nil
}

// setStorageQuota edits the one Plan row directly. The rehearsal owns the
// scratch database, and no route sets a Plan.
func (s *session) setStorageQuota(limit int64) error {
	database, err := sql.Open("pgx", s.databaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	_, err = database.Exec("UPDATE plans SET storage_quota_bytes = $1 WHERE id = 'default'", limit)
	return err
}

func (s *session) do(method, path string, body []byte) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, s.server.origin+path, reader)
	if err != nil {
		return nil, nil, err
	}
	if s.token != "" {
		request.Header.Set("Authorization", "Bearer "+s.token)
	}
	// The app authenticates with the bearer token the hand-off produced, so
	// this deliberately does not use the browser's cookie jar.
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, nil, err
	}
	responseBody, err := readAll(response)
	return response, responseBody, err
}

func (s *session) getJSON(path string, into any) error {
	response, body, err := s.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s = %d %q, want 200", path, response.StatusCode, truncate(body))
	}
	return json.Unmarshal(body, into)
}

func (s *session) postJSON(path string, into any) error {
	response, body, err := s.do(http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s = %d %q, want 200", path, response.StatusCode, truncate(body))
	}
	return json.Unmarshal(body, into)
}

func readAll(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	return io.ReadAll(io.LimitReader(response.Body, 1<<20))
}

func truncate(body []byte) string {
	const limit = 300
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "…"
}
