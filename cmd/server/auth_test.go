package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/events"
)

func TestDecodeDevLogin(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantEmail string
		wantName  string
		wantError string
	}{
		{name: "normalizes email and defaults name", body: `{"email":"  DEV@Example.COM "}`, wantEmail: "dev@example.com", wantName: "dev"},
		{name: "trims explicit name", body: `{"email":"a@example.com","name":"  A User  "}`, wantEmail: "a@example.com", wantName: "A User"},
		{name: "missing email", body: `{}`, wantError: "email is required"},
		{name: "blank email", body: `{"email":"  "}`, wantError: "email is required"},
		{name: "bad json", body: `{`, wantError: "invalid json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/auth/dev/login", strings.NewReader(tt.body))
			got, err := decodeDevLogin(r)
			var gotError string
			if err != nil {
				gotError = err.Error()
			}
			if gotError != tt.wantError || got.Email != tt.wantEmail || got.Name != tt.wantName {
				t.Fatalf("decodeDevLogin() = %#v, %q; want email=%q name=%q error=%q", got, gotError, tt.wantEmail, tt.wantName, tt.wantError)
			}
		})
	}
}

// wireResponse is the client-visible part of a response that the login path
// table pins.
type wireResponse struct {
	status          int
	contentType     string
	wwwAuthenticate string
	allow           string
	setCookie       string
	body            string
}

// TestLoginPathAuthBoundary pins how both login paths answer in both auth
// modes, for every method and credential shape: the active mode's login path
// skips authentication entirely (so a dead session never blocks a login),
// while everything else under /api/ authenticates first and then falls
// through to the plain-text catch-all.
func TestLoginPathAuthBoundary(t *testing.T) {
	database := dbtest.SQLite(t)
	user, err := auth.UpsertUserByEmail(t.Context(), database, "boundary@example.invalid", "Boundary")
	if err != nil {
		t.Fatal(err)
	}
	validToken, err := auth.CreateSession(t.Context(), database, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	const invalidToken = "not-a-session"

	plainNotFound := wireResponse{
		status:      http.StatusNotFound,
		contentType: "text/plain; charset=utf-8",
		body:        "404 Not Found",
	}
	unauthorized := wireResponse{
		status:      http.StatusUnauthorized,
		contentType: "application/json",
		body:        "{\"error\":\"unauthorized\"}\n",
	}
	invalidSession := wireResponse{
		status:          http.StatusUnauthorized,
		contentType:     "application/json",
		wwwAuthenticate: `Bearer realm="futo-notes", error="invalid_token"`,
		body:            "{\"code\":\"invalid_session\",\"error\":\"session expired or invalid\"}\n",
	}

	type credentials struct {
		name          string
		cookie        string
		authorization string
		// outcome is the session state requireAuth sees: the cookie wins
		// over the Authorization header.
		outcome string
	}
	credentialCases := []credentials{
		{name: "none", outcome: "missing"},
		{name: "valid cookie", cookie: validToken, outcome: "valid"},
		{name: "invalid cookie", cookie: invalidToken, outcome: "invalid"},
		{name: "valid bearer", authorization: "Bearer " + validToken, outcome: "valid"},
		{name: "invalid bearer", authorization: "Bearer " + invalidToken, outcome: "invalid"},
		{name: "non-bearer authorization", authorization: "Basic " + validToken, outcome: "missing"},
		{name: "valid cookie invalid bearer", cookie: validToken, authorization: "Bearer " + invalidToken, outcome: "valid"},
		{name: "invalid cookie valid bearer", cookie: invalidToken, authorization: "Bearer " + validToken, outcome: "invalid"},
	}
	protected := func(c credentials) wireResponse {
		switch c.outcome {
		case "missing":
			return unauthorized
		case "invalid":
			return invalidSession
		default:
			// Authenticated, but neither login route is registered for this
			// method and path, so the API catch-all answers.
			return plainNotFound
		}
	}

	methods := []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
	}

	for _, mode := range []struct {
		mode         string
		cfg          config.Config
		body         string
		loginResult  wireResponse
		activePath   string
		inactivePath string
	}{
		{
			mode: "dev",
			cfg:  config.Config{AuthMode: config.AuthModeDev},
			body: `{}`,
			loginResult: wireResponse{
				status:      http.StatusBadRequest,
				contentType: "application/json",
				body:        "{\"error\":\"email is required\"}\n",
			},
			activePath:   "/api/auth/dev/login",
			inactivePath: "/api/auth/password/login",
		},
		{
			mode: "password",
			cfg:  config.Config{AuthMode: config.AuthModePassword, Password: "correct password"},
			body: `{"password":"wrong password"}`,
			loginResult: wireResponse{
				status:      http.StatusUnauthorized,
				contentType: "application/json",
				body:        "{\"error\":\"invalid password\"}\n",
			},
			activePath:   "/api/auth/password/login",
			inactivePath: "/api/auth/dev/login",
		},
	} {
		escaped := func(path string) string { return strings.Replace(path, "/login", "%2Flogin", 1) }
		for _, path := range []struct {
			path string
			// public is the response when authentication is skipped, keyed by
			// whether the method is POST; nil means the path authenticates.
			public func(method string) wireResponse
		}{
			{path: mode.activePath, public: func(method string) wireResponse {
				if method == http.MethodPost {
					return mode.loginResult
				}
				return plainNotFound
			}},
			// The decoded path equals the login path, so authentication is
			// skipped, but the escaped slash does not match the route.
			{path: escaped(mode.activePath), public: func(string) wireResponse { return plainNotFound }},
			{path: mode.activePath + "/"},
			{path: mode.inactivePath},
			{path: escaped(mode.inactivePath)},
		} {
			for _, method := range methods {
				for _, creds := range credentialCases {
					name := mode.mode + " " + method + " " + path.path + " " + creds.name
					t.Run(name, func(t *testing.T) {
						cfg := mode.cfg
						cfg.BlobDir = t.TempDir()
						hub := events.NewHub()
						server := httptest.NewServer(routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, hub,
							events.NewPublisher(database.Dialect(), hub)))
						defer server.Close()

						want := protected(creds)
						if path.public != nil {
							want = path.public(method)
						}
						if method == http.MethodHead {
							want.body = ""
						}

						request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path.path, strings.NewReader(mode.body))
						if err != nil {
							t.Fatal(err)
						}
						request.Header.Set("Content-Type", "application/json")
						if creds.cookie != "" {
							request.AddCookie(&http.Cookie{Name: "session", Value: creds.cookie})
						}
						if creds.authorization != "" {
							request.Header.Set("Authorization", creds.authorization)
						}
						response, err := server.Client().Do(request)
						if err != nil {
							t.Fatal(err)
						}
						defer response.Body.Close()
						body, err := io.ReadAll(response.Body)
						if err != nil {
							t.Fatal(err)
						}
						got := wireResponse{
							status:          response.StatusCode,
							contentType:     response.Header.Get("Content-Type"),
							wwwAuthenticate: response.Header.Get("WWW-Authenticate"),
							allow:           response.Header.Get("Allow"),
							setCookie:       response.Header.Get("Set-Cookie"),
							body:            string(body),
						}
						if got != want {
							t.Fatalf("response = %+v, want %+v", got, want)
						}
					})
				}
			}
		}
	}
}
