// Package mockoidc is a development OpenID Connect issuer that logs in any
// subject. It mimics the parts of Zitadel the hosted API relies on: discovery,
// RS256 ID tokens behind a JWKS, the authorization code flow with mandatory
// S256 PKCE, a confidential client using client_secret_basic, userinfo, and
// RP-initiated logout.
//
// Two ways to get a code: GET /authorize shows a small HTML form for manual
// browser use, and GET /test/authorize issues a code for the given sub with no
// UI, for tests and scripts. Never expose it beyond local development.
package mockoidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

const (
	codeTTL        = 10 * time.Minute
	accessTokenTTL = time.Hour
	idTokenTTL     = time.Hour
)

// Config describes the issuer and its single confidential client.
type Config struct {
	// Issuer is the external base URL, e.g. http://localhost:8092. It must be
	// exactly what relying parties are configured with.
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURIs and PostLogoutRedirectURIs are matched exactly, as Zitadel
	// does.
	RedirectURIs           []string
	PostLogoutRedirectURIs []string
}

// Identity is what a login asserts about the user.
type Identity struct {
	Subject string
	Email   string
	Name    string
}

// Provider is an in-memory OIDC issuer. It is safe for concurrent use.
type Provider struct {
	cfg        Config
	signingKey *rsa.PrivateKey
	keyID      string

	mu     sync.Mutex
	codes  map[string]authCode
	tokens map[string]accessToken
}

type authCode struct {
	identity      Identity
	redirectURI   string
	codeChallenge string
	nonce         string
	scopes        []string
	authTime      time.Time
	expiresAt     time.Time
}

type accessToken struct {
	identity  Identity
	scopes    []string
	expiresAt time.Time
}

// New returns a Provider with a freshly generated RS256 signing key.
func New(cfg Config) (*Provider, error) {
	u, err := url.Parse(cfg.Issuer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("issuer %q must be an http(s) URL without query or fragment", cfg.Issuer)
	}
	cfg.Issuer = strings.TrimSuffix(cfg.Issuer, "/")
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("client id and secret are required")
	}
	if len(cfg.RedirectURIs) == 0 {
		return nil, errors.New("at least one redirect URI is required")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	return &Provider{
		cfg:        cfg,
		signingKey: key,
		keyID:      randomString(8),
		codes:      map[string]authCode{},
		tokens:     map[string]accessToken{},
	}, nil
}

// Issuer returns the issuer URL tokens are stamped with.
func (p *Provider) Issuer() string { return p.cfg.Issuer }

// Handler serves every endpoint, relative to the issuer URL's path.
func (p *Provider) Handler() http.Handler {
	r := chi.NewRouter()
	r.Get("/.well-known/openid-configuration", p.discovery)
	r.Get("/jwks", p.jwks)
	r.Get("/authorize", p.authorizeForm)
	r.Post("/authorize", p.authorizeSubmit)
	r.Get("/test/authorize", p.testAuthorize)
	r.Post("/token", p.token)
	r.Get("/userinfo", p.userinfo)
	r.Post("/userinfo", p.userinfo)
	r.Get("/end_session", p.endSession)

	if path := issuerPath(p.cfg.Issuer); path != "" {
		outer := chi.NewRouter()
		outer.Mount(path, r)
		return outer
	}
	return r
}

func issuerPath(issuer string) string {
	u, _ := url.Parse(issuer)
	return strings.TrimSuffix(u.Path, "/")
}

func (p *Provider) endpoint(path string) string { return p.cfg.Issuer + path }

// authRequest is a validated authorization request.
type authRequest struct {
	redirectURI   string
	state         string
	nonce         string
	codeChallenge string
	scopes        []string
}

func (p *Provider) parseAuthRequest(q url.Values) (authRequest, error) {
	if q.Get("client_id") != p.cfg.ClientID {
		return authRequest{}, fmt.Errorf("unknown client_id %q", q.Get("client_id"))
	}
	redirectURI := q.Get("redirect_uri")
	if !slices.Contains(p.cfg.RedirectURIs, redirectURI) {
		return authRequest{}, fmt.Errorf("redirect_uri %q is not registered", redirectURI)
	}
	if rt := q.Get("response_type"); rt != "code" {
		return authRequest{}, fmt.Errorf("response_type must be code, got %q", rt)
	}
	scopes := strings.Fields(q.Get("scope"))
	if !slices.Contains(scopes, "openid") {
		return authRequest{}, errors.New("scope must include openid")
	}
	if q.Get("code_challenge_method") != "S256" {
		return authRequest{}, errors.New("PKCE is required: code_challenge_method must be S256")
	}
	challenge := q.Get("code_challenge")
	if decoded, err := base64.RawURLEncoding.DecodeString(challenge); err != nil || len(decoded) != sha256.Size {
		return authRequest{}, errors.New("code_challenge must be a base64url SHA-256 digest")
	}
	return authRequest{
		redirectURI:   redirectURI,
		state:         q.Get("state"),
		nonce:         q.Get("nonce"),
		codeChallenge: challenge,
		scopes:        scopes,
	}, nil
}

func identityFrom(q url.Values) (Identity, error) {
	id := Identity{Subject: strings.TrimSpace(q.Get("sub")), Email: strings.TrimSpace(q.Get("email")), Name: strings.TrimSpace(q.Get("name"))}
	if id.Subject == "" {
		return Identity{}, errors.New("sub is required")
	}
	if id.Email == "" {
		id.Email = id.Subject + "@example.test"
	}
	if id.Name == "" {
		id.Name = id.Subject
	}
	return id, nil
}

func (p *Provider) authorizeForm(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	if _, err := p.parseAuthRequest(params); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The form's own fields must be the first values the POST sees.
	for _, k := range []string{"sub", "email", "name"} {
		params.Del(k)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := loginPage.Execute(w, loginPageData{Action: p.endpoint("/authorize"), Params: params}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *Provider) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.issueCode(w, r, r.PostForm)
}

func (p *Provider) testAuthorize(w http.ResponseWriter, r *http.Request) {
	p.issueCode(w, r, r.URL.Query())
}

func (p *Provider) issueCode(w http.ResponseWriter, r *http.Request, params url.Values) {
	req, err := p.parseAuthRequest(params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := identityFrom(params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now()
	code := randomString(32)
	p.mu.Lock()
	p.codes[code] = authCode{
		identity:      id,
		redirectURI:   req.redirectURI,
		codeChallenge: req.codeChallenge,
		nonce:         req.nonce,
		scopes:        req.scopes,
		authTime:      now,
		expiresAt:     now.Add(codeTTL),
	}
	p.mu.Unlock()

	target, _ := url.Parse(req.redirectURI)
	q := target.Query()
	q.Set("code", code)
	q.Set("iss", p.cfg.Issuer)
	if req.state != "" {
		q.Set("state", req.state)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	clientID, secret, ok := r.BasicAuth()
	if ok {
		// RFC 6749 §2.3.1: credentials are form-encoded before Basic encoding.
		clientID, _ = url.QueryUnescape(clientID)
		secret, _ = url.QueryUnescape(secret)
	}
	if !ok || clientID != p.cfg.ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(p.cfg.ClientSecret)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="mock-oidc"`)
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client_secret_basic authentication failed")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if gt := r.PostForm.Get("grant_type"); gt != "authorization_code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", fmt.Sprintf("grant_type %q", gt))
		return
	}

	now := time.Now()
	p.mu.Lock()
	code, found := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	p.mu.Unlock()
	switch {
	case !found || now.After(code.expiresAt):
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown, used or expired code")
		return
	case r.PostForm.Get("redirect_uri") != code.redirectURI:
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	case !pkceMatches(r.PostForm.Get("code_verifier"), code.codeChallenge):
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match code_challenge")
		return
	}

	idToken, err := p.signIDToken(code, now)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	access := randomString(32)
	p.mu.Lock()
	p.tokens[access] = accessToken{identity: code.identity, scopes: code.scopes, expiresAt: now.Add(accessTokenTTL)}
	p.mu.Unlock()

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(accessTokenTTL.Seconds()),
		"id_token":     idToken,
		"scope":        strings.Join(code.scopes, " "),
	})
}

func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(oauth2.S256ChallengeFromVerifier(verifier)), []byte(challenge)) == 1
}

func (p *Provider) signIDToken(code authCode, now time.Time) (string, error) {
	claims := scopedClaims(code.identity, code.scopes)
	claims["iss"] = p.cfg.Issuer
	claims["aud"] = p.cfg.ClientID
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(idTokenTTL).Unix()
	claims["auth_time"] = code.authTime.Unix()
	if code.nonce != "" {
		claims["nonce"] = code.nonce
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = p.keyID
	signed, err := t.SignedString(p.signingKey)
	if err != nil {
		return "", fmt.Errorf("sign id token: %w", err)
	}
	return signed, nil
}

func scopedClaims(id Identity, scopes []string) jwt.MapClaims {
	c := jwt.MapClaims{"sub": id.Subject}
	if slices.Contains(scopes, "profile") {
		c["name"] = id.Name
	}
	if slices.Contains(scopes, "email") {
		c["email"] = id.Email
		c["email_verified"] = true
	}
	return c
}

func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	tok, found := p.tokens[bearer]
	p.mu.Unlock()
	if !ok || !found || time.Now().After(tok.expiresAt) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "unknown or expired access token")
		return
	}
	writeJSON(w, http.StatusOK, scopedClaims(tok.identity, tok.scopes))
}

func (p *Provider) endSession(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("post_logout_redirect_uri")
	if target == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "Signed out.")
		return
	}
	if !slices.Contains(p.cfg.PostLogoutRedirectURIs, target) {
		http.Error(w, fmt.Sprintf("post_logout_redirect_uri %q is not registered", target), http.StatusBadRequest)
		return
	}
	if state := r.URL.Query().Get("state"); state != "" {
		u, _ := url.Parse(target)
		q := u.Query()
		q.Set("state", state)
		u.RawQuery = q.Encode()
		target = u.String()
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                p.cfg.Issuer,
		"authorization_endpoint":                p.endpoint("/authorize"),
		"token_endpoint":                        p.endpoint("/token"),
		"userinfo_endpoint":                     p.endpoint("/userinfo"),
		"jwks_uri":                              p.endpoint("/jwks"),
		"end_session_endpoint":                  p.endpoint("/end_session"),
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
		"code_challenge_methods_supported":      []string{"S256"},
		"claims_supported":                      []string{"sub", "iss", "aud", "exp", "iat", "auth_time", "nonce", "name", "email", "email_verified"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := p.signingKey.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": p.keyID,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(bigEndian(pub.E)),
	}}})
}

func bigEndian(n int) []byte {
	var b []byte
	for ; n > 0; n >>= 8 {
		b = append([]byte{byte(n)}, b...)
	}
	return b
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
