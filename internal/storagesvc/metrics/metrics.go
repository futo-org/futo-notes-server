// Package metrics records the storage service's OTLP metrics. Build them from
// a no-op meter provider when metrics are off; every recording is then free.
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"futo-notes-server/internal/hostedmetrics"
)

// ServiceName is the OTel service.name and meter name.
const ServiceName = "notes-storage"

// Metrics holds the service's instruments.
type Metrics struct {
	http            *hostedmetrics.HTTP
	uploadedBytes   otelmetric.Int64Counter
	downloadedBytes otelmetric.Int64Counter
	s3Duration      otelmetric.Float64Histogram
	s3Errors        otelmetric.Int64Counter
	authFailures    otelmetric.Int64Counter
	receiptsSigned  otelmetric.Int64Counter
}

// NewMetrics creates the instruments on meter.
func NewMetrics(meter otelmetric.Meter) (*Metrics, error) {
	var m Metrics
	var err error
	if m.http, err = hostedmetrics.NewHTTP(meter); err != nil {
		return nil, err
	}
	if m.uploadedBytes, err = meter.Int64Counter("storage.uploaded_bytes",
		otelmetric.WithDescription("Blob bytes stored by uploads"),
		otelmetric.WithUnit("By")); err != nil {
		return nil, fmt.Errorf("creating uploaded bytes counter: %w", err)
	}
	if m.downloadedBytes, err = meter.Int64Counter("storage.downloaded_bytes",
		otelmetric.WithDescription("Blob bytes sent by downloads and batches"),
		otelmetric.WithUnit("By")); err != nil {
		return nil, fmt.Errorf("creating downloaded bytes counter: %w", err)
	}
	if m.s3Duration, err = meter.Float64Histogram("storage.s3.duration",
		otelmetric.WithDescription("S3 call duration, until the response headers"),
		otelmetric.WithUnit("s"),
		otelmetric.WithExplicitBucketBoundaries(hostedmetrics.DurationBuckets...)); err != nil {
		return nil, fmt.Errorf("creating s3 duration histogram: %w", err)
	}
	// Split out of the HTTP error count so a 5xx wave can be pinned on the
	// bucket rather than on this service or on auth.
	if m.s3Errors, err = meter.Int64Counter("storage.s3.errors",
		otelmetric.WithDescription("S3 calls that failed, by operation")); err != nil {
		return nil, fmt.Errorf("creating s3 errors counter: %w", err)
	}
	if m.authFailures, err = meter.Int64Counter("auth.failures",
		otelmetric.WithDescription("Requests rejected for a missing or invalid token, by token kind and reason")); err != nil {
		return nil, fmt.Errorf("creating auth failures counter: %w", err)
	}
	if m.receiptsSigned, err = meter.Int64Counter("storage.receipts.signed",
		otelmetric.WithDescription("Upload receipts signed")); err != nil {
		return nil, fmt.Errorf("creating receipts counter: %w", err)
	}
	return &m, nil
}

// Middleware records HTTP duration, count and errors by chi route pattern.
func (m *Metrics) Middleware(next http.Handler) http.Handler { return m.http.Middleware(next) }

// AddUploaded counts n bytes stored.
func (m *Metrics) AddUploaded(ctx context.Context, n int64) {
	if n > 0 {
		m.uploadedBytes.Add(ctx, n)
	}
}

// AddDownloaded counts n blob bytes sent.
func (m *Metrics) AddDownloaded(ctx context.Context, n int64) {
	if n > 0 {
		m.downloadedBytes.Add(ctx, n)
	}
}

// ObserveS3 implements storage.Observer.
func (m *Metrics) ObserveS3(ctx context.Context, operation string, duration time.Duration, failed bool) {
	opt := cached(&operationAttrs, operation, func() attribute.Set {
		return attribute.NewSet(attribute.String("operation", operation))
	})
	m.s3Duration.Record(ctx, duration.Seconds(), opt)
	if failed {
		m.s3Errors.Add(ctx, 1, opt)
	}
}

// AuthFailure counts a rejected token. It matches auth.OnFailure.
func (m *Metrics) AuthFailure(ctx context.Context, kind, reason string) {
	m.authFailures.Add(ctx, 1, cached(&authAttrs, [2]string{kind, reason}, func() attribute.Set {
		return attribute.NewSet(attribute.String("kind", kind), attribute.String("reason", reason))
	}))
}

// ReceiptSigned counts a signed receipt.
func (m *Metrics) ReceiptSigned(ctx context.Context) {
	m.receiptsSigned.Add(ctx, 1)
}

// Measurement options are cached: building an attribute set on every request
// allocates on the hot path. Labels stay bounded (no user ids): a hosted
// region serves every user, and per-user series would grow without limit.
var (
	operationAttrs sync.Map
	authAttrs      sync.Map
)

func cached[K comparable](cache *sync.Map, key K, build func() attribute.Set) otelmetric.MeasurementOption {
	if v, ok := cache.Load(key); ok {
		return v.(otelmetric.MeasurementOption)
	}
	opt := otelmetric.WithAttributeSet(build())
	cache.Store(key, opt)
	return opt
}
