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
		{http.MethodGet, "/dev/signup"},
		{http.MethodGet, "/dev/storekit"},
		{http.MethodPost, "/dev/jobs/blob-gc"},
		{http.MethodPost, "/dev/jobs/account-purge"},
		{http.MethodPost, "/dev/jobs/reconciliation"},
		{http.MethodPost, "/dev/jobs/pairing-sweep"},
		{http.MethodPost, "/dev/email/lapsed"},
		{http.MethodGet, "/dev/billing/checkout"},
		{http.MethodPost, "/dev/billing/checkout"},
		{http.MethodGet, "/dev/billing/portal"},
		{http.MethodPost, "/dev/billing/portal"},
		{http.MethodPost, "/dev/billing/events/grant"},
		{http.MethodPost, "/dev/billing/lapsed-at"},
		{http.MethodPost, "/dev/jobs/billing-reconciliation"},
		{http.MethodPost, "/dev/jobs/billing-lapses"},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(request.method, request.path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", request.method, request.path, w.Code)
		}
	}
}
