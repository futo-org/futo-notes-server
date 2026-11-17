package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/hosted/api"
	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hostedauth"
)

// newOfflineServer builds a server that never reaches its database or
// identity provider, for the routes that need neither, with configure
// applied to its configuration.
func newOfflineServer(t *testing.T, reader sdkmetric.Reader, configure ...func(*config.Config)) *api.Server {
	t.Helper()
	// Opening connects to nothing until a query runs.
	database, err := db.Open("sqlite:"+t.TempDir()+"/unused.db", db.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	metrics, err := api.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"), database)
	if err != nil {
		t.Fatal(err)
	}
	key, err := hostedauth.GenerateSigningKey("test")
	if err != nil {
		t.Fatal(err)
	}
	public, err := key.MarshalPublicPEM()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := hostedauth.ParseKeySet(public)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		PublicURL:  "http://api.test",
		OIDC:       config.OIDC{Issuer: "http://id.test", ClientID: "c", ClientSecret: "s"},
		Regions:    []config.Region{{ID: "us-east", Name: "US", Endpoint: "http://s.test", ReceiptKeys: keys}},
		SigningKey: key,
	}
	for _, c := range configure {
		c(&cfg)
	}
	server, err := api.New(api.Options{
		Config:   cfg,
		Database: database,
		Hub:      events.NewHub(),
		Metrics:  metrics,
		Version:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestReadinessFailsOnceDraining(t *testing.T) {
	server := newOfflineServer(t, sdkmetric.NewManualReader())
	handler := server.Handler()
	status := func() int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return w.Code
	}
	if got := status(); got != http.StatusOK {
		t.Fatalf("readyz = %d, want 200", got)
	}
	server.BeginDrain()
	if got := status(); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz while draining = %d, want 503", got)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/regions", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("regions while draining = %d, want still served", w.Code)
	}
}

func TestRequestMetricsUseRoutePatterns(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	handler := newOfflineServer(t, reader).Handler()
	for _, path := range []string{"/api/regions", "/api/blobs/anything", "/readyz"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("BREW", "/api/regions", nil))

	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "http.server.request.count" {
				continue
			}
			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				method, _ := point.Attributes.Value(attribute.Key("method"))
				route, _ := point.Attributes.Value(attribute.Key("route"))
				status, _ := point.Attributes.Value(attribute.Key("status"))
				counts[method.AsString()+" "+route.AsString()+" "+status.String()] += point.Value
			}
		}
	}
	want := map[string]int64{
		"GET /api/regions 200": 1,
		// Unknown paths and methods record a bounded pattern, never the
		// path or method the client sent.
		"GET /api/* 404":      1,
		"other unmatched 405": 1,
	}
	if len(counts) != len(want) {
		t.Fatalf("request counts = %v, want %v", counts, want)
	}
	for key, n := range want {
		if counts[key] != n {
			t.Fatalf("request counts = %v, want %v", counts, want)
		}
	}
}
