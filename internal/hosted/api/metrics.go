package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"futo-notes-server/internal/db"
)

// durationBuckets replaces the SDK's default boundaries, which are sized for
// milliseconds and would put every request recorded in seconds in the first
// bucket.
var durationBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120,
}

// Metrics are the API's instruments. Build them from a no-op meter provider
// when metrics are off; every recording is then free.
type Metrics struct {
	requestDuration otelmetric.Float64Histogram
	requestCount    otelmetric.Int64Counter
	requestErrors   otelmetric.Int64Counter
	logins          otelmetric.Int64Counter
	tokensIssued    otelmetric.Int64Counter
	tokenRefreshes  otelmetric.Int64Counter
	receiptFailures otelmetric.Int64Counter
	sseStreams      otelmetric.Int64UpDownCounter
	jobRuns         otelmetric.Int64Counter
}

// NewMetrics creates the API's instruments on meter, and observes database's
// connection pool.
func NewMetrics(meter otelmetric.Meter, database *db.DB) (*Metrics, error) {
	var m Metrics
	var err error
	if m.requestDuration, err = meter.Float64Histogram("http.server.request.duration",
		otelmetric.WithDescription("HTTP server request duration"), otelmetric.WithUnit("s"),
		otelmetric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, fmt.Errorf("creating request duration histogram: %w", err)
	}
	if m.requestCount, err = meter.Int64Counter("http.server.request.count",
		otelmetric.WithDescription("HTTP requests served")); err != nil {
		return nil, fmt.Errorf("creating request counter: %w", err)
	}
	if m.requestErrors, err = meter.Int64Counter("http.server.request.errors",
		otelmetric.WithDescription("HTTP requests answered with a 4xx or 5xx status")); err != nil {
		return nil, fmt.Errorf("creating request error counter: %w", err)
	}
	if m.logins, err = meter.Int64Counter("auth.logins",
		otelmetric.WithDescription("OIDC sign-ins by mode and outcome")); err != nil {
		return nil, fmt.Errorf("creating login counter: %w", err)
	}
	if m.tokensIssued, err = meter.Int64Counter("tokens.issued",
		otelmetric.WithDescription("Session, storage refresh and storage access tokens issued")); err != nil {
		return nil, fmt.Errorf("creating token counter: %w", err)
	}
	if m.tokenRefreshes, err = meter.Int64Counter("tokens.refreshes",
		otelmetric.WithDescription("Storage refresh token exchanges by outcome, reuse detection included")); err != nil {
		return nil, fmt.Errorf("creating refresh counter: %w", err)
	}
	if m.receiptFailures, err = meter.Int64Counter("receipts.verification_failures",
		otelmetric.WithDescription("Upload receipts rejected on object create or update")); err != nil {
		return nil, fmt.Errorf("creating receipt failure counter: %w", err)
	}
	if m.sseStreams, err = meter.Int64UpDownCounter("sse.streams",
		otelmetric.WithDescription("Open sync event streams")); err != nil {
		return nil, fmt.Errorf("creating SSE stream gauge: %w", err)
	}
	if m.jobRuns, err = meter.Int64Counter("jobs.runs",
		otelmetric.WithDescription("Background job runs by job and outcome")); err != nil {
		return nil, fmt.Errorf("creating job run counter: %w", err)
	}

	open, err := meter.Int64ObservableGauge("db.pool.connections",
		otelmetric.WithDescription("Database connections by state"))
	if err != nil {
		return nil, fmt.Errorf("creating pool connections gauge: %w", err)
	}
	waits, err := meter.Int64ObservableCounter("db.pool.waits",
		otelmetric.WithDescription("Times a query waited for a free connection"))
	if err != nil {
		return nil, fmt.Errorf("creating pool wait counter: %w", err)
	}
	waited, err := meter.Float64ObservableCounter("db.pool.wait_duration",
		otelmetric.WithDescription("Total time queries waited for a free connection"), otelmetric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("creating pool wait duration counter: %w", err)
	}
	inUse := otelmetric.WithAttributes(attribute.String("state", "in_use"))
	idle := otelmetric.WithAttributes(attribute.String("state", "idle"))
	_, err = meter.RegisterCallback(func(_ context.Context, o otelmetric.Observer) error {
		stats := database.Stats()
		o.ObserveInt64(open, int64(stats.InUse), inUse)
		o.ObserveInt64(open, int64(stats.Idle), idle)
		o.ObserveInt64(waits, stats.WaitCount)
		o.ObserveFloat64(waited, stats.WaitDuration.Seconds())
		return nil
	}, open, waits, waited)
	if err != nil {
		return nil, fmt.Errorf("registering pool stats callback: %w", err)
	}
	return &m, nil
}

// Login outcomes.
const (
	loginSuccess       = "success"
	loginInvalidState  = "invalid_state"
	loginProviderError = "provider_error"
	loginDenied        = "denied"
)

func (m *Metrics) login(ctx context.Context, app bool, outcome string) {
	mode := "dashboard"
	if app {
		mode = "app"
	}
	m.logins.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("mode", mode), attribute.String("outcome", outcome)))
}

// Token kinds.
const (
	tokenSession        = "session"
	tokenStorageRefresh = "storage_refresh"
	tokenStorageAccess  = "storage_access"
)

func (m *Metrics) issued(ctx context.Context, kind string) {
	m.tokensIssued.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("kind", kind)))
}

func (m *Metrics) refresh(ctx context.Context, outcome string) {
	m.tokenRefreshes.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) receiptFailure(ctx context.Context, reason string) {
	m.receiptFailures.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("reason", reason)))
}

// JobRun records one run of a background job.
func (m *Metrics) JobRun(ctx context.Context, job string, err error) {
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.jobRuns.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("job", job), attribute.String("outcome", outcome)))
}

// countStreams keeps the open-stream gauge while a sync event stream runs.
func (m *Metrics) countStreams(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.sseStreams.Add(r.Context(), 1)
		defer m.sseStreams.Add(context.WithoutCancel(r.Context()), -1)
		next(w, r)
	}
}

// statusRecorder notes the status a handler answers with. Unwrap keeps
// http.ResponseController reaching the underlying writer, which sync event
// streams flush through.
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

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// knownMethods are the methods recorded by name; clients can send any token.
var knownMethods = []string{
	http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodOptions,
	http.MethodPatch, http.MethodPost, http.MethodPut, http.MethodTrace,
}

// middleware records request duration, count and errors by method, route
// pattern and status. Routes are chi patterns, never raw paths, so
// cardinality stays bounded.
func (m *Metrics) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		route := "unmatched"
		if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
			route = rctx.RoutePattern()
		}
		method := r.Method
		if !slices.Contains(knownMethods, method) {
			method = "other"
		}
		attrs := otelmetric.WithAttributes(attribute.String("method", method), attribute.String("route", route),
			attribute.Int("status", status))
		ctx := context.WithoutCancel(r.Context())
		m.requestDuration.Record(ctx, time.Since(start).Seconds(), attrs)
		m.requestCount.Add(ctx, 1, attrs)
		if status >= http.StatusBadRequest {
			m.requestErrors.Add(ctx, 1, attrs)
		}
	})
}
