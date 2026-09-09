package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/oidc"
)

const (
	handoffClientID     = "handoff-client"
	handoffClientSecret = "handoff-secret"
	handoffRedirectURI  = "http://app.test/api/auth/oidc/callback"
)

// handoffHarness is an oidc-mode server on SQLite in front of a fake issuer.
// DevUI is off, so a callback without a ticket answers with login JSON and any
// hand-off page in a response is the ticketed path's own work.
type handoffHarness struct {
	url      string
	client   *http.Client
	claims   *oidc.Claims
	database *db.DB
}

func newHandoffHarness(t *testing.T, allowedEmailDomains ...string) handoffHarness {
	t.Helper()
	issuerURL, claims := newFakeIssuer(t, handoffClientID, handoffClientSecret)
	provider, err := oidc.Discover(context.Background(), issuerURL)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	root := t.TempDir()
	cfg := config.Config{
		DatabaseURL:               "sqlite:" + filepath.Join(root, "notes.db"),
		AuthMode:                  "oidc",
		CookieSecure:              false,
		BlobDir:                   filepath.Join(root, "blobs"),
		OIDCIssuer:                issuerURL,
		OIDCClientID:              handoffClientID,
		OIDCClientSecret:          handoffClientSecret,
		OIDCRedirectURI:           handoffRedirectURI,
		HostedAllowedEmailDomains: allowedEmailDomains,
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

	return handoffHarness{url: server.URL, client: serverClient(server), claims: claims, database: database}
}

type mintedHandoff struct {
	Ticket string `json:"ticket"`
	URL    string `json:"url"`
}

func (h handoffHarness) mint(t *testing.T) mintedHandoff {
	t.Helper()
	response := doRequest(t, h.client, http.MethodPost, h.url+"/api/auth/handoff", "", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("mint = %d %q, want 200", response.StatusCode, response.Body)
	}
	var minted mintedHandoff
	if err := json.Unmarshal([]byte(response.Body), &minted); err != nil {
		t.Fatal(err)
	}
	return minted
}

func TestLoginHandoffMintsATicketAndTheURLToOpen(t *testing.T) {
	h := newHandoffHarness(t)

	minted := h.mint(t)
	if len(minted.Ticket) < 32 {
		t.Fatalf("ticket = %q, want an unguessable value", minted.Ticket)
	}
	loginURL, err := url.Parse(minted.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := loginURL.Scheme + "://" + loginURL.Host + loginURL.Path; got != "http://app.test/api/auth/oidc/start" {
		t.Fatalf("login URL = %q, want the OIDC start route on the server's public origin", minted.URL)
	}
	if loginURL.Query().Get("ticket") != minted.Ticket {
		t.Fatalf("login URL %q does not carry ticket %q", minted.URL, minted.Ticket)
	}

	if second := h.mint(t); second.Ticket == minted.Ticket {
		t.Fatalf("two mints returned the same ticket %q", second.Ticket)
	}
}

// login drives a browser through the fake issuer: the OIDC start route
// carrying ticket, then the callback with the state and flow cookie start
// handed out. An empty ticket starts an ordinary browser login.
func (h handoffHarness) login(t *testing.T, ticket, code string) routeTestResponse {
	t.Helper()
	startURL := h.url + "/api/auth/oidc/start"
	if ticket != "" {
		startURL += "?" + url.Values{handoffTicketParam: {ticket}}.Encode()
	}
	start := doRequest(t, h.client, http.MethodGet, startURL, "", "", nil)
	if start.StatusCode != http.StatusFound {
		t.Fatalf("oidc start = %d %q, want 302", start.StatusCode, start.Body)
	}
	authorizeURL, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	var flowCookie *http.Cookie
	for _, cookie := range start.Cookies {
		if cookie.Name == oidcFlowCookie {
			flowCookie = cookie
		}
	}
	if flowCookie == nil {
		t.Fatal("oidc start did not set the flow cookie")
	}
	return doRequest(t, h.client, http.MethodGet, h.url+"/api/auth/oidc/callback?"+
		url.Values{"code": {code}, "state": {authorizeURL.Query().Get("state")}}.Encode(), "", "", flowCookie)
}

func (h handoffHarness) poll(t *testing.T, ticket string) routeTestResponse {
	t.Helper()
	return doRequest(t, h.client, http.MethodGet, h.url+"/api/auth/handoff/"+url.PathEscape(ticket), "", "", nil)
}

type loginBody struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
	Token string `json:"token"`
}

func TestLoginHandoffHandsBackTheSessionExactlyOnce(t *testing.T) {
	h := newHandoffHarness(t)
	minted := h.mint(t)

	if pending := h.poll(t, minted.Ticket); pending.StatusCode != http.StatusAccepted || pending.Body != "" {
		t.Fatalf("poll before login = %d %q, want 202 with an empty body", pending.StatusCode, pending.Body)
	}

	callback := h.login(t, minted.Ticket, "handoff-code")
	if callback.StatusCode != http.StatusOK {
		t.Fatalf("ticketed callback = %d %q, want 200", callback.StatusCode, callback.Body)
	}
	if contentType := callback.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("ticketed callback Content-Type = %q, want an HTML page", contentType)
	}
	if !strings.Contains(callback.Body, "Go back to FUTO Notes") {
		t.Fatalf("ticketed callback body does not tell the person to go back to the app: %q", callback.Body)
	}
	var browserSession *http.Cookie
	for _, cookie := range callback.Cookies {
		if cookie.Name == "session" {
			browserSession = cookie
		}
	}
	if browserSession == nil || browserSession.Value == "" {
		t.Fatal("ticketed callback did not set the session cookie for the browser")
	}

	redeemed := h.poll(t, minted.Ticket)
	if redeemed.StatusCode != http.StatusOK {
		t.Fatalf("poll after login = %d %q, want 200", redeemed.StatusCode, redeemed.Body)
	}
	var login loginBody
	if err := json.Unmarshal([]byte(redeemed.Body), &login); err != nil {
		t.Fatal(err)
	}
	if login.User.Email != "person@example.test" || login.User.Name != "First Login" {
		t.Fatalf("redeemed user = %+v, want the identity the provider authenticated", login.User)
	}
	if login.Token == "" {
		t.Fatal("redeemed body carries no session token")
	}

	me := doRequest(t, h.client, http.MethodGet, h.url+"/api/auth", "", login.Token, nil)
	if me.StatusCode != http.StatusOK || !strings.Contains(me.Body, login.User.ID) {
		t.Fatalf("GET /api/auth with the redeemed token = %d %q, want the same user", me.StatusCode, me.Body)
	}

	if again := h.poll(t, minted.Ticket); again.StatusCode != http.StatusNotFound {
		t.Fatalf("second poll = %d %q, want 404", again.StatusCode, again.Body)
	}
	if unknown := h.poll(t, "IqvxvL9kNTQVjMhCNoyKmvJqHVCJPZTBUlrDCoLDgNo"); unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("poll for an unminted ticket = %d %q, want 404", unknown.StatusCode, unknown.Body)
	}
}

// expire backdates a ticket the way the clock would, without waiting out
// TicketTTL.
func (h handoffHarness) expire(t *testing.T, ticket string) {
	t.Helper()
	if _, err := h.database.Exec(`UPDATE login_handoffs SET expires_at = $1 WHERE ticket = $2`,
		db.Timestamp(time.Now().Add(-time.Second)), ticket); err != nil {
		t.Fatal(err)
	}
}

func TestLoginHandoffExpiresBeforeAndAfterTheLogin(t *testing.T) {
	h := newHandoffHarness(t)

	stale := h.mint(t)
	h.expire(t, stale.Ticket)
	if response := h.poll(t, stale.Ticket); response.StatusCode != http.StatusNotFound {
		t.Fatalf("poll for an expired unfulfilled ticket = %d %q, want 404", response.StatusCode, response.Body)
	}

	// The browser still finishes its login and lands on the page; the ticket
	// is too old to park against, so the app has to mint a fresh one.
	callback := h.login(t, stale.Ticket, "late-code")
	if callback.StatusCode != http.StatusOK || !strings.Contains(callback.Body, "Go back to FUTO Notes") {
		t.Fatalf("login with an expired ticket = %d %q, want the go-back page", callback.StatusCode, callback.Body)
	}
	if response := h.poll(t, stale.Ticket); response.StatusCode != http.StatusNotFound {
		t.Fatalf("poll after an expired ticket's login = %d %q, want 404", response.StatusCode, response.Body)
	}

	// A fulfilled ticket expires too, once its redemption window closes.
	fresh := h.mint(t)
	if callback := h.login(t, fresh.Ticket, "fresh-code"); callback.StatusCode != http.StatusOK {
		t.Fatalf("login = %d %q", callback.StatusCode, callback.Body)
	}
	h.expire(t, fresh.Ticket)
	if response := h.poll(t, fresh.Ticket); response.StatusCode != http.StatusNotFound {
		t.Fatalf("poll for an expired fulfilled ticket = %d %q, want 404", response.StatusCode, response.Body)
	}
}

func (h handoffHarness) userCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := h.database.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestHostedAllowlistAdmitsOnlyItsEmailDomains(t *testing.T) {
	h := newHandoffHarness(t, "futo.org")
	h.claims.Sub, h.claims.Email = "outsider-sub", "person@example.test"

	minted := h.mint(t)
	rejected := h.login(t, minted.Ticket, "outsider-code")
	if rejected.StatusCode != http.StatusForbidden {
		t.Fatalf("off-domain callback = %d %q, want 403", rejected.StatusCode, rejected.Body)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal([]byte(rejected.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "signup_not_open" || !strings.Contains(body.Error, "not open") {
		t.Fatalf("off-domain callback body = %+v, want the sign-up-not-open error", body)
	}
	for _, cookie := range rejected.Cookies {
		if cookie.Name == "session" && cookie.Value != "" {
			t.Fatal("off-domain callback opened a session")
		}
	}
	if count := h.userCount(t); count != 0 {
		t.Fatalf("users after an off-domain login = %d, want 0", count)
	}
	// The refusal spends the ticket, so the app stops waiting immediately
	// instead of polling out the rest of the ticket's life.
	if response := h.poll(t, minted.Ticket); response.StatusCode != http.StatusNotFound {
		t.Fatalf("poll after an off-domain login = %d %q, want 404", response.StatusCode, response.Body)
	}

	// The comparison ignores case on both sides.
	h.claims.Sub, h.claims.Email = "insider-sub", "Person@FUTO.ORG"
	admitted := h.login(t, "", "insider-code")
	if admitted.StatusCode != http.StatusOK {
		t.Fatalf("allowlisted callback = %d %q, want 200", admitted.StatusCode, admitted.Body)
	}
	if count := h.userCount(t); count != 1 {
		t.Fatalf("users after an allowlisted login = %d, want 1", count)
	}
}

func TestUnsetHostedAllowlistAdmitsEveryone(t *testing.T) {
	h := newHandoffHarness(t)
	h.claims.Sub, h.claims.Email = "anyone-sub", "anyone@somewhere.test"

	if response := h.login(t, "", "anyone-code"); response.StatusCode != http.StatusOK {
		t.Fatalf("callback with no allowlist = %d %q, want 200", response.StatusCode, response.Body)
	}
	if count := h.userCount(t); count != 1 {
		t.Fatalf("users = %d, want the identity admitted", count)
	}
}
