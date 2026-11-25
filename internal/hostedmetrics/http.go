package hostedmetrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// HTTP records request duration, count and errors.
type HTTP struct {
	duration otelmetric.Float64Histogram
	count    otelmetric.Int64Counter
	errors   otelmetric.Int64Counter
	// attrs caches measurement options by label values: building an
	// attribute set on every request allocates on the hot path.
	attrs sync.Map
}

// NewHTTP creates the HTTP instruments on meter.
func NewHTTP(meter otelmetric.Meter) (*HTTP, error) {
	var h HTTP
	var err error
	if h.duration, err = meter.Float64Histogram("http.server.request.duration",
		otelmetric.WithDescription("HTTP server request duration"), otelmetric.WithUnit("s"),
		otelmetric.WithExplicitBucketBoundaries(DurationBuckets...)); err != nil {
		return nil, fmt.Errorf("creating request duration histogram: %w", err)
	}
	if h.count, err = meter.Int64Counter("http.server.request.count",
		otelmetric.WithDescription("HTTP requests served")); err != nil {
		return nil, fmt.Errorf("creating request counter: %w", err)
	}
	if h.errors, err = meter.Int64Counter("http.server.request.errors",
		otelmetric.WithDescription("HTTP requests answered with a 4xx or 5xx status")); err != nil {
		return nil, fmt.Errorf("creating request error counter: %w", err)
	}
	return &h, nil
}

// knownMethods are the methods recorded by name; clients can send any token.
var knownMethods = []string{
	http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodOptions,
	http.MethodPatch, http.MethodPost, http.MethodPut, http.MethodTrace,
}

// unmatched is the route label of requests no route matched.
const unmatched = "unmatched"

type labels struct {
	method, route string
	status        int
}

// Middleware records every request by method, chi route pattern and status.
// Labels stay bounded: routes are patterns, never raw paths, other methods
// read "other", and nothing names a user.
func (h *HTTP) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		l := labels{method: r.Method, route: unmatched, status: recorder.status}
		if l.status == 0 {
			l.status = http.StatusOK
		}
		if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
			l.route = rctx.RoutePattern()
		}
		if !slices.Contains(knownMethods, l.method) {
			l.method = "other"
		}
		opt := h.option(l)
		ctx := context.WithoutCancel(r.Context())
		h.duration.Record(ctx, time.Since(start).Seconds(), opt)
		h.count.Add(ctx, 1, opt)
		if l.status >= http.StatusBadRequest {
			h.errors.Add(ctx, 1, opt)
		}
	})
}

func (h *HTTP) option(l labels) otelmetric.MeasurementOption {
	if v, ok := h.attrs.Load(l); ok {
		return v.(otelmetric.MeasurementOption)
	}
	opt := otelmetric.WithAttributeSet(attribute.NewSet(
		attribute.String("method", l.method),
		attribute.String("route", l.route),
		attribute.Int("status", l.status),
	))
	h.attrs.Store(l, opt)
	return opt
}

// statusRecorder notes the status a handler answers with. It keeps
// io.ReaderFrom, so streamed bodies still take the sendfile/splice path, and
// Unwrap, so http.ResponseController reaches the underlying writer that sync
// event streams flush through.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, r)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
