package api_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"futo-notes-server/internal/hosted/config"
)

const signupRefusal = "Sign-ups are limited to approved email domains."

// restrictedHarness admits new accounts at futo.org and futo.tech only.
func restrictedHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, func(c *config.Config) { c.AllowedEmailDomains = []string{"futo.org", "futo.tech"} })
}

// dashboardCallback runs a dashboard sign-in as sub with the provider's
// extra parameters (email, email_verified) and returns the callback's
// answer.
func (c *client) dashboardCallback(sub string, extra url.Values) response {
	c.h.t.Helper()
	return c.do(http.MethodGet, c.authorizeWith(c.do(http.MethodGet, "/api/auth/oidc/start", nil), sub, extra), nil)
}

// appCallback runs an app sign-in as sub with the provider's extra
// parameters and returns the callback's answer.
func (c *client) appCallback(sub string, extra url.Values) response {
	c.h.t.Helper()
	start := c.do(http.MethodGet, "/api/auth/oidc/start?"+url.Values{
		"redirect_uri":          {"http://127.0.0.1:43123/callback"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"app-state"},
	}.Encode(), nil)
	return c.do(http.MethodGet, c.authorizeWith(start, sub, extra), nil)
}

// users counts the users with sub.
func (h *harness) users(sub string) int {
	h.t.Helper()
	var n int
	if err := h.database.QueryRowContext(h.t.Context(), `SELECT count(*) FROM users WHERE sub = $1`, sub).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// expectSignedIn checks a dashboard callback that signed the browser in.
func expectSignedIn(t *testing.T, res response) {
	t.Helper()
	if res.status != http.StatusSeeOther || res.header.Get("Location") != "/dashboard" ||
		!strings.Contains(strings.Join(res.header.Values("Set-Cookie"), "\n"), "session=") {
		t.Fatalf("callback = %d %v %s, want a signed-in redirect to /dashboard", res.status, res.header, res.body)
	}
}

// expectSignupRefused checks that sub's sign-in was refused on both paths:
// the dashboard's error page and the app's redirect, with no user created
// and no session issued.
func (h *harness) expectSignupRefused(sub string, extra url.Values) {
	h.t.Helper()
	h.t.Logf("signing up as %v", extra)
	browser := h.client()
	res := browser.dashboardCallback(sub, extra)
	expectRenderedError(h.t, res, http.StatusForbidden)
	expectContains(h.t, res, signupRefusal)
	expectLacks(h.t, res, "futo.org", "futo.tech")
	if res.header.Get("Content-Security-Policy") == "" || res.header.Get("Cache-Control") != "no-store" {
		h.t.Fatalf("refusal page headers = %v, want the dashboard's", res.header)
	}
	if cookies := strings.Join(res.header.Values("Set-Cookie"), "\n"); strings.Contains(cookies, "session=") {
		h.t.Fatalf("refused dashboard sign-in set %s", cookies)
	}
	expectHostedError(h.t, browser.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized, "unauthorized")

	res = h.client().appCallback(sub, extra)
	if res.status != http.StatusFound ||
		res.header.Get("Location") != "http://127.0.0.1:43123/callback?error=signup_not_allowed&state=app-state" {
		h.t.Fatalf("refused app sign-in = %d %v", res.status, res.header)
	}
	if n := h.users(sub); n != 0 {
		h.t.Fatalf("a refused sign-up created %d users", n)
	}
}

func TestSignupOpenWithoutAllowedDomains(t *testing.T) {
	h := newHarness(t)
	// Without the list, neither the domain nor verification matters.
	sub := h.newSub()
	expectSignedIn(t, h.client().dashboardCallback(sub, url.Values{"email": {sub + "@anywhere.example"}, "email_verified": {"false"}}))
	sub = h.newSub()
	res := h.client().appCallback(sub, url.Values{"email": {sub + "@anywhere.example"}, "email_verified": {"false"}})
	if back, err := url.Parse(res.header.Get("Location")); err != nil || back.Query().Get("code") == "" {
		t.Fatalf("app callback = %d %v", res.status, res.header)
	}
	if n := h.users(sub); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}

func TestSignupAllowedDomain(t *testing.T) {
	h := restrictedHarness(t)
	sub := h.newSub()
	browser := h.client()
	expectSignedIn(t, browser.dashboardCallback(sub, url.Values{"email": {sub + "@futo.org"}}))
	if a := browser.account(); a.User.Email != sub+"@futo.org" {
		t.Fatalf("account = %#v", a)
	}

	// The domain matches whatever its case; the second listed one works too.
	sub = h.newSub()
	expectSignedIn(t, h.client().dashboardCallback(sub, url.Values{"email": {sub + "@FUTO.Tech"}}))
	sub = h.newSub()
	res := h.client().appCallback(sub, url.Values{"email": {sub + "@Futo.Org"}})
	if back, err := url.Parse(res.header.Get("Location")); err != nil || back.Query().Get("code") == "" {
		t.Fatalf("app callback = %d %v", res.status, res.header)
	}
	if n := h.users(sub); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}

func TestSignupRefusedOutsideAllowedDomains(t *testing.T) {
	h := restrictedHarness(t)
	for _, email := range []func(sub string) string{
		func(sub string) string { return sub + "@example.test" },
		// Subdomains are not implied, and nothing but the whole domain
		// matches.
		func(sub string) string { return sub + "@eng.futo.org" },
		func(sub string) string { return sub + "@notfuto.org" },
		func(sub string) string { return "futo.org@" + sub + ".example" },
	} {
		sub := h.newSub()
		h.expectSignupRefused(sub, url.Values{"email": {email(sub)}})
	}
}

func TestSignupNeedsAVerifiedEmail(t *testing.T) {
	h := restrictedHarness(t)
	sub := h.newSub()
	h.expectSignupRefused(sub, url.Values{"email": {sub + "@futo.org"}, "email_verified": {"false"}})
}

func TestExistingUserSignsInOutsideAllowedDomains(t *testing.T) {
	h := restrictedHarness(t)
	// A user from before the list, or whose domain left it.
	sub := h.newSub()
	h.exec(`INSERT INTO users (id, sub, email, name) VALUES ($1, $2, $3, 'Old')`, uuid.NewV7().String(), sub, sub+"@example.test")
	browser := h.client()
	expectSignedIn(t, browser.dashboardCallback(sub, url.Values{"email": {sub + "@example.test"}, "email_verified": {"false"}}))
	res := h.client().appCallback(sub, url.Values{"email": {sub + "@example.test"}})
	if back, err := url.Parse(res.header.Get("Location")); err != nil || back.Query().Get("code") == "" {
		t.Fatalf("app callback = %d %v", res.status, res.header)
	}

	// A user who signed up at a listed domain keeps signing in after the
	// provider reports another address, which the account then shows.
	sub = h.newSub()
	expectSignedIn(t, h.client().dashboardCallback(sub, url.Values{"email": {sub + "@futo.org"}}))
	browser = h.client()
	expectSignedIn(t, browser.dashboardCallback(sub, url.Values{"email": {sub + "@elsewhere.example"}, "email_verified": {"false"}}))
	if a := browser.account(); a.User.Email != sub+"@elsewhere.example" {
		t.Fatalf("account = %#v", a)
	}
	if n := h.users(sub); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}
