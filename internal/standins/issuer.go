package standins

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"sync"

	"futo-notes-server/internal/oidc"
)

// Issuer stands in for Zitadel. It serves the three endpoints the server
// reads from discovery and completes the Authorization Code + PKCE flow
// without a human at a login page: /authorize redirects straight back to the
// server's callback with a code. That auto-approval is the whole point —
// fetching the Login Hand-off's login URL with a plain HTTP client finishes
// the login, so an automated suite needs no browser.
//
// It is not a rubber stamp. The token endpoint checks client_secret_basic
// against the configured credentials and recomputes the S256 challenge from
// the verifier the server presents, so a run that passes has exercised the
// real PKCE round trip rather than only the redirect shape.
type Issuer struct {
	// Origin is where it is listening, which is what the server is given as
	// OIDC_ISSUER. Set before StartIssuer returns and never written again.
	Origin string

	clientID     string
	clientSecret string

	mu sync.Mutex
	// claims is what /userinfo answers with, and therefore who the next
	// completed login signs in as.
	claims oidc.Claims
	// challenges maps an issued authorization code to the PKCE challenge
	// that was presented with it.
	challenges map[string]string
}

// StartIssuer starts the stand-in on a loopback port. sub, email and name are
// the identity it signs in as until SignInAs says otherwise.
func StartIssuer(clientID, clientSecret, sub, email, name string) (*Issuer, func(), error) {
	issuer := &Issuer{
		clientID:     clientID,
		clientSecret: clientSecret,
		claims:       oidc.Claims{Sub: sub, Email: email, Name: name},
		challenges:   map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", issuer.discovery)
	mux.HandleFunc("GET /authorize", issuer.authorize)
	mux.HandleFunc("POST /token", issuer.token)
	mux.HandleFunc("GET /userinfo", issuer.userinfo)

	origin, stop, err := serve(mux)
	if err != nil {
		return nil, nil, err
	}
	issuer.Origin = origin
	return issuer, stop, nil
}

// SignInAs sets the identity the next completed login will produce.
func (f *Issuer) SignInAs(sub, email, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = oidc.Claims{Sub: sub, Email: email, Name: name}
}

func (f *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"issuer":                 f.Origin,
		"authorization_endpoint": f.Origin + "/authorize",
		"token_endpoint":         f.Origin + "/token",
		"userinfo_endpoint":      f.Origin + "/userinfo",
	})
}

func (f *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("client_id") != f.clientID {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		http.Error(w, "expected an S256 PKCE challenge", http.StatusBadRequest)
		return
	}
	redirectURI, err := url.Parse(query.Get("redirect_uri"))
	if err != nil || !redirectURI.IsAbs() {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}

	code := randomToken()
	f.mu.Lock()
	f.challenges[code] = query.Get("code_challenge")
	f.mu.Unlock()

	redirectURI.RawQuery = url.Values{
		"code":  {code},
		"state": {query.Get("state")},
	}.Encode()
	http.Redirect(w, r, redirectURI.String(), http.StatusFound)
}

func (f *Issuer) token(w http.ResponseWriter, r *http.Request) {
	user, password, ok := r.BasicAuth()
	if !ok || user != f.clientID || password != f.clientSecret {
		http.Error(w, "bad client credentials", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "unreadable form", http.StatusBadRequest)
		return
	}
	code, verifier := r.PostForm.Get("code"), r.PostForm.Get("code_verifier")

	f.mu.Lock()
	challenge, known := f.challenges[code]
	delete(f.challenges, code)
	f.mu.Unlock()
	if !known {
		http.Error(w, "unknown or already-redeemed code", http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		http.Error(w, "code_verifier does not match the challenge", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": "access-" + code})
}

func (f *Issuer) userinfo(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	claims := f.claims
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, claims)
}
