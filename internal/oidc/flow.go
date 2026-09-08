package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const scopes = "openid profile email"

// randomURLSafeString returns n bytes of crypto/rand, base64url-encoded
// (unpadded), for use as an OAuth state or PKCE code verifier.
func randomURLSafeString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewFlow generates a fresh random state and PKCE code verifier for one
// login attempt.
func NewFlow() (state, verifier string) {
	return randomURLSafeString(32), randomURLSafeString(32)
}

// codeChallengeS256 derives the PKCE S256 code challenge for verifier.
func codeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL builds the redirect target for the authorization request:
// Authorization Code flow with an S256 PKCE challenge.
func (p *Provider) AuthorizeURL(clientID, redirectURI, state, verifier string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {scopes},
		"state":                 {state},
		"code_challenge":        {codeChallengeS256(verifier)},
		"code_challenge_method": {"S256"},
	}
	return p.AuthorizationEndpoint + "?" + q.Encode()
}

// Exchange trades an authorization code for an access token, authenticating
// with client_secret_basic and presenting the PKCE code verifier.
func (p *Provider) Exchange(ctx context.Context, clientID, clientSecret, redirectURI, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("token endpoint: unexpected status %d: %s", resp.StatusCode, body)
	}

	var tokenResponse struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResponse); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if tokenResponse.AccessToken == "" {
		return "", fmt.Errorf("token response is missing access_token")
	}
	return tokenResponse.AccessToken, nil
}

// Claims is the subset of userinfo claims this server relies on.
type Claims struct {
	Sub               string `json:"sub"`
	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

// Userinfo fetches the authenticated user's claims from the provider's
// userinfo endpoint using the given access token.
func (p *Provider) Userinfo(ctx context.Context, accessToken string) (Claims, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserinfoEndpoint, nil)
	if err != nil {
		return Claims{}, fmt.Errorf("building userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return Claims{}, fmt.Errorf("sending userinfo request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Claims{}, fmt.Errorf("userinfo endpoint: unexpected status %d: %s", resp.StatusCode, body)
	}

	var claims Claims
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return Claims{}, fmt.Errorf("decoding userinfo response: %w", err)
	}
	return claims, nil
}
