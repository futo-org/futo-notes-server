// Package metrics records the storage service's OTLP metrics. A nil *Metrics
// is valid and records nothing, which is what runs when no OTLP endpoint is
// configured.
package metrics

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// ServiceName is the OTel service.name and meter name.
const ServiceName = "notes-storage"

// SetupMeterProvider returns a provider pushing to endpoint (host:port) every
// interval. version and region identify the process on every series.
func SetupMeterProvider(endpoint, urlPath string, interval time.Duration, version, region string) (*sdkmetric.MeterProvider, error) {
	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpoint(endpoint),
		otlpmetrichttp.WithInsecure(),
	}
	if urlPath != "" {
		opts = append(opts, otlpmetrichttp.WithURLPath(urlPath))
	}
	exporter, err := otlpmetrichttp.New(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP metric exporter: %w", err)
	}
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(resource(version, region)),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(interval))),
	), nil
}

// resource identifies this process. Without service.instance.id every replica
// exports identical series; the TSDB merges them, interleaved cumulative
// counters read as resets, and rate() reports a fraction of the real traffic.
func resource(version, region string) *sdkresource.Resource {
	host, _ := os.Hostname()
	res, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewSchemaless(
		attribute.String("service.name", ServiceName),
		attribute.String("service.version", version),
		attribute.String("service.instance.id", host),
		attribute.String("region", region),
	))
	if err != nil {
		return sdkresource.Default()
	}
	return res
}

// Metrics holds the service's instruments.
type Metrics struct {
	requestDuration otelmetric.Float64Histogram
	requestCount    otelmetric.Int64Counter
	requestErrors   otelmetric.Int64Counter
	uploadedBytes   otelmetric.Int64Counter
	downloadedBytes otelmetric.Int64Counter
	s3Duration      otelmetric.Float64Histogram
	s3Errors        otelmetric.Int64Counter
	authFailures    otelmetric.Int64Counter
	receiptsSigned  otelmetric.Int64Counter
}

// durationBuckets replaces the SDK defaults, which are sized for
// milliseconds: recording seconds against them puts every sub-5s request in
// the first bucket.
var durationBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120,
}

// NewMetrics creates the instruments on meter.
func NewMetrics(meter otelmetric.Meter) (*Metrics, error) {
	var m Metrics
	var err error
	if m.requestDuration, err = meter.Float64Histogram("http.server.request.duration",
		otelmetric.WithDescription("HTTP server request duration"),
		otelmetric.WithUnit("s"),
		otelmetric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, fmt.Errorf("creating request duration histogram: %w", err)
	}
	if m.requestCount, err = meter.Int64Counter("http.server.request.count",
		otelmetric.WithDescription("HTTP requests")); err != nil {
		return nil, fmt.Errorf("creating request count counter: %w", err)
	}
	if m.requestErrors, err = meter.Int64Counter("http.server.request.errors",
		otelmetric.WithDescription("HTTP requests answered 4xx or 5xx")); err != nil {
		return nil, fmt.Errorf("creating request errors counter: %w", err)
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
		otelmetric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
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

// AddUploaded counts n bytes stored.
func (m *Metrics) AddUploaded(ctx context.Context, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.uploadedBytes.Add(ctx, n)
}

// AddDownloaded counts n blob bytes sent.
func (m *Metrics) AddDownloaded(ctx context.Context, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.downloadedBytes.Add(ctx, n)
}

// ObserveS3 implements storage.Observer.
func (m *Metrics) ObserveS3(ctx context.Context, operation string, duration time.Duration, failed bool) {
	if m == nil {
		return
	}
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
	if m == nil {
		return
	}
	m.authFailures.Add(ctx, 1, cached(&authAttrs, [2]string{kind, reason}, func() attribute.Set {
		return attribute.NewSet(attribute.String("kind", kind), attribute.String("reason", reason))
	}))
}

// ReceiptSigned counts a signed receipt.
func (m *Metrics) ReceiptSigned(ctx context.Context) {
	if m == nil {
		return
	}
	m.receiptsSigned.Add(ctx, 1)
}

// Measurement options are cached: building an attribute set on every request
// allocates on the hot path. Labels stay bounded (no user ids): a hosted
// region serves every user, and per-user series would grow without limit.
var (
	operationAttrs sync.Map
	authAttrs      sync.Map
	httpAttrs      sync.Map
)

func cached[K comparable](cache *sync.Map, key K, build func() attribute.Set) otelmetric.MeasurementOption {
	if v, ok := cache.Load(key); ok {
		return v.(otelmetric.MeasurementOption)
	}
	opt := otelmetric.WithAttributeSet(build())
	cache.Store(key, opt)
	return opt
}

type httpKey struct {
	method, route string
	status        int
}

// httpOption labels the HTTP instruments by route pattern, never raw path.
func httpOption(method, route string, status int) otelmetric.MeasurementOption {
	return cached(&httpAttrs, httpKey{method, route, status}, func() attribute.Set {
		return attribute.NewSet(
			attribute.String("method", method),
			attribute.String("route", route),
			attribute.Int("status", status),
		)
	})
}
