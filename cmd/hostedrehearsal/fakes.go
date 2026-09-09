package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"

	"futo-notes-server/internal/oidc"
)

// serve starts an HTTP server on a free loopback port and returns its origin
// plus a shutdown function. The rehearsal's two upstreams are ordinary
// servers rather than httptest ones, because this is a command rather than a
// test binary.
func serve(handler http.Handler) (origin string, stop func(), err error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String(), func() { _ = server.Close() }, nil
}

// fakeIssuer stands in for Zitadel. It serves the three endpoints the server
// reads from discovery and completes the Authorization Code + PKCE flow
// without a human at a login page: /authorize redirects straight back to the
// server's callback with a code.
//
// It is not a rubber stamp. The token endpoint checks client_secret_basic
// against the configured credentials and recomputes the S256 challenge from
// the verifier the server presents, so a rehearsal that passes has exercised
// the real PKCE round trip rather than only the redirect shape.
type fakeIssuer struct {
	origin       string
	clientID     string
	clientSecret string

	mu sync.Mutex
	// claims is what /userinfo answers with. The rehearsal edits it between
	// logins to sign in as a different person, which is how the
	// email-domain allowlist gets exercised.
	claims oidc.Claims
	// challenges maps an issued authorization code to the PKCE challenge
	// that was presented with it.
	challenges map[string]string
}

func startFakeIssuer(clientID, clientSecret string) (*fakeIssuer, func(), error) {
	issuer := &fakeIssuer{
		clientID:     clientID,
		clientSecret: clientSecret,
		challenges:   map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", issuer.discovery)
	mux.HandleFunc("GET /authorize", issuer.authorize)
	mux.HandleFunc("POST /token", issuer.token)
	mux.HandleFunc("GET /userinfo", issuer.userinfo)

	origin, stop, err := serve(mux)
	if err != nil {
		return nil, nil, err
	}
	issuer.origin = origin
	return issuer, stop, nil
}

// signInAs sets the identity the next completed login will produce.
func (f *fakeIssuer) signInAs(sub, email, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = oidc.Claims{Sub: sub, Email: email, Name: name}
}

func (f *fakeIssuer) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"issuer":                 f.origin,
		"authorization_endpoint": f.origin + "/authorize",
		"token_endpoint":         f.origin + "/token",
		"userinfo_endpoint":      f.origin + "/userinfo",
	})
}

func (f *fakeIssuer) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("client_id") != f.clientID {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		http.Error(w, "expected an S256 PKCE challenge", http.StatusBadRequest)
		return
	}
	redirectURI, err := url.Parse(query.Get("redirect_uri"))
	if err != nil || !redirectURI.IsAbs() {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}

	code := randomToken()
	f.mu.Lock()
	f.challenges[code] = query.Get("code_challenge")
	f.mu.Unlock()

	redirectURI.RawQuery = url.Values{
		"code":  {code},
		"state": {query.Get("state")},
	}.Encode()
	http.Redirect(w, r, redirectURI.String(), http.StatusFound)
}

func (f *fakeIssuer) token(w http.ResponseWriter, r *http.Request) {
	user, password, ok := r.BasicAuth()
	if !ok || user != f.clientID || password != f.clientSecret {
		http.Error(w, "bad client credentials", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "unreadable form", http.StatusBadRequest)
		return
	}
	code, verifier := r.PostForm.Get("code"), r.PostForm.Get("code_verifier")

	f.mu.Lock()
	challenge, known := f.challenges[code]
	delete(f.challenges, code)
	f.mu.Unlock()
	if !known {
		http.Error(w, "unknown or already-redeemed code", http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		http.Error(w, "code_verifier does not match the challenge", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": "access-" + code})
}

func (f *fakeIssuer) userinfo(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	claims := f.claims
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, claims)
}

// fakePolar stands in for Polar's API. It answers the three calls the server
// makes and keeps every request body, so the rehearsal can assert what the
// server actually sent — that a checkout carried POLAR_DISCOUNT_ID, and that
// its success URL points at this server's own "go back to the app" page.
//
// It refuses a second customer for an external id it has already seen, the
// way Polar does, so the lazy-registration path is exercised honestly.
type fakePolar struct {
	origin      string
	accessToken string

	mu       sync.Mutex
	requests map[string][]map[string]any
	// customers maps an external id (the account's OIDC sub) to the Polar
	// customer id handed back for it.
	customers map[string]string
}

func startFakePolar(accessToken string) (*fakePolar, func(), error) {
	polar := &fakePolar{
		accessToken: accessToken,
		requests:    map[string][]map[string]any{},
		customers:   map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/customers/", polar.createCustomer)
	mux.HandleFunc("POST /v1/checkouts/", polar.createCheckout)
	mux.HandleFunc("POST /v1/customer-sessions/", polar.createCustomerSession)

	origin, stop, err := serve(mux)
	if err != nil {
		return nil, nil, err
	}
	polar.origin = origin
	return polar, stop, nil
}

// read returns the request body decoded, after checking the access token and
// recording the call. A false second return means it has answered already.
func (f *fakePolar) read(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if r.Header.Get("Authorization") != "Bearer "+f.accessToken {
		http.Error(w, `{"detail":"invalid token"}`, http.StatusUnauthorized)
		return nil, false
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"detail":"invalid json"}`, http.StatusBadRequest)
		return nil, false
	}
	f.mu.Lock()
	f.requests[r.URL.Path] = append(f.requests[r.URL.Path], body)
	f.mu.Unlock()
	return body, true
}

// lastRequest returns the most recent body sent to path.
func (f *fakePolar) lastRequest(path string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.requests[path]
	if len(calls) == 0 {
		return nil, fmt.Errorf("the server never called Polar %s", path)
	}
	return calls[len(calls)-1], nil
}

// callCount reports how many times path was called, which is how the
// rehearsal checks that a customer is registered at most once per account.
func (f *fakePolar) callCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests[path])
}

func (f *fakePolar) createCustomer(w http.ResponseWriter, r *http.Request) {
	body, ok := f.read(w, r)
	if !ok {
		return
	}
	externalID, _ := body["external_id"].(string)

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.customers[externalID]; taken {
		// Polar's own refusal: an external id belongs to one customer.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"detail": []map[string]string{{"msg": "external_id is already used"}},
		})
		return
	}
	customerID := fmt.Sprintf("polar_cus_%d", len(f.customers)+1)
	f.customers[externalID] = customerID
	writeJSON(w, http.StatusCreated, map[string]string{"id": customerID})
}

func (f *fakePolar) createCheckout(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.read(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"url": f.origin + "/checkout/" + randomToken(),
	})
}

func (f *fakePolar) createCustomerSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.read(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"customer_portal_url": f.origin + "/portal/" + randomToken(),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
