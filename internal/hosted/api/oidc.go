package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog/hlog"
	"golang.org/x/oauth2"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/syncapi"
)

const (
	callbackRoute = "/auth/oidc/callback"
	callbackPath  = "/api" + callbackRoute
	// stateCookie binds a login to the browser that started it, so a
	// callback URL lured into another browser cannot complete it there.
	stateCookie = "oidc_state"
)

// oidcClient talks to the identity provider. Discovery happens on first use
// rather than at startup, so an unreachable provider fails sign-ins without
// stopping sync.
type oidcClient struct {
	cfg         config.OIDC
	redirectURL string
	http        *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
}

func newOIDCClient(cfg config.OIDC, redirectURL string) *oidcClient {
	return &oidcClient{cfg: cfg, redirectURL: redirectURL, http: &http.Client{Timeout: 10 * time.Second}}
}

// context returns ctx carrying the client's HTTP client for go-oidc and
// oauth2.
func (c *oidcClient) context(ctx context.Context) context.Context {
	return oidc.ClientContext(ctx, c.http)
}

// get returns the OAuth2 configuration and ID token verifier, discovering the
// provider on first success.
func (c *oidcClient) get(ctx context.Context) (oauth2.Config, *oidc.IDTokenVerifier, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider == nil {
		provider, err := oidc.NewProvider(c.context(ctx), c.cfg.Issuer)
		if err != nil {
			return oauth2.Config{}, nil, fmt.Errorf("discovering OIDC provider: %w", err)
		}
		c.provider = provider
	}
	return oauth2.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		Endpoint:     c.provider.Endpoint(),
		RedirectURL:  c.redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}, c.provider.Verifier(&oidc.Config{ClientID: c.cfg.ClientID}), nil
}

// allowedAppRedirect reports whether an app may name redirect as its
// redirect_uri: a URL with one of the configured custom schemes, or loopback
// http on an IP literal and any port (RFC 8252 section 7.3).
func allowedAppRedirect(redirect string, schemes []string) bool {
	u, err := url.Parse(redirect)
	if err != nil || u.Fragment != "" || u.User != nil {
		return false
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		return u.Port() != "" && (host == "127.0.0.1" || host == "::1") && net.ParseIP(host) != nil
	}
	// url.Parse lowercases the scheme.
	return slices.Contains(schemes, u.Scheme)
}

// handleOIDCStart begins a sign-in. Without redirect_uri it is a dashboard
// sign-in, which ends with the session cookie. An app passes redirect_uri,
// its own PKCE code_challenge (S256), and optionally a state to get back; it
// ends with a one-time code at redirect_uri. reauthenticate=true makes the
// provider ask for credentials again (max_age=0), as deleting the account
// needs a recent sign-in.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	login := account.Login{
		State:        auth.GenerateToken(),
		Nonce:        auth.GenerateToken(),
		CodeVerifier: oauth2.GenerateVerifier(),
	}
	switch q.Get("reauthenticate") {
	case "", "false":
	case "true":
		login.Reauthenticate = true
	default:
		badRequest(w, r, "reauthenticate must be true or false")
		return
	}
	if redirect := q.Get("redirect_uri"); redirect != "" {
		if !allowedAppRedirect(redirect, s.cfg.AppRedirectSchemes) {
			badRequest(w, r, "redirect_uri is not allowed")
			return
		}
		if method := q.Get("code_challenge_method"); method != "" && method != "S256" {
			badRequest(w, r, "code_challenge_method must be S256")
			return
		}
		if !account.ValidCodeChallenge(q.Get("code_challenge")) {
			badRequest(w, r, "code_challenge must be an S256 challenge")
			return
		}
		login.AppRedirectURI, login.AppState, login.AppCodeChallenge = redirect, q.Get("state"), q.Get("code_challenge")
	}

	oauthConfig, _, err := s.oidc.get(r.Context())
	if err != nil {
		hlog.FromRequest(r).Error().Err(err).Msg("starting sign-in")
		httputil.WriteError(w, r, http.StatusServiceUnavailable, codeProviderUnavailable, "Identity provider unavailable")
		return
	}
	if err := s.accounts.SaveLogin(r.Context(), login); err != nil {
		serverError(w, r, "saving login", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    login.State,
		Path:     callbackPath,
		MaxAge:   int(account.LoginTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	options := []oauth2.AuthCodeOption{oidc.Nonce(login.Nonce), oauth2.S256ChallengeOption(login.CodeVerifier)}
	if login.Reauthenticate {
		options = append(options, oauth2.SetAuthURLParam("max_age", "0"))
	}
	http.Redirect(w, r, oauthConfig.AuthCodeURL(login.State, options...), http.StatusFound)
}

// handleOIDCCallback completes a sign-in the identity provider redirected
// back: it swaps the provider's code, upserts the user by sub (refusing a
// new user that ALLOWED_EMAIL_DOMAINS keeps out), and then sets the
// dashboard's session cookie or sends the app its one-time code.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	cookie, err := r.Cookie(stateCookie)
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: callbackPath, MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure})
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		s.metrics.login(r.Context(), false, loginInvalidState)
		httputil.WriteError(w, r, http.StatusBadRequest, codeInvalidLoginState, "Invalid login state")
		return
	}
	login, err := s.accounts.TakeLogin(r.Context(), state)
	if errors.Is(err, account.ErrUnknownLogin) {
		s.metrics.login(r.Context(), false, loginInvalidState)
		httputil.WriteError(w, r, http.StatusBadRequest, codeInvalidLoginState, "Invalid login state")
		return
	}
	if err != nil {
		serverError(w, r, "taking login", err)
		return
	}

	if providerError := q.Get("error"); providerError != "" {
		s.metrics.login(r.Context(), login.ForApp(), loginDenied)
		if login.ForApp() {
			redirectApp(w, r, login, url.Values{"error": {providerError}})
			return
		}
		httputil.WriteError(w, r, http.StatusBadRequest, codeSignInNotCompleted, "Sign-in was not completed: "+providerError)
		return
	}
	code := q.Get("code")
	if code == "" {
		s.metrics.login(r.Context(), login.ForApp(), loginInvalidState)
		if login.ForApp() {
			redirectApp(w, r, login, url.Values{"error": {"invalid_request"}})
			return
		}
		httputil.WriteError(w, r, http.StatusBadRequest, codeSignInNotCompleted, "Sign-in was not completed: missing code")
		return
	}
	identity, err := s.identify(r.Context(), code, login)
	if errors.Is(err, errNonceMismatch) {
		s.metrics.login(r.Context(), login.ForApp(), loginInvalidState)
		httputil.WriteError(w, r, http.StatusBadRequest, codeInvalidLoginState, "Invalid login state")
		return
	}
	if err != nil {
		s.metrics.login(r.Context(), login.ForApp(), loginProviderError)
		hlog.FromRequest(r).Error().Err(err).Msg("completing sign-in with the identity provider")
		httputil.WriteError(w, r, http.StatusBadGateway, codeProviderError, "Could not complete sign-in with the identity provider")
		return
	}
	authenticatedAt := identity.AuthTime
	if authenticatedAt.IsZero() {
		authenticatedAt = time.Now()
	}
	// max_age=0 obliges the provider to authenticate the user now; a stale
	// auth_time means it did not, and the sign-in proves nothing recent.
	if login.Reauthenticate && (identity.AuthTime.IsZero() || time.Since(identity.AuthTime) > account.ReauthWindow) {
		s.metrics.login(r.Context(), login.ForApp(), loginNotReauthenticated)
		if login.ForApp() {
			redirectApp(w, r, login, url.Values{"error": {codeReauthenticationRequired}})
			return
		}
		httputil.WriteError(w, r, http.StatusForbidden, codeReauthenticationRequired,
			"The identity provider did not confirm a fresh sign-in")
		return
	}
	user, err := s.signInUser(r.Context(), identity)
	if errors.Is(err, account.ErrNoUser) {
		s.metrics.login(r.Context(), login.ForApp(), loginSignupNotAllowed)
		if login.ForApp() {
			redirectApp(w, r, login, url.Values{"error": {codeSignupNotAllowed}})
			return
		}
		// A person is in a browser here, so they get a page rather than JSON.
		dashboardHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			renderDashboardError(w, r, http.StatusForbidden, "Sign-ups are limited to approved email domains.")
		})).ServeHTTP(w, r)
		return
	}
	if err != nil {
		serverError(w, r, "upserting user", err)
		return
	}

	if login.ForApp() {
		code, err := s.accounts.CreateAppCode(r.Context(), user.ID, login.AppCodeChallenge, authenticatedAt)
		if err != nil {
			serverError(w, r, "creating app code", err)
			return
		}
		s.metrics.login(r.Context(), true, loginSuccess)
		redirectApp(w, r, login, url.Values{"code": {code}})
		return
	}
	if login.Reauthenticate {
		// The dashboard reauthenticates to delete the account: refresh this
		// browser's session rather than open a second one, and go back to
		// the confirmation.
		if cookie, err := r.Cookie(syncapi.SessionCookie); err == nil {
			ok, err := s.accounts.Reauthenticate(r.Context(), cookie.Value, user.ID, authenticatedAt)
			if err != nil {
				serverError(w, r, "reauthenticating session", err)
				return
			}
			if ok {
				s.metrics.login(r.Context(), false, loginSuccess)
				http.Redirect(w, r, deletePath, http.StatusSeeOther)
				return
			}
		}
	}
	token, err := s.accounts.CreateSession(r.Context(), user.ID, r.UserAgent(), authenticatedAt)
	if err != nil {
		serverError(w, r, "creating session", err)
		return
	}
	s.metrics.login(r.Context(), false, loginSuccess)
	s.metrics.issued(r.Context(), tokenSession)
	s.setSessionCookie(w, token)
	if login.Reauthenticate {
		http.Redirect(w, r, deletePath, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, dashboardPath, http.StatusSeeOther)
}

var errNonceMismatch = errors.New("ID token nonce does not match the login")

// signInUser returns the identity's user. It creates one only when signup
// is open to the identity: with ALLOWED_EMAIL_DOMAINS set, that takes a
// verified email at a listed domain. Otherwise it returns account.ErrNoUser
// for a subject with no user yet, while an existing user always signs in.
func (s *Server) signInUser(ctx context.Context, identity account.Identity) (auth.User, error) {
	if domains := s.cfg.AllowedEmailDomains; len(domains) > 0 {
		at := strings.LastIndexByte(identity.Email, '@')
		if !identity.EmailVerified || at < 0 || !slices.Contains(domains, strings.ToLower(identity.Email[at+1:])) {
			return s.accounts.UpdateUser(ctx, identity)
		}
	}
	return s.accounts.UpsertUser(ctx, identity)
}

// identify swaps the provider's authorization code and returns who the
// verified ID token says signed in.
func (s *Server) identify(ctx context.Context, code string, login account.Login) (account.Identity, error) {
	if code == "" {
		return account.Identity{}, errors.New("callback has no code")
	}
	oauthConfig, verifier, err := s.oidc.get(ctx)
	if err != nil {
		return account.Identity{}, err
	}
	token, err := oauthConfig.Exchange(s.oidc.context(ctx), code, oauth2.VerifierOption(login.CodeVerifier))
	if err != nil {
		return account.Identity{}, fmt.Errorf("exchanging code: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return account.Identity{}, errors.New("token response has no id_token")
	}
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return account.Identity{}, fmt.Errorf("verifying ID token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(login.Nonce)) != 1 {
		return account.Identity{}, errNonceMismatch
	}
	var claims struct {
		Email             string  `json:"email"`
		EmailVerified     bool    `json:"email_verified"`
		Name              string  `json:"name"`
		PreferredUsername string  `json:"preferred_username"`
		AuthTime          float64 `json:"auth_time"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return account.Identity{}, fmt.Errorf("reading ID token claims: %w", err)
	}
	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}
	if name == "" {
		name, _, _ = strings.Cut(claims.Email, "@")
	}
	identity := account.Identity{Subject: idToken.Subject, Email: claims.Email, Name: name, EmailVerified: claims.EmailVerified}
	if claims.AuthTime > 0 {
		identity.AuthTime = time.Unix(int64(claims.AuthTime), 0)
	}
	return identity, nil
}

// redirectApp sends the browser back to the app's redirect_uri with params
// and the app's state.
func redirectApp(w http.ResponseWriter, r *http.Request, login account.Login, params url.Values) {
	// allowedAppRedirect parsed it when the login started.
	target, _ := url.Parse(login.AppRedirectURI)
	q := target.Query()
	maps.Copy(q, params)
	if login.AppState != "" {
		q.Set("state", login.AppState)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// appTokenRequest is the body of POST /api/auth/oidc/token. DeviceName is
// what the device list calls the new session; without it the list shows the
// User-Agent.
type appTokenRequest struct {
	Code         string  `json:"code"`
	CodeVerifier string  `json:"code_verifier"`
	DeviceName   *string `json:"device_name"`
}

// appTokenResponse is what an app gets for its one-time code. The session
// token never travels in a URL.
type appTokenResponse struct {
	Token               string    `json:"token"`
	StorageRefreshToken string    `json:"storage_refresh_token"`
	User                auth.User `json:"user"`
}

// handleOIDCToken swaps an app's one-time code and PKCE verifier for a
// session and a storage refresh token.
func (s *Server) handleOIDCToken(w http.ResponseWriter, r *http.Request) {
	var body appTokenRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Code == "" || body.CodeVerifier == "" {
		badRequest(w, r, "code and code_verifier are required")
		return
	}
	device := account.Device{UserAgent: r.UserAgent()}
	if body.DeviceName != nil {
		device.Name = strings.TrimSpace(*body.DeviceName)
		if n := utf8.RuneCountInString(device.Name); n < 1 || n > account.MaxDeviceName {
			badRequest(w, r, fmt.Sprintf("device_name must be 1 to %d characters", account.MaxDeviceName))
			return
		}
	}
	session, err := s.accounts.RedeemAppCode(r.Context(), body.Code, body.CodeVerifier, device)
	switch {
	case errors.Is(err, account.ErrInvalidCode):
		httputil.WriteError(w, r, http.StatusBadRequest, codeInvalidCode, "Invalid or expired code")
		return
	case errors.Is(err, account.ErrCodeVerifier):
		httputil.WriteError(w, r, http.StatusBadRequest, codeCodeVerifierMismatch, "code_verifier does not match code_challenge")
		return
	case err != nil:
		serverError(w, r, "redeeming app code", err)
		return
	}
	s.metrics.issued(r.Context(), tokenSession)
	s.metrics.issued(r.Context(), tokenStorageRefresh)
	w.Header().Set("Cache-Control", "no-store")
	httputil.WriteJSON(w, r, http.StatusOK, appTokenResponse{Token: session.Token, StorageRefreshToken: session.RefreshToken, User: session.User})
}
