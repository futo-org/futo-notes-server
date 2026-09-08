// Package oidc implements just enough of OpenID Connect Authorization Code +
// PKCE to support "Log in with FUTO" against Zitadel: discovery, the
// authorize redirect, the code exchange, and a userinfo fetch. It does not
// parse or verify ID tokens; user identity comes from the userinfo endpoint,
// reached over TLS with the access token obtained via the code exchange.
package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// httpClient is the single outbound client used for discovery, the code
// exchange, and userinfo requests. Requests carry the caller's context.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// Provider holds the endpoints read from an issuer's discovery document.
type Provider struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	UserinfoEndpoint      string
}

type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// Discover fetches <issuer>/.well-known/openid-configuration, checks that
// its issuer field matches the configured issuer, and requires the
// authorization, token, and userinfo endpoints to be present.
func Discover(ctx context.Context, issuer string) (*Provider, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, fmt.Errorf("building discovery request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching discovery document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery document: unexpected status %d", resp.StatusCode)
	}

	var doc discoveryDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding discovery document: %w", err)
	}
	if doc.Issuer != issuer {
		return nil, fmt.Errorf("discovery document issuer %q does not match configured issuer %q", doc.Issuer, issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.UserinfoEndpoint == "" {
		return nil, fmt.Errorf("discovery document is missing authorization_endpoint, token_endpoint, or userinfo_endpoint")
	}

	return &Provider{
		Issuer:                doc.Issuer,
		AuthorizationEndpoint: doc.AuthorizationEndpoint,
		TokenEndpoint:         doc.TokenEndpoint,
		UserinfoEndpoint:      doc.UserinfoEndpoint,
	}, nil
}
