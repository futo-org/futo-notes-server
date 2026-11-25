package api_test

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// staleSignIn makes every session of userID look signed in before the
// reauthentication window.
func (h *harness) staleSignIn(userID string) {
	h.t.Helper()
	h.exec(`UPDATE sessions SET authenticated_at = now() - interval '11 minutes' WHERE user_id = $1`, userID)
}

// reauthStart starts an app sign-in that asks for a fresh authentication and
// checks the provider is told so.
func (c *client) reauthStart() response {
	c.h.t.Helper()
	start := c.do(http.MethodGet, "/api/auth/oidc/start?"+url.Values{
		"redirect_uri":          {"http://127.0.0.1:43123/callback"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"app-state"},
		"reauthenticate":        {"true"},
	}.Encode(), nil)
	location, err := url.Parse(start.header.Get("Location"))
	if err != nil || location.Query().Get("max_age") != "0" {
		c.h.t.Fatalf("reauthenticating start redirects to %s, want max_age=0", start.header.Get("Location"))
	}
	return start
}

func TestAccountDeletionNeedsARecentSignIn(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	app, tokens := h.appLogin(sub)
	h.staleSignIn(tokens.User.ID)
	expectHostedError(t, app.do(http.MethodDelete, "/api/account", nil), http.StatusForbidden, "reauthentication_required")
	if a := app.account(); a.Deletion != nil {
		t.Fatalf("a refused deletion was scheduled: %#v", a.Deletion)
	}

	// The app signs in again with reauthenticate=true and retries with the
	// new session.
	browser := h.client()
	res := browser.do(http.MethodGet, browser.authorize(browser.reauthStart(), sub), nil)
	back, err := url.Parse(res.header.Get("Location"))
	if err != nil || back.Query().Get("code") == "" {
		t.Fatalf("reauthenticated callback = %d %v", res.status, res.header)
	}
	var fresh appTokens
	h.client().expect(http.MethodPost, "/api/auth/oidc/token",
		map[string]string{"code": back.Query().Get("code"), "code_verifier": verifier}, http.StatusOK).json(t, &fresh)
	again := h.client()
	again.bearer = fresh.Token
	again.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
	expectHostedError(t, app.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized, "invalid_session")
}

func TestSignInRecordsTheProvidersAuthTime(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	old := url.Values{"auth_time": {strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)}}

	// An ordinary sign-in accepts an old authentication, the provider's SSO
	// session, but it is not recent enough to delete the account.
	browser := h.client()
	res := browser.do(http.MethodGet, browser.authorizeWith(browser.do(http.MethodGet, "/api/auth/oidc/start", nil), sub, old), nil)
	if res.status != http.StatusSeeOther {
		t.Fatalf("callback = %d %s", res.status, res.body)
	}
	expectHostedError(t, browser.do(http.MethodDelete, "/api/account", nil), http.StatusForbidden, "reauthentication_required")

	// A reauthentication the provider answers with an old auth_time proves
	// nothing and is refused: the app is told so, the dashboard gets a 403.
	app := h.client()
	res = app.do(http.MethodGet, app.authorizeWith(app.reauthStart(), sub, old), nil)
	if res.status != http.StatusFound ||
		res.header.Get("Location") != "http://127.0.0.1:43123/callback?error=reauthentication_required&state=app-state" {
		t.Fatalf("stale app reauthentication = %d %v", res.status, res.header)
	}
	dashboard := h.client()
	start := dashboard.do(http.MethodGet, "/api/auth/oidc/start?reauthenticate=true", nil)
	expectHostedError(t, dashboard.do(http.MethodGet, dashboard.authorizeWith(start, sub, old), nil),
		http.StatusForbidden, "reauthentication_required")

	expectHostedError(t, h.client().do(http.MethodGet, "/api/auth/oidc/start?reauthenticate=yes", nil),
		http.StatusBadRequest, "invalid_request")
}

func TestDashboardDeleteNeedsARecentSignIn(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	browser := h.dashboardLogin(sub)
	browser.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	current, _ := browser.sessionIDs()
	h.staleSignIn(browser.account().User.ID)

	res := browser.expect(http.MethodGet, "/dashboard/delete", nil, http.StatusOK)
	expectContains(t, res, `href="/api/auth/oidc/start?reauthenticate=true"`, `Sign in again`)
	expectLacks(t, res, `name="confirm"`)
	expectRedirect(t, browser.submit("/dashboard/delete", url.Values{"confirm": {"DELETE"}}), "/api/auth/oidc/start?reauthenticate=true")
	if hx := browser.hxSubmit("/dashboard/delete", url.Values{"confirm": {"DELETE"}}); hx.header.Get("HX-Redirect") != "/api/auth/oidc/start?reauthenticate=true" {
		t.Fatalf("htmx delete with a stale sign-in = %d %v", hx.status, hx.header)
	}
	if a := browser.account(); a.Deletion != nil {
		t.Fatalf("a stale sign-in scheduled the deletion: %#v", a.Deletion)
	}

	// Signing in again refreshes this browser's session and comes back to
	// the confirmation.
	start := browser.do(http.MethodGet, "/api/auth/oidc/start?reauthenticate=true", nil)
	if !strings.Contains(start.header.Get("Location"), "max_age=0") {
		t.Fatalf("dashboard reauthentication redirects to %s, want max_age=0", start.header.Get("Location"))
	}
	res = browser.do(http.MethodGet, browser.authorize(start, sub), nil)
	if res.status != http.StatusSeeOther || res.header.Get("Location") != "/dashboard/delete" {
		t.Fatalf("reauthenticated callback = %d %v", res.status, res.header)
	}
	if after, others := browser.sessionIDs(); after != current || len(others) != 0 {
		t.Fatalf("sessions after reauthenticating = %s %v, want the same session %s alone", after, others, current)
	}
	expectContains(t, browser.expect(http.MethodGet, "/dashboard/delete", nil, http.StatusOK), `name="confirm"`)
	expectRedirect(t, browser.submit("/dashboard/delete", url.Values{"confirm": {"DELETE"}}), "/dashboard")
	if a := browser.account(); a.Deletion == nil {
		t.Fatal("deletion after reauthenticating was not scheduled")
	}
}
