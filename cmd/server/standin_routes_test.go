package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
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

// The two routes stand-in mode adds. A default binary must not serve either:
// one flips an account's Entitlement and the other rewrites the Plan, so
// "undocumented" is not good enough.
var standinRoutes = []string{"/standin/lapse", "/standin/quota"}

func TestStandinRoutesAreAbsentWithoutTheFlag(t *testing.T) {
	for _, mode := range []string{"dev", "password", "oidc"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Config{
				DatabaseURL:  "sqlite:" + filepath.Join(root, "notes.db"),
				AuthMode:     mode,
				Password:     "correct password",
				CookieSecure: false,
				// On, because a server that also runs the dev UI must still
				// not serve these.
				DevUI:              true,
				BlobDir:            filepath.Join(root, "blobs"),
				OIDCIssuer:         "https://idp.example.test",
				OIDCClientID:       "client-id",
				OIDCClientSecret:   "client-secret",
				OIDCRedirectURI:    "http://app.test" + config.OIDCCallbackPath,
				PolarBaseURL:       "https://sandbox-api.polar.sh",
				PolarAccessToken:   "polar_oat_test",
				PolarProductID:     "prod_test",
				PolarWebhookSecret: "whsec_cG9sYXItd2ViaG9vay1rZXk=",
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
				events.NewPublisher(database.Dialect(), hub), nil))
			defer server.Close()
			client := serverClient(server)

			for _, path := range standinRoutes {
				response := doRequest(t, client, http.MethodPost, server.URL+path, "{}", "", nil)
				if response.StatusCode != http.StatusNotFound {
					t.Fatalf("POST %s in %s mode = %d %q, want 404: the route must not exist",
						path, mode, response.StatusCode, response.Body)
				}
			}
		})
	}
}

// newStandinServer boots the whole stand-in mode the way main does: the
// server's own origin is decided first, the stand-ins are started against it,
// and the config they fill in is what routes are built from.
func newStandinServer(t *testing.T) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + listener.Addr().String()

	root := t.TempDir()
	cfg := config.Config{
		DatabaseURL: "sqlite:" + filepath.Join(root, "notes.db"),
		AuthMode:    "oidc",
		StandinMode: true,
		BlobDir:     filepath.Join(root, "blobs"),
	}
	cfg, stopStandins, err := startStandins(cfg, origin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopStandins)

	database, err := db.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	provider, err := oidc.Discover(context.Background(), cfg.OIDCIssuer)
	if err != nil {
		t.Fatalf("discovering the stand-in issuer: %v", err)
	}
	hub := events.NewHub()
	server := httptest.NewUnstartedServer(routes(cfg, database, &blobs.FileStore{Dir: cfg.BlobDir}, hub,
		events.NewPublisher(database.Dialect(), hub), provider))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return server
}

// TestStandinModeDrivesTheWholeHostedPath is the client suite's path, with a
// plain HTTP client and no browser: log in, be refused a write, check out,
// become entitled, write, fill the quota, lapse.
func TestStandinModeDrivesTheWholeHostedPath(t *testing.T) {
	server := newStandinServer(t)

	// The app's client, which follows nothing and holds no cookies.
	app := serverClient(server)
	browser := newStandinBrowser()

	// 1. Log in with no browser at all: fetching the hand-off's URL is the
	// whole login, because the stand-in issuer auto-approves.
	token := logInThroughStandins(t, server, app, browser)

	// 2. Claim the vault, and find a write refused for want of an
	// Entitlement.
	claimed := doRequest(t, app, http.MethodPost, server.URL+"/api/collections", "", token, nil)
	if claimed.StatusCode != http.StatusCreated {
		t.Fatalf("claiming the vault = %d %q", claimed.StatusCode, claimed.Body)
	}
	var vault struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	decodeJSON(t, claimed.Body, &vault)
	write := server.URL + "/api/collections/" + vault.Collection.ID + "/blob-objects"

	refused := doRequest(t, app, http.MethodPost, write, "ciphertext", token, nil)
	if refused.StatusCode != http.StatusPaymentRequired || !strings.Contains(refused.Body, "subscription_required") {
		t.Fatalf("writing unsubscribed = %d %q, want 402 subscription_required", refused.StatusCode, refused.Body)
	}

	// 3. Fetch the checkout URL. That completes the checkout and fires the
	// webhook, so the account is entitled by the time the browser lands.
	started := doRequest(t, app, http.MethodPost, server.URL+"/api/billing/checkout", "", token, nil)
	if started.StatusCode != http.StatusOK {
		t.Fatalf("opening a checkout = %d %q", started.StatusCode, started.Body)
	}
	var checkout struct {
		URL string `json:"url"`
	}
	decodeJSON(t, started.Body, &checkout)

	paid := readResponse(t, browser, newRequest(t, http.MethodGet, checkout.URL, ""))
	if paid.StatusCode != http.StatusOK || !strings.Contains(paid.Body, "FUTO Notes") {
		t.Fatalf("fetching the checkout URL = %d %q, want the success page", paid.StatusCode, paid.Body)
	}

	status := doRequest(t, app, http.MethodGet, server.URL+"/api/billing", "", token, nil)
	var billing struct {
		Entitled bool   `json:"entitled"`
		State    string `json:"state"`
	}
	decodeJSON(t, status.Body, &billing)
	if !billing.Entitled || billing.State != "active" {
		t.Fatalf("billing after checking out = %+v, want entitled and active", billing)
	}

	accepted := doRequest(t, app, http.MethodPost, write, "ciphertext", token, nil)
	if accepted.StatusCode != http.StatusCreated {
		t.Fatalf("writing while entitled = %d %q", accepted.StatusCode, accepted.Body)
	}

	// 4. Fill the quota: the next upload is refused with 507 rather than the
	// 413 an oversized blob gets.
	filled := doRequest(t, app, http.MethodPost, server.URL+"/standin/quota", "", token, nil)
	if filled.StatusCode != http.StatusOK {
		t.Fatalf("filling the quota = %d %q", filled.StatusCode, filled.Body)
	}
	full := doRequest(t, app, http.MethodPost, server.URL+"/api/blobs", "one byte too many", token, nil)
	if full.StatusCode != http.StatusInsufficientStorage || !strings.Contains(full.Body, "quota_exceeded") {
		t.Fatalf("uploading past the quota = %d %q, want 507 quota_exceeded", full.StatusCode, full.Body)
	}
	// And it can be put back, so a suite is not stuck at the ceiling.
	restored := doRequest(t, app, http.MethodPost, server.URL+"/standin/quota",
		`{"storage_quota_bytes":10000000000}`, token, nil)
	if restored.StatusCode != http.StatusOK {
		t.Fatalf("restoring the quota = %d %q", restored.StatusCode, restored.Body)
	}

	// 5. Lapse: writes stop again, reads do not.
	lapsed := doRequest(t, app, http.MethodPost, server.URL+"/standin/lapse", "", token, nil)
	if lapsed.StatusCode != http.StatusOK {
		t.Fatalf("lapsing the account = %d %q", lapsed.StatusCode, lapsed.Body)
	}
	stopped := doRequest(t, app, http.MethodPost, write, "ciphertext", token, nil)
	if stopped.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("writing after a lapse = %d %q, want 402", stopped.StatusCode, stopped.Body)
	}
	read := doRequest(t, app, http.MethodGet,
		server.URL+"/api/collections/"+vault.Collection.ID+"/objects", "", token, nil)
	if read.StatusCode != http.StatusOK {
		t.Fatalf("reading after a lapse = %d %q, want 200", read.StatusCode, read.Body)
	}
}

func TestStandinAccountControlsNeedASession(t *testing.T) {
	server := newStandinServer(t)
	client := serverClient(server)

	for _, path := range standinRoutes {
		response := doRequest(t, client, http.MethodPost, server.URL+path, "{}", "", nil)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("POST %s without a session = %d %q, want 401", path, response.StatusCode, response.Body)
		}
	}
}

// TestStandinCheckoutAgainAfterALapse covers the suite that exercises the 402
// path and then carries on: a second checkout has to entitle the account
// again, although by then the account's Polar customer is already registered
// and the server no longer asks for one.
func TestStandinCheckoutAgainAfterALapse(t *testing.T) {
	server := newStandinServer(t)
	app := serverClient(server)
	browser := newStandinBrowser()
	token := logInThroughStandins(t, server, app, browser)

	payThroughStandinCheckout(t, server, app, browser, token)
	lapsed := doRequest(t, app, http.MethodPost, server.URL+"/standin/lapse", "", token, nil)
	if lapsed.StatusCode != http.StatusOK {
		t.Fatalf("lapsing the account = %d %q", lapsed.StatusCode, lapsed.Body)
	}
	payThroughStandinCheckout(t, server, app, browser, token)

	status := doRequest(t, app, http.MethodGet, server.URL+"/api/billing", "", token, nil)
	if !strings.Contains(status.Body, `"entitled":true`) {
		t.Fatalf("billing after checking out a second time = %q", status.Body)
	}
}

// payThroughStandinCheckout opens a checkout and fetches its URL, which is
// what stands in for a person paying.
func payThroughStandinCheckout(t *testing.T, server *httptest.Server, app, browser *http.Client, token string) {
	t.Helper()
	started := doRequest(t, app, http.MethodPost, server.URL+"/api/billing/checkout", "", token, nil)
	if started.StatusCode != http.StatusOK {
		t.Fatalf("opening a checkout = %d %q", started.StatusCode, started.Body)
	}
	var checkout struct {
		URL string `json:"url"`
	}
	decodeJSON(t, started.Body, &checkout)
	if checkout.URL == "" {
		t.Fatalf("the checkout answered %q, with no URL to open", started.Body)
	}
	paid := readResponse(t, browser, newRequest(t, http.MethodGet, checkout.URL, ""))
	if paid.StatusCode != http.StatusOK || !strings.Contains(paid.Body, "FUTO Notes") {
		t.Fatalf("fetching the checkout URL = %d %q, want the success page", paid.StatusCode, paid.Body)
	}
}

// newStandinBrowser is the browser half of a stand-in login: it follows
// redirects and keeps the OIDC flow cookie across them. Its own client rather
// than the test server's, which the app stops following redirects with.
func newStandinBrowser() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return &http.Client{Jar: jar}
}

// logInThroughStandins mints a Login Hand-off, completes it by fetching its
// URL, and returns the bearer token the app authenticates with.
func logInThroughStandins(t *testing.T, server *httptest.Server, app, browser *http.Client) string {
	t.Helper()
	minted := doRequest(t, app, http.MethodPost, server.URL+"/api/auth/handoff", "", "", nil)
	if minted.StatusCode != http.StatusOK {
		t.Fatalf("minting a hand-off = %d %q", minted.StatusCode, minted.Body)
	}
	var handoff struct {
		Ticket string `json:"ticket"`
		URL    string `json:"url"`
	}
	decodeJSON(t, minted.Body, &handoff)

	loggedIn := readResponse(t, browser, newRequest(t, http.MethodGet, handoff.URL, ""))
	if loggedIn.StatusCode != http.StatusOK || !strings.Contains(loggedIn.Body, "FUTO Notes") {
		t.Fatalf("fetching the login URL = %d %q, want the go-back-to-the-app page",
			loggedIn.StatusCode, loggedIn.Body)
	}

	redeemed := doRequest(t, app, http.MethodGet, server.URL+"/api/auth/handoff/"+handoff.Ticket, "", "", nil)
	if redeemed.StatusCode != http.StatusOK {
		t.Fatalf("redeeming the ticket = %d %q", redeemed.StatusCode, redeemed.Body)
	}
	var login struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
		Token string `json:"token"`
	}
	decodeJSON(t, redeemed.Body, &login)
	if login.User.Email != standinEmail {
		t.Fatalf("signed in as %q, want %q", login.User.Email, standinEmail)
	}
	if login.Token == "" {
		t.Fatal("the hand-off returned no token")
	}
	return login.Token
}

func newRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func decodeJSON(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
}
