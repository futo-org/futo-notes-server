package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"go.opentelemetry.io/otel/metric/noop"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/hosted/api"
	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hosted/hostedtest"
	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/mockoidc"
)

const (
	clientID     = "notes-test"
	clientSecret = "notes-test-secret"
	appScheme    = "org.futo.notes.test"
	verifier     = "dBjftJeZ4CVP-mJ92K9YQ4WllYjv4Ac4lDmRVLz4xBxl0vpEqbr7iw"
)

// harness is a hosted API on an httptest server, signing users in through an
// in-process mock OIDC issuer, with two storage regions whose receipt keys
// the test holds.
type harness struct {
	t        *testing.T
	database *db.DB
	api      *httptest.Server
	issuer   *httptest.Server
	apiKeys  hostedauth.KeySet
	signers  map[string]*hostedauth.ReceiptSigner
	keys     map[string]hostedauth.SigningKey
	hub      *events.Hub
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	database := hostedtest.Postgres(t)
	h := &harness{t: t, database: database, signers: map[string]*hostedauth.ReceiptSigner{},
		keys: map[string]hostedauth.SigningKey{}, hub: events.NewHub()}

	h.api = httptest.NewUnstartedServer(nil)
	publicURL := "http://" + h.api.Listener.Addr().String()
	h.issuer = mockoidc.StartTestServer(t, mockoidc.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURIs: []string{publicURL + "/api/auth/oidc/callback"},
	})

	apiKey, err := hostedauth.GenerateSigningKey("test-api")
	if err != nil {
		t.Fatal(err)
	}
	apiPublic, err := apiKey.MarshalPublicPEM()
	if err != nil {
		t.Fatal(err)
	}
	if h.apiKeys, err = hostedauth.ParseKeySet(apiPublic); err != nil {
		t.Fatal(err)
	}
	var regions []config.Region
	for _, id := range []string{"us-east", "eu-central"} {
		key, err := hostedauth.GenerateSigningKey("test-" + id)
		if err != nil {
			t.Fatal(err)
		}
		public, err := key.MarshalPublicPEM()
		if err != nil {
			t.Fatal(err)
		}
		keys, err := hostedauth.ParseKeySet(public)
		if err != nil {
			t.Fatal(err)
		}
		h.keys[id] = key
		if h.signers[id], err = hostedauth.NewReceiptSigner(key, id); err != nil {
			t.Fatal(err)
		}
		regions = append(regions, config.Region{ID: id, Name: id + " name", Description: id + " description",
			Endpoint: "http://storage-" + id + ".test", ReceiptKeys: keys})
	}

	metrics, err := api.NewMetrics(noop.NewMeterProvider().Meter("test"), database)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(api.Options{
		Config: config.Config{
			PublicURL:          publicURL,
			OIDC:               config.OIDC{Issuer: h.issuer.URL, ClientID: clientID, ClientSecret: clientSecret},
			AppRedirectSchemes: []string{appScheme},
			Regions:            regions,
			SigningKey:         apiKey,
		},
		Database: database,
		Hub:      h.hub,
		Metrics:  metrics,
		Version:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.api.Config.Handler = server.Handler()
	h.api.Start()
	t.Cleanup(h.api.Close)
	return h
}

// client is a browser or app talking to the API: it keeps cookies, never
// follows redirects, and sends the bearer token when it has one.
type client struct {
	h      *harness
	http   *http.Client
	bearer string
}

func (h *harness) client() *client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return &client{h: h, http: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}}
}

type response struct {
	status int
	header http.Header
	body   string
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), v); err != nil {
		t.Fatalf("decoding %q: %v", r.body, err)
	}
}

func (c *client) do(method, target string, body any, headers ...string) response {
	c.h.t.Helper()
	if strings.HasPrefix(target, "/") {
		target = c.h.api.URL + target
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.h.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(c.h.t.Context(), method, target, reader)
	if err != nil {
		c.h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		c.h.t.Fatal(err)
	}
	return response{status: res.StatusCode, header: res.Header, body: string(data)}
}

func (c *client) expect(method, target string, body any, status int) response {
	c.h.t.Helper()
	res := c.do(method, target, body)
	if res.status != status {
		c.h.t.Fatalf("%s %s = %d %s, want %d", method, target, res.status, res.body, status)
	}
	return res
}

// authorize follows the API's redirect to the provider's authorize endpoint
// as the test shortcut does, signing in as sub, and returns the provider's
// redirect back to the API.
func (c *client) authorize(start response, sub string) string {
	c.h.t.Helper()
	if start.status != http.StatusFound {
		c.h.t.Fatalf("start = %d %s, want 302", start.status, start.body)
	}
	target, err := url.Parse(start.header.Get("Location"))
	if err != nil {
		c.h.t.Fatal(err)
	}
	if !strings.HasPrefix(target.String(), c.h.issuer.URL+"/authorize") {
		c.h.t.Fatalf("start redirects to %s, want the issuer", target)
	}
	target.Path = "/test/authorize"
	q := target.Query()
	q.Set("sub", sub)
	q.Set("email", sub+"@example.test")
	q.Set("name", "Test "+sub)
	target.RawQuery = q.Encode()
	res := c.do(http.MethodGet, target.String(), nil)
	if res.status != http.StatusFound {
		c.h.t.Fatalf("authorize = %d %s", res.status, res.body)
	}
	return res.header.Get("Location")
}

// newSub returns a fresh subject whose user is removed when the test ends.
func (h *harness) newSub() string {
	sub := "api-test-" + uuid.NewV7().String()
	dbtest.CleanupExec(h.t, h.database, `DELETE FROM users WHERE sub = $1`, sub)
	return sub
}

// dashboardLogin signs a new browser in through the dashboard flow.
func (h *harness) dashboardLogin(sub string) *client {
	h.t.Helper()
	c := h.client()
	callback := c.authorize(c.do(http.MethodGet, "/api/auth/oidc/start", nil), sub)
	res := c.do(http.MethodGet, callback, nil)
	if res.status != http.StatusSeeOther || res.header.Get("Location") != "/" {
		h.t.Fatalf("dashboard callback = %d %v %s", res.status, res.header, res.body)
	}
	return c
}

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// appCode runs the app flow up to the redirect back to the app and returns
// that redirect.
func (c *client) appCode(sub, redirectURI string) *url.URL {
	c.h.t.Helper()
	start := c.do(http.MethodGet, "/api/auth/oidc/start?"+url.Values{
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
		"state":                 {"app-state"},
	}.Encode(), nil)
	res := c.do(http.MethodGet, c.authorize(start, sub), nil)
	if res.status != http.StatusFound {
		c.h.t.Fatalf("app callback = %d %s", res.status, res.body)
	}
	back, err := url.Parse(res.header.Get("Location"))
	if err != nil {
		c.h.t.Fatal(err)
	}
	return back
}

type appTokens struct {
	Token               string `json:"token"`
	StorageRefreshToken string `json:"storage_refresh_token"`
	User                struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
}

// appLogin signs a new app in and returns it holding its bearer token.
func (h *harness) appLogin(sub string) (*client, appTokens) {
	h.t.Helper()
	c := h.client()
	back := c.appCode(sub, "http://127.0.0.1:43123/callback")
	var tokens appTokens
	c.expect(http.MethodPost, "/api/auth/oidc/token",
		map[string]string{"code": back.Query().Get("code"), "code_verifier": verifier}, http.StatusOK).json(h.t, &tokens)
	app := h.client()
	app.bearer = tokens.Token
	return app, tokens
}

// receipt signs a receipt in region for userID's upload of size bytes.
func (h *harness) receipt(region, userID string, size int64) (token, key string) {
	h.t.Helper()
	key = userID + "/" + uuid.NewV7().String()
	token, err := h.signers[region].Sign(userID, key, size)
	if err != nil {
		h.t.Fatal(err)
	}
	return token, key
}
