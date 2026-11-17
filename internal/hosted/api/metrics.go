package api

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/hostedmetrics"
)

// Metrics are the API's instruments. Build them from a no-op meter provider
// when metrics are off; every recording is then free.
type Metrics struct {
	http            *hostedmetrics.HTTP
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
	if m.http, err = hostedmetrics.NewHTTP(meter); err != nil {
		return nil, err
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
	// loginNotReauthenticated is a reauthentication the provider answered
	// with a stale auth_time.
	loginNotReauthenticated = "not_reauthenticated"
	// loginSignupNotAllowed is a new account ALLOWED_EMAIL_DOMAINS refused.
	loginSignupNotAllowed = "signup_not_allowed"
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
