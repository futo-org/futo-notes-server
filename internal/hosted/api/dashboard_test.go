package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/config"
)

const dashboardCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; " +
	"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

var csrfMeta = regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)">`)

// postForm posts form to path as a browser form submission would.
func (c *client) postForm(path string, form url.Values, headers ...string) response {
	c.h.t.Helper()
	req, err := http.NewRequestWithContext(c.h.t.Context(), http.MethodPost, c.h.api.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		c.h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return c.send(req)
}

// csrf returns the CSRF token the dashboard renders for this browser.
func (c *client) csrf() string {
	c.h.t.Helper()
	res := c.expect(http.MethodGet, "/dashboard/devices", nil, http.StatusOK)
	m := csrfMeta.FindStringSubmatch(res.body)
	if m == nil {
		c.h.t.Fatalf("no CSRF meta tag in %s", res.body)
	}
	return m[1]
}

// submit posts a dashboard form without JavaScript: the token rides in the
// form.
func (c *client) submit(path string, form url.Values) response {
	c.h.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf_token", c.csrf())
	return c.postForm(path, form)
}

// hxSubmit posts a dashboard form as htmx does: the token rides in the
// header hx-headers sets.
func (c *client) hxSubmit(path string, form url.Values) response {
	c.h.t.Helper()
	return c.postForm(path, form, "HX-Request", "true", "X-CSRF-Token", c.csrf())
}

func expectRedirect(t *testing.T, res response, location string) {
	t.Helper()
	if res.status != http.StatusSeeOther || res.header.Get("Location") != location {
		t.Fatalf("response = %d to %q %s, want 303 to %s", res.status, res.header.Get("Location"), res.body, location)
	}
}

func expectContains(t *testing.T, res response, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(res.body, part) {
			t.Fatalf("response lacks %q:\n%s", part, res.body)
		}
	}
}

func expectLacks(t *testing.T, res response, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if strings.Contains(res.body, part) {
			t.Fatalf("response has %q:\n%s", part, res.body)
		}
	}
}

// sessionIDs returns the caller's current session and its others, from the
// JSON API.
func (c *client) sessionIDs() (current string, others []string) {
	c.h.t.Helper()
	var listed struct {
		Sessions []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"sessions"`
	}
	c.expect(http.MethodGet, "/api/sessions", nil, http.StatusOK).json(c.h.t, &listed)
	for _, s := range listed.Sessions {
		if s.Current {
			current = s.ID
		} else {
			others = append(others, s.ID)
		}
	}
	return current, others
}

func (c *client) account() accountBody {
	c.h.t.Helper()
	var a accountBody
	c.expect(http.MethodGet, "/api/account", nil, http.StatusOK).json(c.h.t, &a)
	return a
}

func sessionCookie(t *testing.T, res response) *http.Cookie {
	t.Helper()
	for _, line := range res.header.Values("Set-Cookie") {
		cookie, err := http.ParseSetCookie(line)
		if err != nil {
			t.Fatal(err)
		}
		if cookie.Name == "session" {
			return cookie
		}
	}
	return nil
}

func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.database.ExecContext(h.t.Context(), query, args...); err != nil {
		h.t.Fatal(err)
	}
}

// regionBrowser signs a new browser in and chooses us-east through the
// JSON API.
func (h *harness) regionBrowser() *client {
	h.t.Helper()
	browser := h.dashboardLogin(h.newSub())
	browser.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	return browser
}

func TestDashboardSignedOut(t *testing.T) {
	h := newHarness(t)
	browser := h.client()
	for _, path := range []string{"/dashboard", "/dashboard/region", "/dashboard/devices", "/dashboard/delete"} {
		expectRedirect(t, browser.do(http.MethodGet, path, nil), "/dashboard/sign-in")
	}
	expectRedirect(t, browser.postForm("/dashboard/restore", url.Values{}), "/dashboard/sign-in")

	// htmx would follow a 3xx inside its request; it gets HX-Redirect.
	res := browser.do(http.MethodGet, "/dashboard/devices", nil, "HX-Request", "true")
	if res.status != http.StatusUnauthorized || res.header.Get("HX-Redirect") != "/dashboard/sign-in" {
		t.Fatalf("htmx signed out = %d %v", res.status, res.header)
	}

	res = browser.expect(http.MethodGet, "/dashboard/sign-in", nil, http.StatusOK)
	expectContains(t, res, `<!doctype html>`, `<a class="button" href="/api/auth/oidc/start">Sign in</a>`)
	expectLacks(t, res, `csrf-token`, `/dashboard/sign-out`, `<nav`)

	// A cookie naming no session is cleared on the way to sign-in.
	stale := h.client()
	target, err := url.Parse(h.api.URL)
	if err != nil {
		t.Fatal(err)
	}
	stale.http.Jar.SetCookies(target, []*http.Cookie{{Name: "session", Value: "not-a-session"}})
	res = stale.do(http.MethodGet, "/dashboard", nil)
	expectRedirect(t, res, "/dashboard/sign-in")
	if cookie := sessionCookie(t, res); cookie == nil || cookie.MaxAge >= 0 {
		t.Fatalf("stale cookie not cleared: %v", res.header.Values("Set-Cookie"))
	}
}

// TestDashboardOIDCSignInAndRegionChoice signs in through the mock issuer,
// lands on the dashboard, and chooses a region exactly once.
func TestDashboardOIDCSignInAndRegionChoice(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	browser := h.dashboardLogin(sub)

	expectRedirect(t, browser.do(http.MethodGet, "/dashboard", nil), "/dashboard/region")
	res := browser.expect(http.MethodGet, "/dashboard/region", nil, http.StatusOK)
	expectContains(t, res,
		`<input type="radio" id="region-us-east" name="region_id" value="us-east" required>`,
		`<label for="region-us-east"><strong>us-east name</strong> <span>us-east description</span></label>`,
		`<label for="region-eu-central"><strong>eu-central name</strong> <span>eu-central description</span></label>`,
		`cannot be changed later`)

	expectRenderedError(t, browser.submit("/dashboard/region", url.Values{"region_id": {"mars"}}), http.StatusBadRequest)

	expectRedirect(t, browser.submit("/dashboard/region", url.Values{"region_id": {"eu-central"}}), "/dashboard")
	if a := browser.account(); a.Region == nil || a.Region.ID != "eu-central" {
		t.Fatalf("account = %#v", a)
	}
	res = browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK)
	expectContains(t, res, `<strong>eu-central name</strong>`, `Test `+sub, sub+`@example.test`)

	// Once chosen, the choice is not offered again and cannot be repeated.
	expectRedirect(t, browser.do(http.MethodGet, "/dashboard/region", nil), "/dashboard")
	expectRenderedError(t, browser.submit("/dashboard/region", url.Values{"region_id": {"us-east"}}), http.StatusConflict)
	res = browser.hxSubmit("/dashboard/region", url.Values{"region_id": {"us-east"}})
	expectAlertFragment(t, res, http.StatusConflict, "already chosen")
	if a := browser.account(); a.Region.ID != "eu-central" {
		t.Fatalf("region changed to %s", a.Region.ID)
	}
}

// expectRenderedError checks a full-page dashboard error.
func expectRenderedError(t *testing.T, res response, status int) {
	t.Helper()
	if res.status != status || !strings.HasPrefix(res.body, "<!doctype html>") || !strings.Contains(res.body, `<p class="alert">`) {
		t.Fatalf("response = %d %s, want a %d error page", res.status, res.body, status)
	}
}

// expectCallbackError checks a sign-in callback that cannot complete: a
// dashboard error page saying message, under the dashboard's headers.
func expectCallbackError(t *testing.T, res response, status int, message string) {
	t.Helper()
	expectRenderedError(t, res, status)
	if !strings.Contains(res.body, message) || !strings.HasPrefix(res.header.Get("Content-Type"), "text/html") ||
		res.header.Get("Content-Security-Policy") != dashboardCSP || res.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("callback error = %d %v %s, want a page saying %q", res.status, res.header, res.body, message)
	}
}

// expectAlertFragment checks an htmx error: a fragment retargeted to the
// alert region.
func expectAlertFragment(t *testing.T, res response, status int, message string) {
	t.Helper()
	if res.status != status || res.header.Get("HX-Retarget") != "#alert" || res.header.Get("HX-Reswap") != "innerHTML" ||
		!strings.HasPrefix(res.body, `<p class="alert">`) || !strings.Contains(res.body, message) {
		t.Fatalf("response = %d %v %s, want a %d alert fragment", res.status, res.header, res.body, status)
	}
}

func TestDashboardOverview(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	browser := h.dashboardLogin(sub)
	browser.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	userID := browser.account().User.ID

	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	browser.expect(http.MethodPost, "/api/collections", nil, http.StatusCreated).json(t, &claimed)
	receipt, _ := h.receipt("us-east", userID, 1536)
	browser.expect(http.MethodPost, "/api/collections/"+claimed.Collection.ID+"/objects", map[string]string{"receipt": receipt}, http.StatusCreated)

	res := browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK)
	expectContains(t, res,
		`<dt>Name</dt><dd>Test `+sub+`</dd>`,
		`<dt>Email</dt><dd>`+sub+`@example.test</dd>`,
		`<p class="item-title"><strong>us-east name</strong></p>`, `<p class="item-desc">us-east description</p>`,
		`<dt>Collections</dt><dd>1</dd>`,
		`<dt>Blobs</dt><dd>1</dd>`,
		`<dt>Stored</dt><dd><data value="1536">1.5 KiB</data></dd>`,
		`aria-current="page">Overview</a>`)
	expectLacks(t, res, `scheduled for deletion`, `/dashboard/restore`)

	browser.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
	purge := browser.account().Deletion.PurgeAt.UTC()
	res = browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK)
	expectContains(t, res,
		`Your account is scheduled for deletion`,
		`<time datetime="`+purge.Format(time.RFC3339)+`">`+purge.Format("2006-01-02 15:04")+` UTC</time>`,
		`<form method="post" action="/dashboard/restore" hx-post="/dashboard/restore">`,
		`<button type="submit">Cancel deletion</button>`)

	// The user-controlled name is escaped.
	h.exec(`UPDATE users SET name = $2 WHERE id = $1`, userID, `<script>alert(1)</script>`)
	res = browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK)
	expectContains(t, res, `<dd>&lt;script&gt;alert(1)&lt;/script&gt;</dd>`)
	expectLacks(t, res, `<script>alert(1)`)
}

func TestDashboardDevices(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	browser := h.dashboardLogin(sub)
	app, _ := h.appLogin(sub)
	other, _ := h.appLogin(sub)
	current, others := browser.sessionIDs()
	if len(others) != 2 {
		t.Fatalf("other sessions = %v", others)
	}

	res := browser.expect(http.MethodGet, "/dashboard/devices", nil, http.StatusOK)
	if strings.Count(res.body, `<span class="tag">This device</span>`) != 1 || strings.Count(res.body, "<tr>") != 4 {
		t.Fatalf("devices page:\n%s", res.body)
	}
	expectContains(t, res, `Go-http-client/1.1`, `UTC</time>`)
	expectLacks(t, res, `/dashboard/devices/`+current+`/revoke`)
	for _, id := range others {
		expectContains(t, res, `hx-post="/dashboard/devices/`+id+`/revoke" hx-target="#devices" hx-swap="outerHTML"`)
	}

	// htmx gets just the refreshed list.
	appCurrent, appOthers := app.sessionIDs()
	if !slices.Contains(others, appCurrent) || len(appOthers) != 2 {
		t.Fatalf("app session %s not among %v", appCurrent, others)
	}
	res = browser.hxSubmit("/dashboard/devices/"+appCurrent+"/revoke", nil)
	if res.status != http.StatusOK || !strings.HasPrefix(res.body, `<section id="devices"`) || strings.Contains(res.body, "<!doctype") ||
		strings.Contains(res.body, appCurrent) || strings.Count(res.body, "<tr>") != 3 {
		t.Fatalf("revoke fragment = %d %s", res.status, res.body)
	}
	expectHostedError(t, app.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized, "invalid_session")

	expectAlertFragment(t, browser.hxSubmit("/dashboard/devices/"+appCurrent+"/revoke", nil), http.StatusNotFound, "already signed out")
	expectAlertFragment(t, browser.hxSubmit("/dashboard/devices/"+current+"/revoke", nil), http.StatusBadRequest, "Use Sign out")
	browser.expect(http.MethodGet, "/api/account", nil, http.StatusOK)

	// Without JavaScript the form posts and lands back on the list.
	otherCurrent, _ := other.sessionIDs()
	expectRedirect(t, browser.submit("/dashboard/devices/"+otherCurrent+"/revoke", nil), "/dashboard/devices")
	expectHostedError(t, other.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized, "invalid_session")
	expectRenderedError(t, browser.submit("/dashboard/devices/not-a-uuid/revoke", nil), http.StatusNotFound)
}

func TestDashboardDeleteAndCancel(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	browser := h.dashboardLogin(sub)
	browser.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	app, _ := h.appLogin(sub)

	res := browser.expect(http.MethodGet, "/dashboard/delete", nil, http.StatusOK)
	expectContains(t, res, `After 30 days`, `To confirm, type <kbd>DELETE</kbd>`,
		`<input type="text" id="confirm" name="confirm" required`, `<button type="submit" class="danger">Delete my account</button>`)

	for _, confirm := range []string{"", "delete", "yes"} {
		expectRenderedError(t, browser.submit("/dashboard/delete", url.Values{"confirm": {confirm}}), http.StatusUnprocessableEntity)
	}
	expectAlertFragment(t, browser.hxSubmit("/dashboard/delete", url.Values{"confirm": {"nope"}}), http.StatusUnprocessableEntity, "Type DELETE")
	if a := browser.account(); a.Deletion != nil {
		t.Fatalf("unconfirmed deletion scheduled: %#v", a.Deletion)
	}

	res = browser.hxSubmit("/dashboard/delete", url.Values{"confirm": {"DELETE"}})
	if res.status != http.StatusOK || res.header.Get("HX-Push-Url") != "/dashboard" {
		t.Fatalf("delete = %d %v %s", res.status, res.header, res.body)
	}
	expectContains(t, res, `Your account is scheduled for deletion`, `Cancel deletion`)
	a := browser.account()
	if a.Deletion == nil || a.Deletion.PurgeAt.Sub(a.Deletion.RequestedAt) != 30*24*time.Hour {
		t.Fatalf("deletion = %#v", a.Deletion)
	}
	// Every other device is signed out, and sync stops.
	expectHostedError(t, app.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized, "invalid_session")
	browser.expect(http.MethodGet, "/api/collections", nil, http.StatusForbidden)

	res = browser.expect(http.MethodGet, "/dashboard/delete", nil, http.StatusOK)
	expectContains(t, res, `Your account is scheduled for deletion`, `action="/dashboard/restore"`)
	expectLacks(t, res, `name="confirm"`)

	expectRedirect(t, browser.submit("/dashboard/restore", nil), "/dashboard")
	if a := browser.account(); a.Deletion != nil {
		t.Fatalf("deletion after cancel = %#v", a.Deletion)
	}
	expectLacks(t, browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `scheduled for deletion`)
	browser.expect(http.MethodGet, "/api/collections", nil, http.StatusOK)

	// Without JavaScript, deleting lands on the overview.
	expectRedirect(t, browser.submit("/dashboard/delete", url.Values{"confirm": {" DELETE "}}), "/dashboard")
	expectContains(t, browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `scheduled for deletion`)
}

func TestDashboardSignOut(t *testing.T) {
	h := newHarness(t)
	browser := h.regionBrowser()
	res := browser.submit("/dashboard/sign-out", nil)
	expectRedirect(t, res, "/dashboard/sign-in")
	if cookie := sessionCookie(t, res); cookie == nil || cookie.MaxAge >= 0 {
		t.Fatalf("sign-out cookie = %v", res.header.Values("Set-Cookie"))
	}
	expectRedirect(t, browser.do(http.MethodGet, "/dashboard", nil), "/dashboard/sign-in")
}

func TestDashboardCSRF(t *testing.T) {
	h := newHarness(t)
	browser := h.regionBrowser()
	stranger := h.regionBrowser()
	theirs := stranger.csrf()
	current, _ := browser.sessionIDs()

	for _, action := range []string{"/dashboard/region", "/dashboard/devices/" + current + "/revoke",
		"/dashboard/delete", "/dashboard/restore", "/dashboard/sign-out"} {
		form := url.Values{"region_id": {"eu-central"}, "confirm": {"DELETE"}}
		cases := map[string]response{
			"missing":            browser.postForm(action, form),
			"wrong":              browser.postForm(action, url.Values{"csrf_token": {"x"}, "confirm": {"DELETE"}}),
			"other session's":    browser.postForm(action, url.Values{"csrf_token": {theirs}, "confirm": {"DELETE"}}),
			"htmx missing":       browser.postForm(action, form, "HX-Request", "true"),
			"htmx other session": browser.postForm(action, form, "HX-Request", "true", "X-CSRF-Token", theirs),
		}
		for name, res := range cases {
			if res.status != http.StatusForbidden {
				t.Fatalf("%s with %s token = %d %s, want 403", action, name, res.status, res.body)
			}
		}
	}
	// Nothing changed, and the browser is still signed in.
	if a := browser.account(); a.Region.ID != "us-east" || a.Deletion != nil {
		t.Fatalf("account changed: %#v", a)
	}
	expectAlertFragment(t, browser.postForm("/dashboard/restore", nil, "HX-Request", "true"), http.StatusForbidden, "expired")

	// The token is the same on every page for the session, in the meta tag,
	// hx-headers and each form.
	token := browser.csrf()
	res := browser.expect(http.MethodGet, "/dashboard/delete", nil, http.StatusOK)
	expectContains(t, res, `<meta name="csrf-token" content="`+token+`">`,
		`hx-headers="{&#34;X-CSRF-Token&#34;:&#34;`+token+`&#34;}"`,
		`<input type="hidden" name="csrf_token" value="`+token+`">`)
	if token == theirs {
		t.Fatal("two sessions share a CSRF token")
	}
}

func TestDashboardSecurityHeaders(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.CookieSecure = true })
	browser := h.client()
	callback := browser.authorize(browser.do(http.MethodGet, "/api/auth/oidc/start", nil), h.newSub())
	res := browser.do(http.MethodGet, callback, nil)
	cookie := sessionCookie(t, res)
	if res.header.Get("Location") != "/dashboard" || cookie == nil || !cookie.HttpOnly || !cookie.Secure ||
		cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge != int((90*24*time.Hour).Seconds()) {
		t.Fatalf("callback = %d %v", res.status, res.header)
	}
	// The jar keeps Secure cookies off plain http, so present it by hand.
	browser.http.Jar.SetCookies(&url.URL{Scheme: "http", Host: strings.TrimPrefix(h.api.URL, "http://")},
		[]*http.Cookie{{Name: "session", Value: cookie.Value}})

	responses := map[string]response{
		"sign-in":  browser.do(http.MethodGet, "/dashboard/sign-in", nil),
		"region":   browser.do(http.MethodGet, "/dashboard/region", nil),
		"devices":  browser.do(http.MethodGet, "/dashboard/devices", nil),
		"fragment": browser.do(http.MethodGet, "/dashboard/devices", nil, "HX-Request", "true"),
		"error":    browser.postForm("/dashboard/restore", nil),
		"redirect": browser.do(http.MethodGet, "/dashboard", nil),
		"htmx":     browser.do(http.MethodGet, "/dashboard/static/htmx-2.0.4.min.js", nil),
		"css":      browser.do(http.MethodGet, "/dashboard/static/dashboard.css", nil),
		"font":     browser.do(http.MethodGet, "/dashboard/static/Barlow-400-latin.woff2", nil),
		"icon":     browser.do(http.MethodGet, "/dashboard/static/icon-32.png", nil),
	}
	scriptTag := regexp.MustCompile(`<script[^>]*>`)
	inlineStyleOrHandler := regexp.MustCompile(`\son[a-z]+=|\sstyle=|<style`)
	for name, res := range responses {
		if got := res.header.Get("Content-Security-Policy"); got != dashboardCSP {
			t.Errorf("%s CSP = %q", name, got)
		}
		if res.header.Get("X-Content-Type-Options") != "nosniff" || res.header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s headers = %v", name, res.header)
		}
		if strings.HasPrefix(res.header.Get("Content-Type"), "text/html") {
			for _, loc := range scriptTag.FindAllStringIndex(res.body, -1) {
				tag := res.body[loc[0]:loc[1]]
				if !strings.Contains(tag, ` src="/dashboard/static/`) || !strings.HasPrefix(res.body[loc[1]:], "</script>") {
					t.Errorf("%s has an inline script: %s", name, tag)
				}
			}
			if inlineStyleOrHandler.MatchString(res.body) {
				t.Errorf("%s has an inline style or event handler:\n%s", name, res.body)
			}
			if res.header.Get("Cache-Control") != "no-store" {
				t.Errorf("%s Cache-Control = %q", name, res.header.Get("Cache-Control"))
			}
		}
	}
	page := responses["devices"]
	expectContains(t, page, `<script src="/dashboard/static/htmx-2.0.4.min.js" defer></script>`,
		`<meta name="htmx-config" content='{"includeIndicatorStyles":false,"allowEval":false,"allowScriptTags":false,`)

	htmx := responses["htmx"]
	sum := sha256.Sum256([]byte(htmx.body))
	if htmx.status != http.StatusOK || !strings.HasPrefix(htmx.header.Get("Content-Type"), "text/javascript") ||
		hex.EncodeToString(sum[:]) != "e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447" ||
		htmx.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("htmx = %d %v (%d bytes)", htmx.status, htmx.header, len(htmx.body))
	}
	if css := responses["css"]; css.status != http.StatusOK || !strings.HasPrefix(css.header.Get("Content-Type"), "text/css") {
		t.Fatalf("css = %d %v", css.status, css.header)
	}
	if font := responses["font"]; font.status != http.StatusOK || font.header.Get("Content-Type") != "font/woff2" ||
		font.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("font = %d %v", font.status, font.header)
	}
	if icon := responses["icon"]; icon.status != http.StatusOK || icon.header.Get("Content-Type") != "image/png" {
		t.Fatalf("icon = %d %v", icon.status, icon.header)
	}
	expectContains(t, page, `<link rel="icon" type="image/png" sizes="32x32" href="/dashboard/static/icon-32.png">`,
		`<img src="/dashboard/static/icon-128.png" alt="" width="32" height="32">FUTO Notes</a>`)
	for _, path := range []string{"/dashboard/static/", "/dashboard/static/missing.js", "/dashboard/static/..%2ftemplates%2flayout.html",
		"/dashboard/static/Barlow-OFL.txt"} {
		if res := browser.do(http.MethodGet, path, nil); res.status != http.StatusNotFound {
			t.Fatalf("GET %s = %d", path, res.status)
		}
	}
}

func TestDashboardFragmentsAndFullPages(t *testing.T) {
	h := newHarness(t)
	browser := h.regionBrowser()
	for _, path := range []string{"/dashboard", "/dashboard/devices", "/dashboard/delete"} {
		full := browser.expect(http.MethodGet, path, nil, http.StatusOK)
		if !strings.HasPrefix(full.body, "<!doctype html>") || !strings.Contains(full.body, `<header class="site">`) ||
			!strings.Contains(full.body, `<div id="page">`) || !strings.Contains(full.body, `<main id="main">`) {
			t.Fatalf("GET %s full page:\n%s", path, full.body)
		}
		fragment := browser.do(http.MethodGet, path, nil, "HX-Request", "true")
		if fragment.status != http.StatusOK || !strings.HasPrefix(fragment.body, "<title>") ||
			strings.Contains(fragment.body, "<!doctype") || strings.Contains(fragment.body, `<header`) ||
			!strings.Contains(fragment.body, `<main id="main">`) || !strings.Contains(fragment.body, `<nav aria-label="Account"`) {
			t.Fatalf("GET %s fragment:\n%s", path, fragment.body)
		}
		if full.header.Get("Vary") != "HX-Request" || fragment.header.Get("Vary") != "HX-Request" {
			t.Fatalf("GET %s Vary = %q, %q", path, full.header.Get("Vary"), fragment.header.Get("Vary"))
		}
	}
	res := browser.expect(http.MethodGet, "/dashboard/devices", nil, http.StatusOK)
	expectContains(t, res, `<a href="/dashboard/devices" hx-get="/dashboard/devices" aria-current="page">Devices</a>`)
}

func TestSessionCookieSlides(t *testing.T) {
	h := newHarness(t)
	browser := h.regionBrowser()
	app, _ := h.appLogin(h.newSub())
	current, _ := browser.sessionIDs()

	if cookie := sessionCookie(t, browser.expect(http.MethodGet, "/dashboard", nil, http.StatusOK)); cookie != nil {
		t.Fatalf("cookie reissued without a slide: %v", cookie)
	}
	stale := func(sessionID string) {
		t.Helper()
		h.exec(`UPDATE sessions SET last_used_at = now() - interval '1 hour', expires_at = now() + interval '1 day'
			WHERE id = $1`, sessionID)
	}
	expiry := func(sessionID string) time.Time {
		t.Helper()
		var expiresAt time.Time
		if err := h.database.QueryRowContext(t.Context(), `SELECT expires_at FROM sessions WHERE id = $1`, sessionID).Scan(&expiresAt); err != nil {
			t.Fatal(err)
		}
		return expiresAt
	}
	for _, path := range []string{"/dashboard", "/api/account", "/api/collections"} {
		stale(current)
		res := browser.expect(http.MethodGet, path, nil, http.StatusOK)
		cookie := sessionCookie(t, res)
		if cookie == nil || cookie.MaxAge != int((90*24*time.Hour).Seconds()) || !cookie.HttpOnly ||
			cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
			t.Fatalf("GET %s after a slide: Set-Cookie %v", path, res.header.Values("Set-Cookie"))
		}
		if time.Until(expiry(current)) < 89*24*time.Hour {
			t.Fatalf("GET %s did not slide the session: %v", path, expiry(current))
		}
	}

	// A bearer session slides without a cookie.
	appSession, _ := app.sessionIDs()
	stale(appSession)
	res := app.expect(http.MethodGet, "/api/account", nil, http.StatusOK)
	if len(res.header.Values("Set-Cookie")) != 0 || time.Until(expiry(appSession)) < 89*24*time.Hour {
		t.Fatalf("bearer slide: %v", res.header)
	}
}

// TestDashboardParity checks every state-changing dashboard route against
// the JSON API route that does the same thing: each is exercised one way and
// observed the other, against the same account.
func TestDashboardParity(t *testing.T) {
	h := newHarness(t)
	parity := []struct {
		dashboard, api string
		run            func(t *testing.T)
	}{
		{"POST /dashboard/region", "PUT /api/account/region", func(t *testing.T) {
			viaDashboard := h.dashboardLogin(h.newSub())
			expectRedirect(t, viaDashboard.submit("/dashboard/region", url.Values{"region_id": {"us-east"}}), "/dashboard")
			if a := viaDashboard.account(); a.Region == nil || a.Region.ID != "us-east" {
				t.Fatalf("API sees %#v", a.Region)
			}
			expectHostedError(t, viaDashboard.do(http.MethodPut, "/api/account/region", map[string]string{"region_id": "eu-central"}), http.StatusConflict, "region_already_chosen")

			viaAPI := h.dashboardLogin(h.newSub())
			viaAPI.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "eu-central"}, http.StatusOK)
			expectContains(t, viaAPI.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `<strong>eu-central name</strong>`)
			expectRenderedError(t, viaAPI.submit("/dashboard/region", url.Values{"region_id": {"us-east"}}), http.StatusConflict)
		}},
		{"POST /dashboard/devices/{id}/revoke", "DELETE /api/sessions/{id}", func(t *testing.T) {
			sub := h.newSub()
			browser := h.dashboardLogin(sub)
			first, _ := h.appLogin(sub)
			second, _ := h.appLogin(sub)
			firstID, _ := first.sessionIDs()
			secondID, _ := second.sessionIDs()

			expectRedirect(t, browser.submit("/dashboard/devices/"+firstID+"/revoke", nil), "/dashboard/devices")
			if _, others := browser.sessionIDs(); slices.Contains(others, firstID) {
				t.Fatalf("API still lists %s", firstID)
			}
			expectHostedError(t, browser.do(http.MethodDelete, "/api/sessions/"+firstID, nil), http.StatusNotFound, "not_found")

			browser.expect(http.MethodDelete, "/api/sessions/"+secondID, nil, http.StatusNoContent)
			expectLacks(t, browser.expect(http.MethodGet, "/dashboard/devices", nil, http.StatusOK), secondID)
			expectRenderedError(t, browser.submit("/dashboard/devices/"+secondID+"/revoke", nil), http.StatusNotFound)
		}},
		{"POST /dashboard/delete", "DELETE /api/account", func(t *testing.T) {
			sub := h.newSub()
			viaDashboard := h.dashboardLogin(sub)
			app, _ := h.appLogin(sub)
			expectRedirect(t, viaDashboard.submit("/dashboard/delete", url.Values{"confirm": {"DELETE"}}), "/dashboard")
			if a := viaDashboard.account(); a.Deletion == nil {
				t.Fatal("API sees no deletion")
			}
			expectHostedError(t, app.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized, "invalid_session")

			sub = h.newSub()
			viaAPI := h.dashboardLogin(sub)
			app, _ = h.appLogin(sub)
			viaAPI.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
			expectContains(t, viaAPI.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `scheduled for deletion`)
			expectHostedError(t, app.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized, "invalid_session")
		}},
		{"POST /dashboard/restore", "POST /api/account/restore", func(t *testing.T) {
			viaDashboard := h.regionBrowser()
			viaDashboard.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
			expectRedirect(t, viaDashboard.submit("/dashboard/restore", nil), "/dashboard")
			if a := viaDashboard.account(); a.Deletion != nil {
				t.Fatalf("API sees %#v", a.Deletion)
			}

			viaAPI := h.regionBrowser()
			expectRedirect(t, viaAPI.submit("/dashboard/delete", url.Values{"confirm": {"DELETE"}}), "/dashboard")
			viaAPI.expect(http.MethodPost, "/api/account/restore", nil, http.StatusOK)
			expectLacks(t, viaAPI.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `scheduled for deletion`)
		}},
		{"POST /dashboard/sign-out", "POST /api/auth/logout", func(t *testing.T) {
			viaDashboard := h.regionBrowser()
			expectRedirect(t, viaDashboard.submit("/dashboard/sign-out", nil), "/dashboard/sign-in")
			expectHostedError(t, viaDashboard.do(http.MethodGet, "/api/account", nil), http.StatusUnauthorized, "unauthorized")

			viaAPI := h.regionBrowser()
			viaAPI.expect(http.MethodPost, "/api/auth/logout", nil, http.StatusNoContent)
			expectRedirect(t, viaAPI.do(http.MethodGet, "/dashboard", nil), "/dashboard/sign-in")
		}},
		{"POST /dashboard/billing/checkout", "POST /api/billing/checkout", func(t *testing.T) {
			b := newBillingHarness(t)
			viaDashboard := b.regionBrowser()
			res := viaDashboard.submit("/dashboard/billing/checkout", nil)
			checkout, err := url.Parse(res.header.Get("Location"))
			if res.status != http.StatusSeeOther || err != nil || checkout.Path != "/dev/billing/checkout" {
				t.Fatalf("dashboard checkout = %d %v", res.status, res.header)
			}
			viaDashboard.postForm(checkout.Path, checkout.Query())
			if !viaDashboard.billing().Entitled {
				t.Fatal("API sees no subscription")
			}
			expectHostedError(t, viaDashboard.do(http.MethodPost, "/api/billing/checkout", nil), http.StatusConflict, "already_subscribed")

			viaAPI := b.regionBrowser()
			viaAPI.subscribe()
			expectContains(t, viaAPI.expect(http.MethodGet, "/dashboard", nil, http.StatusOK), `data-billing-status="active"`)
			expectRenderedError(t, viaAPI.submit("/dashboard/billing/checkout", nil), http.StatusConflict)
		}},
		{"POST /dashboard/billing/portal", "GET /api/billing/portal", func(t *testing.T) {
			b := newBillingHarness(t)
			browser := b.regionBrowser()
			browser.subscribe()
			var portal struct {
				URL string `json:"url"`
			}
			browser.expect(http.MethodGet, "/api/billing/portal", nil, http.StatusOK).json(t, &portal)
			expectRedirect(t, browser.submit("/dashboard/billing/portal", nil), portal.URL)

			store := b.regionBrowser()
			token, _ := b.appStore.sell(store.billing().StoreAccountToken)
			store.expect(http.MethodPost, "/api/billing/app-store/transactions", map[string]string{"signed_transaction": token}, http.StatusOK)
			expectHostedError(t, store.do(http.MethodGet, "/api/billing/portal", nil), http.StatusConflict, "managed_by_store")
			expectRenderedError(t, store.submit("/dashboard/billing/portal", nil), http.StatusConflict)
		}},
	}

	// The table covers exactly the dashboard's actions, and each one's API
	// route is mounted.
	want := h.server.DashboardParity()
	got := map[string]string{}
	for _, p := range parity {
		got[p.dashboard] = p.api
	}
	if !maps.Equal(got, want) {
		t.Fatalf("parity table = %v, server declares %v", got, want)
	}
	routes := h.server.Routes()
	for _, route := range routes {
		if strings.HasPrefix(route, "POST /dashboard") || strings.HasPrefix(route, "PUT /dashboard") ||
			strings.HasPrefix(route, "DELETE /dashboard") || strings.HasPrefix(route, "PATCH /dashboard") {
			if _, ok := want[route]; !ok {
				t.Errorf("dashboard action %s has no API parity entry", route)
			}
		}
	}
	for dashboard, api := range want {
		if !slices.Contains(routes, dashboard) || !slices.Contains(routes, api) {
			t.Errorf("parity %s -> %s: route not mounted (routes: %v)", dashboard, api, routes)
		}
	}

	for _, p := range parity {
		t.Run(p.dashboard, p.run)
	}
}
