package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

func TestDevRoutesAbsentWithoutDevUI(t *testing.T) {
	handler := newOfflineServer(t, sdkmetric.NewManualReader()).Handler()
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/dev"},
		{http.MethodPost, "/dev/jobs/blob-gc"},
		{http.MethodPost, "/dev/jobs/account-purge"},
		{http.MethodPost, "/dev/jobs/reconciliation"},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(request.method, request.path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", request.method, request.path, w.Code)
		}
	}
}
