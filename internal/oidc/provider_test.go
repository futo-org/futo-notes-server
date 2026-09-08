package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discoveryHandler(t *testing.T, issuer string, override func(doc *discoveryDocument)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		doc := discoveryDocument{
			Issuer:                issuer,
			AuthorizationEndpoint: issuer + "/oauth/v2/authorize",
			TokenEndpoint:         issuer + "/oauth/v2/token",
			UserinfoEndpoint:      issuer + "/oidc/v1/userinfo",
		}
		if override != nil {
			override(&doc)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
}

func newDiscoveryServer(t *testing.T, override func(doc *discoveryDocument)) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	server.Start()
	server.Config.Handler = discoveryHandler(t, server.URL, override)
	return server
}

func TestDiscoverOK(t *testing.T) {
	server := newDiscoveryServer(t, nil)
	defer server.Close()
	issuer := server.URL

	provider, err := Discover(context.Background(), issuer)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if provider.AuthorizationEndpoint != issuer+"/oauth/v2/authorize" {
		t.Fatalf("AuthorizationEndpoint = %q", provider.AuthorizationEndpoint)
	}
	if provider.TokenEndpoint != issuer+"/oauth/v2/token" {
		t.Fatalf("TokenEndpoint = %q", provider.TokenEndpoint)
	}
	if provider.UserinfoEndpoint != issuer+"/oidc/v1/userinfo" {
		t.Fatalf("UserinfoEndpoint = %q", provider.UserinfoEndpoint)
	}
}

func TestDiscoverIssuerMismatch(t *testing.T) {
	server := newDiscoveryServer(t, func(doc *discoveryDocument) {
		doc.Issuer = "https://not-the-configured-issuer.test"
	})
	defer server.Close()

	_, err := Discover(context.Background(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Discover() error = %v, want issuer mismatch error", err)
	}
}

func TestDiscoverMissingEndpoint(t *testing.T) {
	server := newDiscoveryServer(t, func(doc *discoveryDocument) {
		doc.TokenEndpoint = ""
	})
	defer server.Close()

	_, err := Discover(context.Background(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Discover() error = %v, want missing endpoint error", err)
	}
}
