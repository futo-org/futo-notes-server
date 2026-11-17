package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/storagesvc/httputil"
	"futo-notes-server/internal/storagesvc/metrics"
	"futo-notes-server/internal/storagesvc/storage"
)

const (
	testRegion = "us-east"
	userA      = "0190a2b3-0000-7000-8000-00000000000a"
	userB      = "0190a2b3-0000-7000-8000-00000000000b"
)

// env is a storage service on a real listener, with keys generated for the
// test and a client that checks every response against the OpenAPI spec.
type env struct {
	t *testing.T
	// store is the fake behind server, or nil when it runs on real S3.
	store    *memStorage
	server   *Server
	url      string
	client   *http.Client
	apiKey   hostedauth.SigningKey
	minter   *hostedauth.Minter
	receipts *hostedauth.ReceiptVerifier
}

func newEnv(t *testing.T) *env {
	return newEnvWith(t, newMemStorage(), nil)
}

func newEnvWithMetrics(t *testing.T, m *metrics.Metrics) *env {
	return newEnvWith(t, newMemStorage(), m)
}

func newEnvWith(t *testing.T, store storage.Storage, m *metrics.Metrics) *env {
	t.Helper()
	apiKey := mustKey(t, "api-test")
	regionKey := mustKey(t, "region-test")
	verifier, err := hostedauth.NewVerifier(keySet(t, apiKey), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := hostedauth.NewReceiptSigner(regionKey, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := hostedauth.NewReceiptVerifier(map[string]hostedauth.KeySet{testRegion: keySet(t, regionKey)})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t:        t,
		apiKey:   apiKey,
		minter:   hostedauth.NewMinter(apiKey),
		receipts: receipts,
	}
	e.store, _ = store.(*memStorage)
	e.server = &Server{Storage: store, Verifier: verifier, Signer: signer, Metrics: m}
	srv := httptest.NewServer(e.server.Handler())
	t.Cleanup(srv.Close)
	e.url = srv.URL
	e.client = &http.Client{Transport: &specCheck{t: t, router: specRouter(t), next: http.DefaultTransport}}
	return e
}

func mustKey(t *testing.T, kid string) hostedauth.SigningKey {
	t.Helper()
	key, err := hostedauth.GenerateSigningKey(kid)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func keySet(t *testing.T, keys ...hostedauth.SigningKey) hostedauth.KeySet {
	t.Helper()
	var pemData []byte
	for _, k := range keys {
		pub, err := k.MarshalPublicPEM()
		if err != nil {
			t.Fatal(err)
		}
		pemData = append(pemData, pub...)
	}
	set, err := hostedauth.ParseKeySet(pemData)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func (e *env) access(userID string) string {
	e.t.Helper()
	token, _, err := e.minter.MintAccess(testRegion, userID)
	if err != nil {
		e.t.Fatal(err)
	}
	return token
}

func (e *env) admin() string {
	e.t.Helper()
	token, err := e.minter.MintAdmin(testRegion)
	if err != nil {
		e.t.Fatal(err)
	}
	return token
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// errorMessage checks the error body shape and returns its message.
func (r response) errorMessage(t *testing.T) string {
	t.Helper()
	var body httputil.ErrorResponse
	r.json(t, &body)
	if body.StatusCode != r.status || body.RequestID == "" || body.RequestID != r.header.Get("X-Request-Id") {
		t.Fatalf("error body %s for status %d, X-Request-Id %q", r.body, r.status, r.header.Get("X-Request-Id"))
	}
	return body.Message
}

func (e *env) do(method, path, token string, body io.Reader, header map[string]string) response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.url+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("%s %s: reading body: %v", method, path, err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: data}
}

func (e *env) upload(token string, data []byte) response {
	e.t.Helper()
	return e.do(http.MethodPost, "/blobs", token, bytes.NewReader(data), nil)
}

func (e *env) postJSON(path, token string, v any) response {
	e.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do(http.MethodPost, path, token, bytes.NewReader(data), map[string]string{"Content-Type": "application/json"})
}

func (e *env) deleteJSON(path, token string, v any) response {
	e.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do(http.MethodDelete, path, token, bytes.NewReader(data), map[string]string{"Content-Type": "application/json"})
}

func specRouter(t *testing.T) routers.Router {
	t.Helper()
	doc := loadSpec(t)
	// The spec's server is a template; match on paths alone.
	doc.Servers = nil
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../docs/openapi-storage.yaml")
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("invalid spec: %v", err)
	}
	return doc
}

// specCheck validates every response that reaches the client against the
// OpenAPI spec: its status must be documented and its headers and body must
// match. Requests are not validated, since tests send invalid ones on
// purpose.
type specCheck struct {
	t      *testing.T
	router routers.Router
	next   http.RoundTripper
}

func (c *specCheck) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	// Paths the spec does not describe are checked by the tests that send
	// them.
	route, params, findErr := c.router.FindRoute(req)
	if findErr != nil {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    req,
			PathParams: params,
			Route:      route,
		},
		Status:  resp.StatusCode,
		Header:  resp.Header,
		Body:    io.NopCloser(bytes.NewReader(body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	}
	if err := openapi3filter.ValidateResponse(req.Context(), input); err != nil {
		c.t.Errorf("%s %s -> %d does not match docs/openapi-storage.yaml: %v\nbody: %.300s",
			req.Method, req.URL.Path, resp.StatusCode, err, body)
	}
	return resp, nil
}
