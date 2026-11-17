package api_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hosted/hostedtest"
)

const specFile = "../../../docs/openapi-hosted.yaml"

// specCoverage records every "operationId status" a checked response
// answered, across the package's tests.
var specCoverage = struct {
	sync.Mutex
	hit map[string]bool
}{hit: map[string]bool{}}

// uncoverable are documented responses no test can provoke: a 500 needs the
// database to fail mid-request.
var uncoverable = []string{"500"}

// TestMain fails a complete run of the package that leaves a documented
// response of docs/openapi-hosted.yaml unexercised. A filtered run, or one
// without the hosted test database, checks only what it ran.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 && os.Getenv(hostedtest.EnvVar) != "" &&
		flag.Lookup("test.run").Value.String() == "" && flag.Lookup("test.skip").Value.String() == "" {
		missing, err := uncoveredResponses()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		} else if len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "FAIL: no test received these responses of docs/openapi-hosted.yaml:\n  %s\n",
				strings.Join(missing, "\n  "))
			code = 1
		}
	}
	os.Exit(code)
}

func uncoveredResponses() ([]string, error) {
	s, err := loadedSpec()
	if err != nil {
		return nil, err
	}
	specCoverage.Lock()
	defer specCoverage.Unlock()
	var missing []string
	for path, item := range s.doc.Paths.Map() {
		for method, op := range item.Operations() {
			for status := range op.Responses.Map() {
				key := op.OperationID + " " + status
				if !specCoverage.hit[key] && !slices.Contains(uncoverable, status) {
					missing = append(missing, key+" ("+method+" "+path+")")
				}
			}
		}
	}
	sort.Strings(missing)
	return missing, nil
}

type spec struct {
	doc    *openapi3.T
	router routers.Router
}

// loadedSpec is the hosted API spec and a router over it, loaded once.
var loadedSpec = sync.OnceValues(func() (spec, error) {
	loader := openapi3.NewLoader()
	// The sync operations reference docs/openapi.yaml.
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromFile(specFile)
	if err != nil {
		return spec{}, fmt.Errorf("load %s: %w", specFile, err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return spec{}, fmt.Errorf("invalid %s: %w", specFile, err)
	}
	// The spec's server is a template; match on paths alone.
	doc.Servers = nil
	router, err := legacy.NewRouter(doc)
	return spec{doc: doc, router: router}, err
})

func loadSpec(t *testing.T) spec {
	t.Helper()
	s, err := loadedSpec()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// specCheck returns a transport that checks every response from the API
// against docs/openapi-hosted.yaml.
func (h *harness) specCheck() http.RoundTripper {
	return &specCheck{t: h.t, host: h.api.Listener.Addr().String(), router: loadSpec(h.t).router, next: http.DefaultTransport}
}

// specCheck validates every response the API sends a client against the
// spec: its status must be documented and its headers, content type and body
// must match. Requests are not validated, since tests send invalid ones on
// purpose. Requests to other hosts (the OIDC issuer, the app's redirect)
// and to paths the spec does not describe (the dashboard, removed routes)
// pass through: the route parity test keeps the latter honest.
type specCheck struct {
	t      *testing.T
	host   string
	router routers.Router
	next   http.RoundTripper
}

func (c *specCheck) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(req)
	if err != nil || req.URL.Host != c.host || req.Method == http.MethodHead {
		return resp, err
	}
	route, params, findErr := c.router.FindRoute(req)
	if findErr != nil {
		return resp, nil
	}
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
		Options:                &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	}
	var body []byte
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType == "text/event-stream" {
		// The stream stays open; check all but its body, which the tests
		// read.
		input.Options.ExcludeResponseBody = true
		if documented := route.Operation.Responses.Status(resp.StatusCode); documented == nil || documented.Value.Content.Get(mediaType) == nil {
			c.t.Errorf("%s %s -> %d %s is not documented in docs/openapi-hosted.yaml", req.Method, req.URL.Path, resp.StatusCode, mediaType)
		}
	} else {
		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		input.Body = io.NopCloser(bytes.NewReader(body))
	}
	if err := openapi3filter.ValidateResponse(req.Context(), input); err != nil {
		c.t.Errorf("%s %s -> %d does not match docs/openapi-hosted.yaml: %v\nbody: %.300s",
			req.Method, req.URL.Path, resp.StatusCode, err, body)
	}
	specCoverage.Lock()
	specCoverage.hit[fmt.Sprintf("%s %d", route.Operation.OperationID, resp.StatusCode)] = true
	specCoverage.Unlock()
	return resp, nil
}

// TestSpecDescribesEveryRoute checks that the spec's operations and its
// x-internal-routes list are exactly the routes the server mounts, so no
// route goes unchecked by specCheck and none is documented that is not
// served.
func TestSpecDescribesEveryRoute(t *testing.T) {
	doc := loadSpec(t).doc
	server := newOfflineServer(t, sdkmetric.NewManualReader(), func(c *config.Config) { c.DevUI = true })
	// /readyz is answered ahead of the router.
	routes := append(server.Routes(), "GET /readyz")

	var documented []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented = append(documented, method+" "+path)
		}
	}
	internal, ok := doc.Extensions["x-internal-routes"].([]any)
	if !ok {
		t.Fatalf("x-internal-routes = %#v, want a list", doc.Extensions["x-internal-routes"])
	}
	for _, route := range internal {
		route, _ := route.(string)
		if slices.Contains(documented, route) {
			t.Errorf("%s is both an operation and an internal route", route)
		}
		documented = append(documented, route)
	}

	for _, route := range routes {
		if !slices.Contains(documented, route) {
			t.Errorf("%s is served but not in docs/openapi-hosted.yaml", route)
		}
	}
	for _, route := range documented {
		if !slices.Contains(routes, route) {
			t.Errorf("%s is in docs/openapi-hosted.yaml but not served", route)
		}
	}
}
