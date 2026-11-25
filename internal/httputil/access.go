package httputil

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
)

// LogAccess writes the access log line for a finished request, naming its chi
// route pattern, or its path when no route matched. It is the callback of
// hlog.AccessHandler.
func LogAccess(r *http.Request, status, size int, duration time.Duration) {
	route := ""
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		route = rctx.RoutePattern()
	}
	if route == "" {
		route = r.URL.Path
	}
	hlog.FromRequest(r).Info().
		Int("status", status).
		Int("size", size).
		Dur("duration", duration).
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Msgf("%s %s (%d)", r.Method, route, status)
}
