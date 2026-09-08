package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAuthorizeURL(t *testing.T) {
	p := &Provider{AuthorizationEndpoint: "https://idp.test/oauth/v2/authorize"}
	state, verifier := "the-state", "the-verifier"

	raw := p.AuthorizeURL("client-id", "https://app.test/callback", state, verifier)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("AuthorizeURL() = %q, not a valid URL: %v", raw, err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://idp.test/oauth/v2/authorize" {
		t.Fatalf("authorize endpoint = %q", got)
	}
	q := u.Query()
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type = %q", q.Get("response_type"))
	}
	if q.Get("client_id") != "client-id" {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "https://app.test/callback" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("scope") != "openid profile email" {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
	if q.Get("state") != state {
		t.Fatalf("state = %q", q.Get("state"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	wantChallenge := func() string {
		sum := sha256.Sum256([]byte(verifier))
		return base64.RawURLEncoding.EncodeToString(sum[:])
	}()
	if q.Get("code_challenge") != wantChallenge {
		t.Fatalf("code_challenge = %q, want %q", q.Get("code_challenge"), wantChallenge)
	}
}

func TestExchangeSendsBasicAuthAndVerifier(t *testing.T) {
	var gotUser, gotPass string
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "the-access-token"})
	}))
	defer server.Close()

	p := &Provider{TokenEndpoint: server.URL}
	token, err := p.Exchange(context.Background(), "client-id", "client-secret", "https://app.test/callback", "the-code", "the-verifier")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if token != "the-access-token" {
		t.Fatalf("token = %q", token)
	}
	if gotUser != "client-id" || gotPass != "client-secret" {
		t.Fatalf("basic auth = %q:%q, want client-id:client-secret", gotUser, gotPass)
	}
	if gotForm.Get("code_verifier") != "the-verifier" {
		t.Fatalf("code_verifier = %q", gotForm.Get("code_verifier"))
	}
	if gotForm.Get("code") != "the-code" {
		t.Fatalf("code = %q", gotForm.Get("code"))
	}
	if gotForm.Get("grant_type") != "authorization_code" {
		t.Fatalf("grant_type = %q", gotForm.Get("grant_type"))
	}
}

func TestExchangeNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()

	p := &Provider{TokenEndpoint: server.URL}
	_, err := p.Exchange(context.Background(), "client-id", "client-secret", "https://app.test/callback", "bad-code", "verifier")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("Exchange() error = %v, want an error naming the status", err)
	}
}

func TestUserinfoParsesClaims(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Claims{
			Sub:               "sub-123",
			Email:             "person@example.test",
			Name:              "Person",
			PreferredUsername: "person",
		})
	}))
	defer server.Close()

	p := &Provider{UserinfoEndpoint: server.URL}
	claims, err := p.Userinfo(context.Background(), "the-access-token")
	if err != nil {
		t.Fatalf("Userinfo() error = %v", err)
	}
	if gotAuth != "Bearer the-access-token" {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
	if claims.Sub != "sub-123" || claims.Email != "person@example.test" || claims.Name != "Person" || claims.PreferredUsername != "person" {
		t.Fatalf("claims = %+v", claims)
	}
}
