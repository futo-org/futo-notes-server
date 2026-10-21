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

	"futo-notes-server/internal/auth"
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
// DevUI is off, so a callback without a ticket redirects to the account page
// and any hand-off page in a response is the ticketed path's own work.
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
	if admitted.StatusCode != http.StatusFound {
		t.Fatalf("allowlisted callback = %d %q, want 302", admitted.StatusCode, admitted.Body)
	}
	if count := h.userCount(t); count != 1 {
		t.Fatalf("users after an allowlisted login = %d, want 1", count)
	}
}

func TestUnsetHostedAllowlistAdmitsEveryone(t *testing.T) {
	h := newHandoffHarness(t)
	h.claims.Sub, h.claims.Email = "anyone-sub", "anyone@somewhere.test"

	if response := h.login(t, "", "anyone-code"); response.StatusCode != http.StatusFound {
		t.Fatalf("callback with no allowlist = %d %q, want 302", response.StatusCode, response.Body)
	}
	if count := h.userCount(t); count != 1 {
		t.Fatalf("users = %d, want the identity admitted", count)
	}
}

func TestABrowserLoginLandsOnTheAccountPage(t *testing.T) {
	h := newHandoffHarness(t)

	callback := h.login(t, "", "browser-code")
	if callback.StatusCode != http.StatusFound || callback.Header.Get("Location") != accountPath {
		t.Fatalf("callback = %d, location %q, want 302 to %s", callback.StatusCode, callback.Header.Get("Location"), accountPath)
	}
	var session *http.Cookie
	for _, cookie := range callback.Cookies {
		if cookie.Name == "session" && cookie.Value != "" {
			session = cookie
		}
	}
	if session == nil {
		t.Fatal("browser login set no session cookie for the account page to read")
	}
	if me := doRequest(t, h.client, http.MethodGet, h.url+"/api/auth", "", "", session); me.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/auth on the browser session = %d %q", me.StatusCode, me.Body)
	}

	page := doRequest(t, h.client, http.MethodGet, h.url+accountPath, "", "", nil)
	if page.StatusCode != http.StatusOK || !strings.Contains(page.Body, "/api/auth/oidc/start") {
		t.Fatalf("GET %s = %d, want the page with a login link: %q", accountPath, page.StatusCode, page.Body)
	}
}

// expiryOf reads the stored expiry of the session a raw token opens.
func (h handoffHarness) expiryOf(t *testing.T, token string) time.Time {
	t.Helper()
	var expiry db.Time
	if err := h.database.QueryRowContext(context.Background(),
		`SELECT expires_at FROM sessions WHERE access_token_hash = $1`,
		auth.HashToken(token)).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	return expiry.Time
}

// lastUsed rewinds a session's clock without sleeping: it rewrites the expiry
// as the one a session whose last authenticated request was ago in the past
// would carry, given the lifetime it was opened with.
func (h handoffHarness) lastUsed(t *testing.T, token string, ttl, ago time.Duration) {
	t.Helper()
	if _, err := h.database.ExecContext(context.Background(),
		`UPDATE sessions SET expires_at = $1 WHERE access_token_hash = $2`,
		db.Timestamp(time.Now().Add(-ago).Add(ttl)), auth.HashToken(token)); err != nil {
		t.Fatal(err)
	}
}

// One login produces two sessions with different lifetimes: the app's, which
// slides so a device that syncs never has to log in again, and the browser's
// cookie, which keeps the fixed week.
func TestLoginHandoffSessionSlidesAndTheBrowserCookieDoesNot(t *testing.T) {
	h := newHandoffHarness(t)
	minted := h.mint(t)

	callback := h.login(t, minted.Ticket, "handoff-code")
	var browserCookie *http.Cookie
	for _, cookie := range callback.Cookies {
		if cookie.Name == "session" {
			browserCookie = cookie
		}
	}
	if browserCookie == nil {
		t.Fatal("ticketed callback did not set the session cookie for the browser")
	}
	var login loginBody
	if err := json.Unmarshal([]byte(h.poll(t, minted.Ticket).Body), &login); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if got, want := h.expiryOf(t, login.Token), now.Add(auth.SlidingSessionTTL); got.Sub(want).Abs() > time.Minute {
		t.Fatalf("app session expires at %v, want about %v (90 days)", got, want)
	}
	if got, want := h.expiryOf(t, browserCookie.Value), now.Add(auth.SessionTTL); got.Sub(want).Abs() > time.Minute {
		t.Fatalf("browser session expires at %v, want about %v (7 days)", got, want)
	}

	// Two hours on: one more authenticated request from each.
	h.lastUsed(t, login.Token, auth.SlidingSessionTTL, 2*time.Hour)
	h.lastUsed(t, browserCookie.Value, auth.SessionTTL, 2*time.Hour)
	browserBefore := h.expiryOf(t, browserCookie.Value)

	if app := doRequest(t, h.client, http.MethodGet, h.url+"/api/auth", "", login.Token, nil); app.StatusCode != http.StatusOK {
		t.Fatalf("app request = %d %q, want 200", app.StatusCode, app.Body)
	}
	if browser := doRequest(t, h.client, http.MethodGet, h.url+"/api/auth", "", "", browserCookie); browser.StatusCode != http.StatusOK {
		t.Fatalf("browser request = %d %q, want 200", browser.StatusCode, browser.Body)
	}

	if got, want := h.expiryOf(t, login.Token), time.Now().Add(auth.SlidingSessionTTL); got.Sub(want).Abs() > time.Minute {
		t.Fatalf("app session expires at %v after a request, want about %v — it did not slide", got, want)
	}
	if got := h.expiryOf(t, browserCookie.Value); !got.Equal(browserBefore) {
		t.Fatalf("browser session expiry moved from %v to %v; a cookie session stays fixed", browserBefore, got)
	}
}

// A hand-off session that goes 90 days without a request is dead, and the
// client is told to log in again rather than that its vault is gone.
func TestLoginHandoffSessionDiesAfterNinetyDaysOfSilence(t *testing.T) {
	h := newHandoffHarness(t)
	minted := h.mint(t)
	h.login(t, minted.Ticket, "handoff-code")
	var login loginBody
	if err := json.Unmarshal([]byte(h.poll(t, minted.Ticket).Body), &login); err != nil {
		t.Fatal(err)
	}

	h.lastUsed(t, login.Token, auth.SlidingSessionTTL, auth.SlidingSessionTTL+time.Minute)

	response := doRequest(t, h.client, http.MethodGet, h.url+"/api/auth", "", login.Token, nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("request on a 90-day-old session = %d %q, want 401", response.StatusCode, response.Body)
	}
	if !strings.Contains(response.Body, `"code":"invalid_session"`) {
		t.Fatalf("401 body = %q, want the invalid_session code", response.Body)
	}
}
