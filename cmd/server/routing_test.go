package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/dbtest"
	"futo-notes-server/internal/events"
)

// routingResponse is a response as the handler chain produced it, before the
// HTTP server adds Date and Content-Length.
type routingResponse struct {
	status int
	header http.Header
	body   string
}

func (r routingResponse) equal(other routingResponse) bool {
	return r.status == other.status && r.body == other.body &&
		maps.EqualFunc(r.header, other.header, slices.Equal)
}

// unpinnedHeaders are response headers the middleware chain may add for
// observability. They are not part of the client contract, and the comparison
// harness does not check them.
var unpinnedHeaders = []string{"X-Request-Id"}

// flushCancelingRecorder ends the request as soon as the handler flushes, so
// a sync event stream returns after its ready event.
type flushCancelingRecorder struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w flushCancelingRecorder) Flush() {
	w.ResponseRecorder.Flush()
	w.cancel()
}

func serveRouting(t *testing.T, handler http.Handler, method, target, bearer, body string) routingResponse {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(flushCancelingRecorder{recorder, cancel}, request)
	result := recorder.Result()
	defer result.Body.Close()
	header := result.Header.Clone()
	for _, name := range unpinnedHeaders {
		header.Del(name)
	}
	return routingResponse{status: result.StatusCode, header: header, body: recorder.Body.String()}
}

func textHeader(contentType string, extra ...string) http.Header {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	for i := 0; i < len(extra); i += 2 {
		header.Set(extra[i], extra[i+1])
	}
	return header
}

var (
	// muxNotFound is net/http's NotFound, answered outside /api/.
	muxNotFound = routingResponse{
		status: http.StatusNotFound,
		header: textHeader("text/plain; charset=utf-8", "X-Content-Type-Options", "nosniff"),
		body:   "404 page not found\n",
	}
	// apiNotFound is the /api/ catch-all, reached only after authentication.
	apiNotFound = routingResponse{
		status: http.StatusNotFound,
		header: textHeader("text/plain; charset=utf-8"),
		body:   "404 Not Found",
	}
	unauthorizedRouting = routingResponse{
		status: http.StatusUnauthorized,
		header: textHeader("application/json"),
		body:   "{\"error\":\"unauthorized\"}\n",
	}
	invalidSessionRouting = routingResponse{
		status: http.StatusUnauthorized,
		header: textHeader("application/json",
			"WWW-Authenticate", `Bearer realm="futo-notes", error="invalid_token"`),
		body: "{\"code\":\"invalid_session\",\"error\":\"session expired or invalid\"}\n",
	}
	// asteriskResponse answers a request-target of "*" (RFC 9110 §7.1).
	asteriskResponse = routingResponse{
		status: http.StatusBadRequest,
		header: textHeader("", "Connection", "close"),
	}
)

func methodNotAllowed(allow string) routingResponse {
	return routingResponse{
		status: http.StatusMethodNotAllowed,
		header: textHeader("text/plain; charset=utf-8", "Allow", allow, "X-Content-Type-Options", "nosniff"),
		body:   "Method Not Allowed\n",
	}
}

// temporaryRedirect is the 307 for a request whose path is not canonical. GET
// and HEAD get an HTML content type; only GET gets the HTML body.
func temporaryRedirect(method, location, htmlLocation string) routingResponse {
	response := routingResponse{status: http.StatusTemporaryRedirect, header: textHeader("", "Location", location)}
	if method == http.MethodGet || method == http.MethodHead {
		response.header.Set("Content-Type", "text/html; charset=utf-8")
	}
	if method == http.MethodGet {
		response.body = "<a href=\"" + htmlLocation + "\">Temporary Redirect</a>.\n\n"
	}
	return response
}

func redirectTo(method, location string) routingResponse {
	return temporaryRedirect(method, location, location)
}

// routingMethods are the methods every path is tried with: the standard ones,
// an extension method, and a standard name in the wrong case.
var routingMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
	http.MethodDelete, http.MethodOptions, http.MethodTrace, http.MethodConnect,
	"PROPFIND", "get",
}

// apiRoute is one path under /api/ and the methods registered for it.
type apiRoute struct {
	path    string
	methods []string
	// getStatus is the status GET answers with a valid session, for paths
	// with a GET route.
	getStatus int
}

// TestRoutingPinned pins how the router answers everything other than a
// handler's own output: wrong methods, HEAD, OPTIONS, unknown paths, path
// cleaning redirects, trailing and doubled slashes, dot segments,
// percent-encoding, the exact-match root and the authentication boundary, in
// both auth modes and with the dev UI on and off.
func TestRoutingPinned(t *testing.T) {
	database := dbtest.SQLite(t)
	user, err := auth.UpsertUserByEmail(t.Context(), database, "routing@example.invalid", "Routing")
	if err != nil {
		t.Fatal(err)
	}
	validToken, err := auth.CreateSession(t.Context(), database, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	// A real collection and blob, so that GET routes reached through an
	// alternative spelling answer differently from a miss.
	blobDir := t.TempDir()
	setupHub := events.NewHub()
	setup := routes(config.Config{AuthMode: config.AuthModeDev, BlobDir: blobDir}, database,
		&blobs.Store{Dir: blobDir}, setupHub, events.NewPublisher(database.Dialect(), setupHub))
	var claimed struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	if response := serveRouting(t, setup, http.MethodPost, "/api/collections", validToken, ""); response.status != http.StatusCreated ||
		json.Unmarshal([]byte(response.body), &claimed) != nil {
		t.Fatalf("claiming a collection = %d %q", response.status, response.body)
	}
	var uploaded struct {
		Key string `json:"key"`
	}
	if response := serveRouting(t, setup, http.MethodPost, "/api/blobs", validToken, "ciphertext"); response.status != http.StatusCreated ||
		json.Unmarshal([]byte(response.body), &uploaded) != nil {
		t.Fatalf("uploading a blob = %d %q", response.status, response.body)
	}
	collectionID := claimed.Collection.ID
	const (
		objectID   = "0190f5c4-8f6e-7d2a-9b1c-3d4e5f607183"
		mutationID = "0190f5c4-8f6e-7d2a-9b1c-3d4e5f607184"
	)
	_, blobID, _ := strings.Cut(uploaded.Key, "/")
	collection := "/api/collections/" + collectionID
	apiRoutes := []apiRoute{
		{path: "/api/auth", methods: []string{http.MethodGet}, getStatus: http.StatusOK},
		{path: "/api/auth/logout", methods: []string{http.MethodPost}},
		{path: "/api/collections", methods: []string{http.MethodGet, http.MethodPost}, getStatus: http.StatusOK},
		{path: collection, methods: []string{http.MethodGet, http.MethodDelete}, getStatus: http.StatusOK},
		{path: collection + "/key", methods: []string{http.MethodGet, http.MethodPut}, getStatus: http.StatusOK},
		{path: collection + "/objects", methods: []string{http.MethodGet, http.MethodPost}, getStatus: http.StatusOK},
		{path: collection + "/objects/" + objectID, methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}, getStatus: http.StatusNotFound},
		{path: collection + "/blob-objects", methods: []string{http.MethodPost}},
		{path: collection + "/blob-objects/" + objectID, methods: []string{http.MethodPut}},
		// PUT here reaches the {objectId} route with objectId "batch".
		{path: collection + "/blob-objects/batch", methods: []string{http.MethodPost, http.MethodPut}},
		{path: collection + "/create-mutations/" + mutationID, methods: []string{http.MethodGet}, getStatus: http.StatusNotFound},
		{path: "/api/blobs", methods: []string{http.MethodPost}},
		{path: "/api/blobs/batch", methods: []string{http.MethodPost}},
		{path: "/api/blobs/" + user.ID + "/" + blobID, methods: []string{http.MethodGet, http.MethodDelete}, getStatus: http.StatusOK},
		{path: "/api/sync/events", methods: []string{http.MethodGet}, getStatus: http.StatusOK},
	}

	type credentials struct {
		name   string
		bearer string
		// authenticated maps what an authenticated request gets to what this
		// credential gets once requireAuth has run.
		authenticated func(routingResponse) routingResponse
	}
	credentialCases := []credentials{
		{name: "no session", authenticated: func(routingResponse) routingResponse { return unauthorizedRouting }},
		{name: "invalid session", bearer: "not-a-session", authenticated: func(routingResponse) routingResponse { return invalidSessionRouting }},
		{name: "valid session", bearer: validToken, authenticated: func(r routingResponse) routingResponse { return r }},
	}

	for _, mode := range []struct {
		name        string
		cfg         config.Config
		loginPath   string
		loginResult routingResponse
	}{
		{
			name:      "dev",
			cfg:       config.Config{AuthMode: config.AuthModeDev},
			loginPath: devLoginPath,
			loginResult: routingResponse{
				status: http.StatusBadRequest, header: textHeader("application/json"),
				body: "{\"error\":\"email is required\"}\n",
			},
		},
		{
			name:      "password",
			cfg:       config.Config{AuthMode: config.AuthModePassword, Password: "correct password"},
			loginPath: passwordLoginPath,
			loginResult: routingResponse{
				status: http.StatusBadRequest, header: textHeader("application/json"),
				body: "{\"error\":\"password is required\"}\n",
			},
		},
	} {
		for _, devUI := range []bool{false, true} {
			cfg := mode.cfg
			cfg.DevUI = devUI
			cfg.BlobDir = blobDir
			hub := events.NewHub()
			handler := routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, hub, events.NewPublisher(database.Dialect(), hub))

			prefix := mode.name
			if devUI {
				prefix += " dev-ui"
			}
			serve := func(method, target, bearer, body string) routingResponse {
				return serveRouting(t, handler, method, target, bearer, body)
			}
			expect := func(method, target, bearer string, want routingResponse) {
				t.Helper()
				if got := serve(method, target, bearer, ""); !got.equal(want) {
					t.Errorf("%s: %s %s (bearer %q)\n got %d %v %q\nwant %d %v %q", prefix, method, target, bearer,
						got.status, got.header, got.body, want.status, want.header, want.body)
				}
			}

			// The capability document answers only the exact root.
			capability := routingResponse{
				status: http.StatusOK,
				header: textHeader("application/json"),
				body: `{"name":"futo-notes","version":"0.1.0","auth_mode":"` + mode.name +
					`","signup":"closed","billing":false,"mutation_ids":{"supported":true,"required":false,` +
					`"retention_days":30,"successful_create_outcomes":"durable"}}` + "\n",
			}
			health := routingResponse{
				status: http.StatusOK,
				header: textHeader("application/json"),
				body:   "{\"status\":\"ok\",\"db\":\"connected\"}\n",
			}
			for _, method := range routingMethods {
				switch method {
				case http.MethodGet, http.MethodHead:
					expect(method, "/", "", capability)
					expect(method, "/?x=1", "", capability)
					// A segment that decodes to "/" matches like a trailing
					// slash, so this is the root.
					expect(method, "/%2F", "", capability)
					expect(method, "/%2f", "", capability)
					expect(method, "/health", "", health)
					expect(method, "/health?x=1", "", health)
					expect(method, "/%68ealth", "", health)
				default:
					expect(method, "/", "", methodNotAllowed("GET, HEAD"))
					expect(method, "/%2F", "", methodNotAllowed("GET, HEAD"))
					expect(method, "/health", "", methodNotAllowed("GET, HEAD"))
				}
				for _, target := range []string{
					"/x", "/x/", "/x/y", "/index.html", "/favicon.ico", "/healthz", "/%2F/health",
					"/health/", "/HEALTH", "/health%2F", "/API/auth", "/Api/", "/apix", "/apis/auth",
					"/api%2F", "/api%2Fauth", "/%61pi%2F",
				} {
					expect(method, target, "", muxNotFound)
				}

				// Dev UI routes exist only when it is enabled, and the clean-path
				// redirect runs before a route is chosen either way.
				if devUI {
					switch method {
					case http.MethodGet, http.MethodHead:
						for _, target := range []string{
							"/dev/panic", "/dev/jobs/sessions", "/dev/jobs/reconciliation",
							"/dev/jobs/mutation-results", "/dev/jobs/blob-gc",
						} {
							expect(method, target, "", methodNotAllowed("POST"))
						}
					case http.MethodPost:
						expect(method, "/dev", "", methodNotAllowed("GET, HEAD"))
					default:
						expect(method, "/dev", "", methodNotAllowed("GET, HEAD"))
						for _, target := range []string{
							"/dev/panic", "/dev/jobs/sessions", "/dev/jobs/reconciliation",
							"/dev/jobs/mutation-results", "/dev/jobs/blob-gc",
						} {
							expect(method, target, "", methodNotAllowed("POST"))
						}
					}
				} else {
					for _, target := range []string{"/dev", "/dev/panic", "/dev/jobs/sessions", "/dev/jobs/blob-gc"} {
						expect(method, target, "", muxNotFound)
					}
				}
				for _, target := range []string{"/dev/", "/dev/jobs", "/dev/jobs/", "/dev/jobs/unknown"} {
					expect(method, target, "", muxNotFound)
				}

				if method == http.MethodConnect {
					continue
				}
				// Non-canonical paths outside /api/ redirect to the cleaned path,
				// whatever the method and whether or not a route matches it.
				for target, location := range map[string]string{
					"//":                  "/",
					"/.":                  "/",
					"/./":                 "/",
					"/..":                 "/",
					"/../":                "/",
					"/x/..":               "/",
					"//health":            "/health",
					"/./health":           "/health",
					"/x/../health":        "/health",
					"/api/../health":      "/health",
					"/health/.":           "/health",
					"/health//":           "/health/",
					"/health/./":          "/health/",
					"//x":                 "/x",
					"//dev":               "/dev",
					"/dev//panic":         "/dev/panic",
					"//health?x=1":        "/health?x=1",
					"/x/../?a=1":          "/?a=1",
					"/health/x/..?q=a%2F": "/health?q=a%2F",
				} {
					expect(method, target, "", redirectTo(method, location))
				}
				expect(method, "//health?a=1&b=2", "", temporaryRedirect(method, "/health?a=1&b=2", "/health?a=1&amp;b=2"))

				// /api without its trailing slash redirects to the subtree, before
				// authentication.
				for target, location := range map[string]string{
					"/api":       "/api/",
					"/api?x=1":   "/api/?x=1",
					"//api":      "/api/",
					"/x/../api":  "/api/",
					"/api//":     "/api/",
					"/%61pi":     "/%61pi/",
					"/./api?x=1": "/api/?x=1",
				} {
					for _, creds := range credentialCases {
						expect(method, target, creds.bearer, redirectTo(method, location))
					}
				}
			}

			// A request-target of "*" is rejected before routing.
			for _, method := range []string{http.MethodGet, http.MethodOptions, http.MethodPost} {
				expect(method, "*", "", asteriskResponse)
			}

			// CONNECT paths are routed without cleaning, but /api still gets its
			// trailing-slash redirect.
			expect(http.MethodConnect, "/api", "", redirectTo(http.MethodConnect, "/api/"))
			expect(http.MethodConnect, "//health", "", muxNotFound)
			expect(http.MethodConnect, "//api/auth", validToken, muxNotFound)
			expect(http.MethodConnect, "/./health", "", muxNotFound)
			for _, creds := range credentialCases {
				expect(http.MethodConnect, "/api/../health", creds.bearer, creds.authenticated(apiNotFound))
				expect(http.MethodConnect, "/api//auth", creds.bearer, creds.authenticated(apiNotFound))
				expect(http.MethodConnect, "/api//", creds.bearer, creds.authenticated(apiNotFound))
				expect(http.MethodConnect, "/api/auth", creds.bearer, creds.authenticated(apiNotFound))
			}

			if devUI {
				get := serve(http.MethodGet, "/dev", "", "")
				if get.status != http.StatusOK || get.header.Get("Content-Type") != "text/html; charset=utf-8" {
					t.Errorf("%s: GET /dev = %d %v", prefix, get.status, get.header)
				}
				for _, target := range []string{"/dev?x=1", "/%64ev"} {
					expect(http.MethodGet, target, "", get)
				}
				expect(http.MethodHead, "/dev", "", get)
				expect(http.MethodPost, "/dev/panic", "", routingResponse{
					status: http.StatusInternalServerError,
					header: textHeader("application/json", "Connection", "close"),
					body:   "{\"error\":\"internal server error\"}\n",
				})
			}

			// Everything under /api/ authenticates first, except the active
			// login path, and then falls through to the plain-text catch-all.
			for _, creds := range credentialCases {
				for _, target := range []string{
					"/api/", "/api/x", "/api/x/", "/api/%2F", "/api/%2F/auth", "/api/AUTH", "/api/auth/dev",
					"/api/collections/%2F", "/api/collections/%2F/key", collection + "/%2F",
					"/api/blobs/%2F/" + blobID,
					"/api/collections/", "/api/sync", "/api/sync/events/", "/api/blobs/" + user.ID,
					"/api/blobs/" + user.ID + "/" + blobID + "/x", collection + "/objects/" + objectID + "/x",
					"/api/%2e%2e/health", "/api/.%2E/health",
				} {
					for _, method := range routingMethods {
						expect(method, target, creds.bearer, creds.authenticated(apiNotFound))
					}
				}
			}

			// The login path skips authentication even when its spelling is
			// percent-encoded, and the other mode's login path does not exist.
			for _, creds := range credentialCases {
				for _, target := range []string{mode.loginPath, strings.Replace(mode.loginPath, "/api/", "/%61pi/", 1)} {
					if got := serve(http.MethodPost, target, creds.bearer, "{}"); !got.equal(mode.loginResult) {
						t.Errorf("%s: POST %s (%s) = %d %q, want login result", prefix, target, creds.name, got.status, got.body)
					}
				}
			}

			for _, route := range apiRoutes {
				for _, creds := range credentialCases {
					// Methods without a route fall through to the catch-all.
					for _, method := range routingMethods {
						registered := slices.Contains(route.methods, method) ||
							method == http.MethodHead && slices.Contains(route.methods, http.MethodGet)
						if !registered {
							expect(method, route.path, creds.bearer, creds.authenticated(apiNotFound))
						}
					}
					// Trailing slashes are not cleaned and match no route.
					for _, method := range []string{http.MethodGet, http.MethodPost} {
						expect(method, route.path+"/", creds.bearer, creds.authenticated(apiNotFound))
					}
				}

				// Non-canonical spellings redirect before authentication, keeping
				// the query string.
				rest := strings.TrimPrefix(route.path, "/api/")
				for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete} {
					for _, target := range []string{
						"/" + route.path,
						"/api//" + rest,
						"/api/./" + rest,
						"/api/x/../" + rest,
						route.path + "/.",
						route.path + "/x/..",
						strings.ReplaceAll(route.path, "/", "//"),
					} {
						for _, creds := range credentialCases {
							expect(method, target, creds.bearer, redirectTo(method, route.path))
						}
					}
					expect(method, "/"+route.path+"?cursor=1&limit=2", "",
						temporaryRedirect(method, route.path+"?cursor=1&limit=2", route.path+"?cursor=1&amp;limit=2"))
				}

				if !slices.Contains(route.methods, http.MethodGet) {
					continue
				}
				get := serve(http.MethodGet, route.path, validToken, "")
				if get.status != route.getStatus {
					t.Errorf("%s: GET %s = %d %q, want status %d", prefix, route.path, get.status, get.body, route.getStatus)
				}
				// HEAD runs the GET handler; only http.ServeContent drops the body.
				head := serve(http.MethodHead, route.path, validToken, "")
				if head.status != get.status || (head.body != get.body && head.body != "") ||
					!(routingResponse{header: head.header}).equal(routingResponse{header: get.header}) {
					t.Errorf("%s: HEAD %s = %d %v %q, want GET's response %d %v %q", prefix, route.path,
						head.status, head.header, head.body, get.status, get.header, get.body)
				}
				// Percent-encoding any character of a segment routes as the
				// decoded path.
				variants := []string{
					route.path,
					route.path + "?unused=1",
					strings.Replace(route.path, "/api/", "/%61pi/", 1),
					strings.Replace(route.path, "/api/", "/%61%70%69/", 1),
					route.path[:len(route.path)-1] + "%" + strings.ToUpper(hexByte(route.path[len(route.path)-1])),
					route.path[:len(route.path)-1] + "%" + hexByte(route.path[len(route.path)-1]),
				}
				for _, creds := range credentialCases {
					for _, target := range variants {
						expect(http.MethodGet, target, creds.bearer, creds.authenticated(get))
						expect(http.MethodHead, target, creds.bearer, creds.authenticated(head))
					}
				}
			}

			// A literal segment registered for one method does not hide a
			// wildcard route registered for another.
			for _, creds := range credentialCases {
				expect(http.MethodPut, collection+"/blob-objects/batch", creds.bearer, creds.authenticated(routingResponse{
					status: http.StatusBadRequest, header: textHeader("application/json"),
					body: "{\"error\":\"invalid version\"}\n",
				}))
			}

			// A wildcard segment receives the decoded value, slashes included,
			// so these reach a route, which finds nothing. A literal segment
			// with an encoded slash in it matches nothing.
			jsonNotFound := routingResponse{
				status: http.StatusNotFound, header: textHeader("application/json"),
				body: "{\"error\":\"not found\"}\n",
			}
			for _, target := range []string{
				collection + "%2Fkey",
				"/api/collections/a%2Fb/objects",
				"/api/collections/a%2fb/objects",
				"/api/collections/%25/objects",
				"/api/blobs/" + user.ID + "/a%2Fb",
				"/api/blobs/" + user.ID + "%2F/" + blobID,
			} {
				for _, creds := range credentialCases {
					expect(http.MethodGet, target, creds.bearer, creds.authenticated(jsonNotFound))
				}
			}
			for _, creds := range credentialCases {
				for _, target := range []string{
					"/api/collections%2F" + collectionID,
					"/api/sync%2Fevents",
					"/api/blobs%2Fbatch",
					"/api/blobs/" + user.ID + "%2F" + blobID,
				} {
					expect(http.MethodGet, target, creds.bearer, creds.authenticated(apiNotFound))
					expect(http.MethodPost, target, creds.bearer, creds.authenticated(apiNotFound))
				}
			}
		}
	}
}

func hexByte(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0xf]})
}
