package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	databasepkg "futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"uuid"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestRoutesComposePublicAuthAndRecoveryMiddleware(t *testing.T) {
	databaseURL := os.Getenv("SERVER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SERVER_TEST_DATABASE_URL is not set")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := databasepkg.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		AuthMode:     "dev",
		CookieSecure: false,
		DevUI:        true,
		BlobDir:      t.TempDir(),
	}
	server := httptest.NewServer(routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, events.NewHub()))
	defer server.Close()
	client := serverClient(server)

	for _, path := range []string{"/", "/health"} {
		response := doRequest(t, client, http.MethodGet, server.URL+path, "", "", nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body = %q", path, response.StatusCode, response.Body)
		}
	}

	response := doRequest(t, client, http.MethodGet, server.URL+"/api/auth", "", "", nil)
	assertResponse(t, response, http.StatusUnauthorized, "{\"error\":\"unauthorized\"}\n")

	response = doRequest(t, client, http.MethodGet, server.URL+"/api/auth", "", "garbage", nil)
	assertResponse(t, response, http.StatusUnauthorized, "{\"code\":\"invalid_session\",\"error\":\"session expired or invalid\"}\n")
	if got := response.Header.Get("WWW-Authenticate"); got != `Bearer realm="futo-notes", error="invalid_token"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}

	email := "routes-" + uuid.NewV7().String() + "@example.invalid"
	response = doRequest(t, client, http.MethodPost, server.URL+"/api/auth/dev/login",
		`{"email":"`+email+`","name":"Routes Test"}`, "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dev login status = %d, want 200; body = %q", response.StatusCode, response.Body)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(response.Body), &login); err != nil {
		t.Fatalf("decoding dev login: %v", err)
	}
	if login.Token == "" {
		t.Fatal("dev login returned an empty token")
	}
	var sessionCookie *http.Cookie
	for _, cookie := range response.Cookies {
		if cookie.Name == "session" {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil || sessionCookie.Value != login.Token {
		t.Fatalf("session cookie = %#v, want token from login response", sessionCookie)
	}

	response = doRequest(t, client, http.MethodGet, server.URL+"/api/auth", "", "", sessionCookie)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("cookie auth status = %d, want 200; body = %q", response.StatusCode, response.Body)
	}
	response = doRequest(t, client, http.MethodGet, server.URL+"/api/auth", "", login.Token, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("bearer auth status = %d, want 200; body = %q", response.StatusCode, response.Body)
	}

	response = doRequest(t, client, http.MethodPost, server.URL+"/api/auth/logout", "", login.Token, nil)
	assertResponse(t, response, http.StatusNoContent, "")
	response = doRequest(t, client, http.MethodGet, server.URL+"/api/auth", "", login.Token, nil)
	assertResponse(t, response, http.StatusUnauthorized, "{\"code\":\"invalid_session\",\"error\":\"session expired or invalid\"}\n")

	response = doRequest(t, client, http.MethodPost, server.URL+"/dev/panic", "", "", nil)
	assertResponse(t, response, http.StatusInternalServerError, "{\"error\":\"internal server error\"}\n")
	if !response.Close {
		t.Fatal("panic response did not close the connection")
	}
}

func TestRoutesComposePasswordLoginRateLimit(t *testing.T) {
	databaseURL := os.Getenv("SERVER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SERVER_TEST_DATABASE_URL is not set")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := databasepkg.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		AuthMode:     "password",
		Password:     "correct password",
		CookieSecure: false,
		BlobDir:      t.TempDir(),
	}
	server := httptest.NewServer(routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, events.NewHub()))
	defer server.Close()
	client := serverClient(server)

	for attempt := 1; attempt <= 10; attempt++ {
		response := doRequest(t, client, http.MethodPost, server.URL+"/api/auth/password/login",
			`{"password":"wrong password"}`, "", nil)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401; body = %q", attempt, response.StatusCode, response.Body)
		}
	}
	response := doRequest(t, client, http.MethodPost, server.URL+"/api/auth/password/login",
		`{"password":"wrong password"}`, "", nil)
	assertResponse(t, response, http.StatusTooManyRequests, "{\"error\":\"too many requests\"}\n")
	if response.Header.Get("Retry-After") == "" {
		t.Fatal("429 response has no Retry-After header")
	}
}

type routeTestResponse struct {
	StatusCode int
	Header     http.Header
	Body       string
	Cookies    []*http.Cookie
	Close      bool
}

func serverClient(server *httptest.Server) *http.Client {
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func doRequest(t *testing.T, client *http.Client, method, url, body, bearer string, cookie *http.Cookie) routeTestResponse {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return routeTestResponse{
		StatusCode: response.StatusCode,
		Header:     response.Header.Clone(),
		Body:       string(responseBody),
		Cookies:    response.Cookies(),
		Close:      response.Close,
	}
}

func assertResponse(t *testing.T, got routeTestResponse, wantStatus int, wantBody string) {
	t.Helper()
	if got.StatusCode != wantStatus || got.Body != wantBody {
		t.Fatalf("response = %d %q, want %d %q", got.StatusCode, got.Body, wantStatus, wantBody)
	}
}
