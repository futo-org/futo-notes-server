package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/oidc"
)

// newFakeIssuer starts an httptest server that serves a minimal OIDC
// discovery document, token endpoint, and userinfo endpoint. The returned
// claims are what /userinfo answers with and a test may edit them between
// requests, to simulate a different identity or an upstream name change.
func newFakeIssuer(t *testing.T, clientID, clientSecret string) (issuerURL string, claims *oidc.Claims) {
	t.Helper()
	claims = &oidc.Claims{
		Sub:   "fake-issuer-sub-1",
		Email: "person@example.test",
		Name:  "First Login",
	}
	var issuer string

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/authorize",
			"token_endpoint":         issuer + "/token",
			"userinfo_endpoint":      issuer + "/userinfo",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != clientID || pass != clientSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("code") == "" || r.PostForm.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access-" + r.PostForm.Get("code")})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(claims)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	issuer = server.URL
	return issuer, claims
}

func TestOIDCRoutesEndToEnd(t *testing.T) {
	const clientID, clientSecret = "test-client", "test-secret"
	issuerURL, claims := newFakeIssuer(t, clientID, clientSecret)

	provider, err := oidc.Discover(context.Background(), issuerURL)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	root := t.TempDir()
	cfg := config.Config{
		DatabaseURL:      "sqlite:" + filepath.Join(root, "notes.db"),
		AuthMode:         "oidc",
		CookieSecure:     false,
		DevUI:            true,
		BlobDir:          filepath.Join(root, "blobs"),
		OIDCIssuer:       issuerURL,
		OIDCClientID:     clientID,
		OIDCClientSecret: clientSecret,
		OIDCRedirectURI:  "http://app.test/api/auth/oidc/callback",
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
	server := httptest.NewServer(routes(cfg, database, &blobs.FileStore{Dir: cfg.BlobDir}, hub,
		events.NewPublisher(database.Dialect(), hub), provider))
	defer server.Close()
	client := serverClient(server)

	// start: 302 to the fake authorize endpoint with state + S256 PKCE params,
	// and an oidc_flow cookie.
	start := doRequest(t, client, http.MethodGet, server.URL+"/api/auth/oidc/start", "", "", nil)
	if start.StatusCode != http.StatusFound {
		t.Fatalf("start = %d %q", start.StatusCode, start.Body)
	}
	authorizeURL, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := authorizeURL.Scheme + "://" + authorizeURL.Host + authorizeURL.Path; got != issuerURL+"/authorize" {
		t.Fatalf("redirect target = %q, want %q/authorize", got, issuerURL)
	}
	q := authorizeURL.Query()
	state := q.Get("state")
	if state == "" {
		t.Fatal("authorize URL has no state")
	}
	if q.Get("code_challenge") == "" {
		t.Fatal("authorize URL has no code_challenge")
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	var flowCookie *http.Cookie
	for _, c := range start.Cookies {
		if c.Name == oidcFlowCookie {
			flowCookie = c
		}
	}
	if flowCookie == nil {
		t.Fatal("start response did not set the oidc_flow cookie")
	}

	// callback: exchanges the fake's code, sets the session cookie, clears
	// the flow cookie, and redirects to /dev because DevUI is true.
	callback := doRequest(t, client, http.MethodGet,
		server.URL+"/api/auth/oidc/callback?code=fake-code&state="+state, "", "", flowCookie)
	if callback.StatusCode != http.StatusFound || callback.Header.Get("Location") != "/dev" {
		t.Fatalf("callback = %d, location %q, body %q", callback.StatusCode, callback.Header.Get("Location"), callback.Body)
	}
	var sessionCookie, clearedFlowCookie *http.Cookie
	for _, c := range callback.Cookies {
		switch c.Name {
		case "session":
			sessionCookie = c
		case oidcFlowCookie:
			clearedFlowCookie = c
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatal("callback did not set a session cookie")
	}
	if clearedFlowCookie == nil || clearedFlowCookie.MaxAge >= 0 {
		t.Fatalf("callback did not clear the oidc_flow cookie: %+v", clearedFlowCookie)
	}

	me := doRequest(t, client, http.MethodGet, server.URL+"/api/auth", "", "", sessionCookie)
	if me.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/auth = %d %q", me.StatusCode, me.Body)
	}
	var meBody struct {
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(me.Body), &meBody); err != nil {
		t.Fatal(err)
	}
	if meBody.User.Email != "person@example.test" {
		t.Fatalf("user email = %q", meBody.User.Email)
	}
	if meBody.User.Name != "First Login" {
		t.Fatalf("user name = %q", meBody.User.Name)
	}
	firstUserID := meBody.User.ID

	// A second login with the same sub but a new upstream name updates the
	// row rather than creating a second user.
	claims.Name = "Second Login"
	start2 := doRequest(t, client, http.MethodGet, server.URL+"/api/auth/oidc/start", "", "", nil)
	authorizeURL2, err := url.Parse(start2.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state2 := authorizeURL2.Query().Get("state")
	var flowCookie2 *http.Cookie
	for _, c := range start2.Cookies {
		if c.Name == oidcFlowCookie {
			flowCookie2 = c
		}
	}
	callback2 := doRequest(t, client, http.MethodGet,
		server.URL+"/api/auth/oidc/callback?code=fake-code-2&state="+state2, "", "", flowCookie2)
	if callback2.StatusCode != http.StatusFound {
		t.Fatalf("second callback = %d %q", callback2.StatusCode, callback2.Body)
	}
	var sessionCookie2 *http.Cookie
	for _, c := range callback2.Cookies {
		if c.Name == "session" {
			sessionCookie2 = c
		}
	}
	me2 := doRequest(t, client, http.MethodGet, server.URL+"/api/auth", "", "", sessionCookie2)
	var meBody2 struct {
		User struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(me2.Body), &meBody2); err != nil {
		t.Fatal(err)
	}
	if meBody2.User.ID != firstUserID {
		t.Fatalf("second login user id = %q, want %q (same user)", meBody2.User.ID, firstUserID)
	}
	if meBody2.User.Name != "Second Login" {
		t.Fatalf("second login user name = %q, want updated name", meBody2.User.Name)
	}

	// callback with the wrong state is rejected.
	start3 := doRequest(t, client, http.MethodGet, server.URL+"/api/auth/oidc/start", "", "", nil)
	var flowCookie3 *http.Cookie
	for _, c := range start3.Cookies {
		if c.Name == oidcFlowCookie {
			flowCookie3 = c
		}
	}
	wrongState := doRequest(t, client, http.MethodGet,
		server.URL+"/api/auth/oidc/callback?code=fake-code-3&state=not-the-real-state", "", "", flowCookie3)
	if wrongState.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong-state callback = %d %q, want 400", wrongState.StatusCode, wrongState.Body)
	}

	// callback without the flow cookie is rejected.
	noCookie := doRequest(t, client, http.MethodGet,
		server.URL+"/api/auth/oidc/callback?code=fake-code-4&state=whatever", "", "", nil)
	if noCookie.StatusCode != http.StatusBadRequest {
		t.Fatalf("no-cookie callback = %d %q, want 400", noCookie.StatusCode, noCookie.Body)
	}
}
