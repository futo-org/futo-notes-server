package metrics

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"futo-notes-server/internal/storagesvc/auth"
)

// userCapture carries the verified user out of the authenticated route group
// to Middleware, which wraps the whole router (it must see auth failures too)
// and so never sees the auth context on its own request.
type userCapture struct {
	user string
}

type captureKey struct{}

// CaptureUser hands the request's verified user to Middleware. Mount it after
// auth.Access.
func CaptureUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, _ := r.Context().Value(captureKey{}).(*userCapture); c != nil {
			c.user = auth.UserID(r.Context())
		}
		next.ServeHTTP(w, r)
	})
}

// Middleware records HTTP duration, count and errors by chi route pattern.
func Middleware(m *Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := &statusWriter{ResponseWriter: w}
			capture := &userCapture{}
			r = r.WithContext(context.WithValue(r.Context(), captureKey{}, capture))

			next.ServeHTTP(ww, r)

			status := ww.status
			if status == 0 {
				status = http.StatusOK
			}
			route := "unknown"
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				if p := routePattern(rctx); p != "" {
					route = p
				}
			}
			m.requestDuration.Record(r.Context(), time.Since(start).Seconds(), httpOption(r.Method, route, status))
			countOpt := httpUserOption(r.Method, route, status, capture.user)
			m.requestCount.Add(r.Context(), 1, countOpt)
			if status >= 400 {
				m.requestErrors.Add(r.Context(), 1, countOpt)
			}
		})
	}
}

var routePatterns sync.Map

func routePattern(rctx *chi.Context) string {
	key := strings.Join(rctx.RoutePatterns, "\x00")
	if v, ok := routePatterns.Load(key); ok {
		return v.(string)
	}
	p := rctx.RoutePattern()
	routePatterns.Store(key, p)
	return p
}

// statusWriter records the status code. It keeps io.ReaderFrom so streamed
// bodies still take the sendfile/splice path, and Unwrap for
// http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, r)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
