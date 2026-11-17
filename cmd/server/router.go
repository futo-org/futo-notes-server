package main

import (
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
)

// The router is chi, but clients were served by http.ServeMux, and its answers
// to unusual requests are part of the contract (TestRoutingPinned). The
// functions below reproduce the ServeMux behavior chi lacks: the "*"
// request-target, path cleaning and trailing-slash redirects, per-segment
// percent-decoding, routing of methods chi does not know, and the Allow list
// and body of a 405.

// matchServeMux answers what ServeMux answered before choosing a route, and
// otherwise sets the path and method chi routes on.
func matchServeMux(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "*" {
			if r.ProtoAtLeast(1, 1) {
				w.Header().Set("Connection", "close")
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		escaped := r.URL.EscapedPath()
		path := escaped
		// ServeMux leaves CONNECT paths as sent.
		if r.Method != http.MethodConnect {
			path = cleanPath(escaped)
		}
		if isAPISubtreeRoot(path) {
			http.Redirect(w, r, urlFromEscaped(path+"/", r.URL.RawQuery).String(), http.StatusTemporaryRedirect)
			return
		}
		if path != escaped {
			http.Redirect(w, r, urlFromEscaped(path, r.URL.RawQuery).String(), http.StatusTemporaryRedirect)
			return
		}

		rctx := chi.RouteContext(r.Context())
		rctx.RoutePath = routingPath(path)
		// ServeMux routes any method token, but chi answers a method it does
		// not know with 405 before looking at the path. No route accepts
		// TRACE, so routing such a request as TRACE reaches the same answer:
		// the /api/ subtree, or 404 or 405 by path.
		if !slices.Contains(chiMethods, r.Method) {
			rctx.RouteMethod = http.MethodTrace
		}
		next.ServeHTTP(w, r)
	})
}

// chiMethods are the methods chi routes without chi.RegisterMethod, sorted as
// ServeMux sorted its Allow header.
var chiMethods = []string{
	http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodOptions,
	http.MethodPatch, http.MethodPost, http.MethodPut, http.MethodTrace,
}

// isAPISubtreeRoot reports whether path is a spelling of /api, which ServeMux
// redirected to /api/ because only the /api/ subtree is registered.
func isAPISubtreeRoot(path string) bool {
	segment, ok := strings.CutPrefix(path, "/")
	if !ok || segment == "" || strings.Contains(segment, "/") {
		return false
	}
	decoded, err := url.PathUnescape(segment)
	return err == nil && decoded == "api"
}

// cleanPath is net/http's cleanPath: path.Clean that keeps a trailing slash.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// urlFromEscaped is net/http's urlFromEscaped: the redirect target for an
// escaped path, keeping its escaping.
func urlFromEscaped(escaped, rawQuery string) *url.URL {
	unescaped, err := url.PathUnescape(escaped)
	if err != nil {
		unescaped = escaped
	}
	return &url.URL{Path: unescaped, RawPath: escaped, RawQuery: rawQuery}
}

// segmentEscaper keeps a decoded segment in one piece and decodable.
var segmentEscaper = strings.NewReplacer("%", "%25", "/", "%2F")

// routingPath is the path chi matches. ServeMux decoded each segment before
// comparing it, so "/%61pi/auth" is /api/auth, while "%2F" stays inside its
// segment. A last segment that decodes to "/" matched as a trailing slash, so
// "/%2F" is the root.
func routingPath(escaped string) string {
	segments := strings.Split(escaped, "/")
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			continue
		}
		if decoded == "/" && i == len(segments)-1 {
			decoded = ""
		}
		segments[i] = segmentEscaper.Replace(decoded)
	}
	return strings.Join(segments, "/")
}

// decodePathValues gives handlers each path wildcard decoded, as ServeMux
// did. A segment that decodes to "/" matched no ServeMux wildcard, so such a
// request reaches the /api/ catch-all instead.
func decodePathValues(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		params := chi.RouteContext(r.Context()).URLParams
		for i, key := range params.Keys {
			if key == "*" {
				continue
			}
			// routingPath escaped the value, so it always decodes.
			value, _ := url.PathUnescape(params.Values[i])
			if value == "/" {
				apiCatchAll(w, r)
				return
			}
			r.SetPathValue(key, value)
		}
		next.ServeHTTP(w, r)
	})
}

// serveMuxMethodNotAllowed is ServeMux's 405, listing every method routed for the
// path or for the path with a trailing slash.
func serveMuxMethodNotAllowed(router chi.Routes) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		routePath := chi.RouteContext(r.Context()).RoutePath
		var allowed []string
		for _, method := range chiMethods {
			if router.Match(chi.NewRouteContext(), method, routePath) ||
				!strings.HasSuffix(routePath, "/") && router.Match(chi.NewRouteContext(), method, routePath+"/") {
				allowed = append(allowed, method)
			}
		}
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	}
}

// apiCatchAll answers every /api/ request no route matches, after
// authentication.
func apiCatchAll(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("404 Not Found"))
}

// handleGet routes GET and HEAD to h, as a ServeMux "GET" pattern did.
func handleGet(r chi.Router, pattern string, h http.HandlerFunc) {
	r.Get(pattern, h)
	r.Head(pattern, h)
}

// logAccess writes the access log line for a finished request.
func logAccess(r *http.Request, status, size int, duration time.Duration) {
	event := hlog.FromRequest(r).Info().
		Int("status", status).
		Int("size", size).
		Dur("duration", duration).
		Str("method", r.Method).
		Str("path", r.URL.Path)
	if route := chi.RouteContext(r.Context()).RoutePattern(); route != "" {
		event = event.Str("route", route)
	}
	event.Msg("request")
}
