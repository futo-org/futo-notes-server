package mockoidc_test

import (
	"context"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"futo-notes-server/internal/mockoidc"
	"futo-notes-server/internal/mockoidc/mockoidctest"
)

const (
	testClientID     = "notes-test-client"
	testClientSecret = "notes test secret"
	testRedirect     = "http://app.test/api/auth/oidc/callback"
	testPostLogout   = "http://app.test/"
)

func testConfig() mockoidc.Config {
	return mockoidc.Config{
		ClientID:               testClientID,
		ClientSecret:           testClientSecret,
		RedirectURIs:           []string{testRedirect},
		PostLogoutRedirectURIs: []string{testPostLogout},
	}
}

type relyingParty struct {
	provider *oidc.Provider
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func newRelyingParty(t *testing.T, issuer string) relyingParty {
	t.Helper()
	provider, err := oidc.NewProvider(context.Background(), issuer)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	return relyingParty{
		provider: provider,
		oauth: oauth2.Config{
			ClientID:     testClientID,
			ClientSecret: testClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  testRedirect,
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: testClientID}),
	}
}

var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// callbackQuery returns the query of the issuer's redirect, failing unless it
// redirected to testRedirect.
func callbackQuery(t *testing.T, resp *http.Response) url.Values {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want 302: %s", resp.StatusCode, body)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("location: %v", err)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != testRedirect {
		t.Fatalf("redirected to %q, want %q", got, testRedirect)
	}
	return loc.Query()
}

func TestCodeFlowWithLoginForm(t *testing.T) {
	srv := mockoidctest.Start(t, testConfig())
	rp := newRelyingParty(t, srv.URL)
	ctx := context.Background()

	pkce := oauth2.GenerateVerifier()
	authURL := rp.oauth.AuthCodeURL("state-123", oauth2.S256ChallengeOption(pkce), oidc.Nonce("nonce-456"))

	page, err := http.Get(authURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(body), `<form method="post"`) {
		t.Fatalf("login form: status %d\n%s", page.StatusCode, body)
	}

	form := url.Values{"sub": {"user-1"}, "email": {"alice@example.test"}, "name": {"Alice"}}
	for _, m := range regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`).FindAllStringSubmatch(string(body), -1) {
		form.Add(m[1], html.UnescapeString(m[2]))
	}
	resp, err := noRedirects.PostForm(srv.URL+"/authorize", form)
	if err != nil {
		t.Fatalf("submit login: %v", err)
	}
	q := callbackQuery(t, resp)
	if q.Get("state") != "state-123" || q.Get("iss") != srv.URL {
		t.Fatalf("callback query %v", q)
	}

	tok, err := rp.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(pkce))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	rawID, _ := tok.Extra("id_token").(string)
	idToken, err := rp.verifier.Verify(ctx, rawID)
	if err != nil {
		t.Fatalf("verify id token: %v", err)
	}
	if idToken.Subject != "user-1" || idToken.Nonce != "nonce-456" {
		t.Errorf("id token sub %q nonce %q", idToken.Subject, idToken.Nonce)
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims.Email != "alice@example.test" || !claims.EmailVerified || claims.Name != "Alice" {
		t.Errorf("id token claims %+v", claims)
	}

	info, err := rp.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		t.Fatalf("userinfo: %v", err)
	}
	if info.Subject != "user-1" || info.Email != "alice@example.test" || !info.EmailVerified {
		t.Errorf("userinfo %+v", info)
	}

	if _, err := rp.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(pkce)); err == nil {
		t.Error("code was accepted twice")
	}
}

func testAuthorize(t *testing.T, srv *httptest.Server, rp relyingParty, sub, pkce string) url.Values {
	t.Helper()
	u, _ := url.Parse(rp.oauth.AuthCodeURL("s", oauth2.S256ChallengeOption(pkce)))
	q := u.Query()
	q.Set("sub", sub)
	resp, err := noRedirects.Get(srv.URL + "/test/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("test authorize: %v", err)
	}
	return callbackQuery(t, resp)
}

func TestTestAuthorizeShortcut(t *testing.T) {
	srv := mockoidctest.Start(t, testConfig())
	rp := newRelyingParty(t, srv.URL)
	ctx := context.Background()
	pkce := oauth2.GenerateVerifier()

	code := testAuthorize(t, srv, rp, "283749283749", pkce).Get("code")
	tok, err := rp.oauth.Exchange(ctx, code, oauth2.VerifierOption(pkce))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	idToken, err := rp.verifier.Verify(ctx, tok.Extra("id_token").(string))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var claims struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if idToken.Subject != "283749283749" || claims.Email != "283749283749@example.test" || claims.Name != "283749283749" {
		t.Errorf("sub %q claims %+v", idToken.Subject, claims)
	}
}

func TestTokenEndpointRejects(t *testing.T) {
	srv := mockoidctest.Start(t, testConfig())
	rp := newRelyingParty(t, srv.URL)
	ctx := context.Background()

	cases := map[string]func(code, pkce string) error{
		"wrong code_verifier": func(code, _ string) error {
			_, err := rp.oauth.Exchange(ctx, code, oauth2.VerifierOption(oauth2.GenerateVerifier()))
			return err
		},
		"missing code_verifier": func(code, _ string) error {
			_, err := rp.oauth.Exchange(ctx, code)
			return err
		},
		"wrong client secret": func(code, pkce string) error {
			c := rp.oauth
			c.ClientSecret = "nope"
			_, err := c.Exchange(ctx, code, oauth2.VerifierOption(pkce))
			return err
		},
		"client secret in body": func(code, pkce string) error {
			c := rp.oauth
			c.Endpoint.AuthStyle = oauth2.AuthStyleInParams
			_, err := c.Exchange(ctx, code, oauth2.VerifierOption(pkce))
			return err
		},
		"different redirect_uri": func(code, pkce string) error {
			c := rp.oauth
			c.RedirectURL = "http://app.test/other"
			_, err := c.Exchange(ctx, code, oauth2.VerifierOption(pkce))
			return err
		},
		"unknown code": func(_, pkce string) error {
			_, err := rp.oauth.Exchange(ctx, "made-up", oauth2.VerifierOption(pkce))
			return err
		},
	}
	for name, exchange := range cases {
		t.Run(name, func(t *testing.T) {
			pkce := oauth2.GenerateVerifier()
			code := testAuthorize(t, srv, rp, "user-1", pkce).Get("code")
			err := exchange(code, pkce)
			if _, ok := errors.AsType[*oauth2.RetrieveError](err); !ok {
				t.Fatalf("got %v, want an OAuth error", err)
			}
			// Once the client is authenticated, a failed exchange of this code
			// still burns it, as single use requires.
			wantRedeemable := strings.Contains(name, "secret") || name == "unknown code"
			if _, err := rp.oauth.Exchange(ctx, code, oauth2.VerifierOption(pkce)); (err == nil) != wantRedeemable {
				t.Errorf("code redeemable afterwards: %v, want %v", err == nil, wantRedeemable)
			}
		})
	}
}

func TestAuthorizeRejects(t *testing.T) {
	srv := mockoidctest.Start(t, testConfig())
	rp := newRelyingParty(t, srv.URL)
	base, _ := url.Parse(rp.oauth.AuthCodeURL("s", oauth2.S256ChallengeOption(oauth2.GenerateVerifier())))

	cases := map[string]func(url.Values){
		"no PKCE":               func(q url.Values) { q.Del("code_challenge"); q.Del("code_challenge_method") },
		"plain PKCE":            func(q url.Values) { q.Set("code_challenge_method", "plain") },
		"bad challenge":         func(q url.Values) { q.Set("code_challenge", "short") },
		"unknown client":        func(q url.Values) { q.Set("client_id", "other") },
		"unregistered redirect": func(q url.Values) { q.Set("redirect_uri", "http://evil.test/cb") },
		"no openid scope":       func(q url.Values) { q.Set("scope", "email") },
		"implicit flow":         func(q url.Values) { q.Set("response_type", "token") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			q := base.Query()
			mutate(q)
			for _, path := range []string{"/authorize", "/test/authorize"} {
				q.Set("sub", "user-1")
				resp, err := noRedirects.Get(srv.URL + path + "?" + q.Encode())
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("%s: status %d, want 400", path, resp.StatusCode)
				}
			}
		})
	}
}

func TestUserinfoRejectsUnknownToken(t *testing.T) {
	srv := mockoidctest.Start(t, testConfig())
	rp := newRelyingParty(t, srv.URL)
	_, err := rp.provider.UserInfo(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "nope", TokenType: "Bearer"}))
	if err == nil {
		t.Fatal("userinfo accepted an unknown token")
	}
}

func TestEndSession(t *testing.T) {
	srv := mockoidctest.Start(t, testConfig())

	resp, err := noRedirects.Get(srv.URL + "/end_session?" + url.Values{"post_logout_redirect_uri": {testPostLogout}, "state": {"bye"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != testPostLogout+"?state=bye" {
		t.Errorf("status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err = noRedirects.Get(srv.URL + "/end_session?post_logout_redirect_uri=" + url.QueryEscape("http://evil.test/"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unregistered post-logout redirect: status %d, want 400", resp.StatusCode)
	}
}

func TestIssuerWithPath(t *testing.T) {
	srv := httptest.NewUnstartedServer(nil)
	cfg := testConfig()
	cfg.Issuer = "http://" + srv.Listener.Addr().String() + "/oidc"
	p, err := mockoidc.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = p.Handler()
	srv.Start()
	defer srv.Close()

	rp := newRelyingParty(t, p.Issuer())
	pkce := oauth2.GenerateVerifier()
	u, _ := url.Parse(rp.oauth.AuthCodeURL("s", oauth2.S256ChallengeOption(pkce)))
	q := u.Query()
	q.Set("sub", "user-1")
	resp, err := noRedirects.Get(p.Issuer() + "/test/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	code := callbackQuery(t, resp).Get("code")
	tok, err := rp.oauth.Exchange(context.Background(), code, oauth2.VerifierOption(pkce))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if _, err := rp.verifier.Verify(context.Background(), tok.Extra("id_token").(string)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	cases := map[string]func(*mockoidc.Config){
		"no issuer":       func(c *mockoidc.Config) { c.Issuer = "" },
		"relative issuer": func(c *mockoidc.Config) { c.Issuer = "/oidc" },
		"issuer query":    func(c *mockoidc.Config) { c.Issuer = "http://x.test?a=b" },
		"no client id":    func(c *mockoidc.Config) { c.ClientID = "" },
		"no secret":       func(c *mockoidc.Config) { c.ClientSecret = "" },
		"no redirect":     func(c *mockoidc.Config) { c.RedirectURIs = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Issuer = "http://localhost:8092"
			mutate(&cfg)
			if _, err := mockoidc.New(cfg); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
