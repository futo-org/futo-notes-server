package handlers

import (
	"context"
	"net/http"
	"testing"
	"time"
	"uuid"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/storagesvc/metrics"
)

func TestMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := metrics.NewMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWithMetrics(t, m)

	var up UploadResponse
	e.upload(e.access(userA), []byte("12345")).json(t, &up)
	if resp := e.do(http.MethodGet, "/blobs/"+up.Key, e.access(userA), nil, nil); resp.status != http.StatusOK {
		t.Fatalf("get: %d", resp.status)
	}
	e.batch(e.access(userA), map[string][]string{"keys": {up.Key, userA + "/" + uuid.NewV7().String()}})
	expired, _, err := hostedauth.NewMinter(e.apiKey, hostedauth.WithClock(func() time.Time { return time.Now().Add(-48 * time.Hour) })).
		MintAccess(testRegion, userA)
	if err != nil {
		t.Fatal(err)
	}
	e.upload(expired, []byte("x"))
	e.do(http.MethodGet, "/internal/blobs", "", nil, nil)
	m.ObserveS3(context.Background(), "put", time.Millisecond, true)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	byName := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, metric := range sm.Metrics {
			byName[metric.Name] = metric.Data
		}
	}

	if got := sum(t, byName["storage.uploaded_bytes"]); got != 5 {
		t.Errorf("uploaded bytes %d", got)
	}
	if got := sum(t, byName["storage.downloaded_bytes"]); got != 10 {
		t.Errorf("downloaded bytes %d (get + batch)", got)
	}
	if got := sum(t, byName["storage.receipts.signed"]); got != 1 {
		t.Errorf("receipts %d", got)
	}
	if got := sum(t, byName["auth.failures"], attribute.String("kind", "access"), attribute.String("reason", "expired")); got != 1 {
		t.Errorf("expired access failures %d", got)
	}
	if got := sum(t, byName["auth.failures"], attribute.String("kind", "admin"), attribute.String("reason", "missing")); got != 1 {
		t.Errorf("missing admin failures %d", got)
	}
	if got := sum(t, byName["storage.s3.errors"], attribute.String("operation", "put")); got != 1 {
		t.Errorf("s3 put errors %d", got)
	}
	if got := sum(t, byName["http.server.request.count"], attribute.String("route", "/blobs/{userId}/{blobId}")); got != 1 {
		t.Errorf("get requests %d", got)
	}
	if got := sum(t, byName["http.server.request.errors"], attribute.String("route", "/blobs"), attribute.Int("status", 401)); got != 1 {
		t.Errorf("401 uploads %d", got)
	}

	for _, sm := range rm.ScopeMetrics {
		for _, metric := range sm.Metrics {
			for _, attrs := range dataPointAttributes(metric.Data) {
				if _, ok := attrs.Value("userId"); ok {
					t.Fatalf("%s carries a user id: %v", metric.Name, attrs)
				}
			}
		}
	}
}

// sum adds the data points of a counter that carry every attribute in want.
func sum(t *testing.T, data metricdata.Aggregation, want ...attribute.KeyValue) int64 {
	t.Helper()
	s, ok := data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("not an int64 sum: %T", data)
	}
	var total int64
	for _, dp := range s.DataPoints {
		matches := true
		for _, kv := range want {
			if v, ok := dp.Attributes.Value(kv.Key); !ok || v != kv.Value {
				matches = false
			}
		}
		if matches {
			total += dp.Value
		}
	}
	return total
}

func dataPointAttributes(data metricdata.Aggregation) []attribute.Set {
	var sets []attribute.Set
	switch d := data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	}
	return sets
}
