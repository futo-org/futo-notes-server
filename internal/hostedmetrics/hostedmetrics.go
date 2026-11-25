// Package hostedmetrics is the OTLP metrics plumbing the hosted API and the
// storage service share: the meter provider, and the HTTP request
// instruments with their middleware.
package hostedmetrics

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// DurationBuckets replaces the SDK's default boundaries, which are sized for
// milliseconds and would put every duration recorded in seconds in the first
// bucket.
var DurationBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120,
}

// Export is where metrics are pushed.
type Export struct {
	// Endpoint is host:port, reached over plain HTTP; empty turns metrics
	// off.
	Endpoint string
	URLPath  string
	Interval time.Duration
}

// Provider is a meter provider that flushes on Shutdown.
type Provider interface {
	otelmetric.MeterProvider
	Shutdown(context.Context) error
}

type noopProvider struct{ noop.MeterProvider }

func (noopProvider) Shutdown(context.Context) error { return nil }

// NewProvider returns a provider pushing to export, or a no-op one when
// export has no endpoint, on which every recording is free. service and
// version, plus attrs, identify the process on every series.
func NewProvider(export Export, service, version string, attrs ...attribute.KeyValue) (Provider, error) {
	if export.Endpoint == "" {
		return noopProvider{}, nil
	}
	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(export.Endpoint), otlpmetrichttp.WithInsecure()}
	if export.URLPath != "" {
		opts = append(opts, otlpmetrichttp.WithURLPath(export.URLPath))
	}
	exporter, err := otlpmetrichttp.New(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP metric exporter: %w", err)
	}
	host, _ := os.Hostname()
	// Without service.instance.id every replica exports identical series;
	// the TSDB merges them, interleaved cumulative counters read as resets,
	// and rate() reports a fraction of the real traffic.
	resource, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewSchemaless(append([]attribute.KeyValue{
		attribute.String("service.name", service),
		attribute.String("service.version", version),
		attribute.String("service.instance.id", host),
	}, attrs...)...))
	if err != nil {
		resource = sdkresource.Default()
	}
	log.Info().Str("endpoint", export.Endpoint).Msg("OpenTelemetry metrics enabled")
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(resource),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(export.Interval))),
	), nil
}
