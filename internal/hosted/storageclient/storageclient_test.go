package storageclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/storagesvc/adminapi"
)

const (
	region = "us-east"
	user   = "0190a2b3-0000-7000-8000-00000000000a"
)

// fake is a storage service whose admin endpoints answer with handle, after
// checking the admin token.
type fake struct {
	t        *testing.T
	verifier *hostedauth.Verifier
	calls    atomic.Int32
	handle   func(w http.ResponseWriter, r *http.Request, call int)
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := int(f.calls.Add(1))
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || f.verifier.VerifyAdmin(token) != nil {
		f.t.Errorf("call %d: bad admin token %q", call, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.handle(w, r, call)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newClient returns a client for region served by a fake running handle,
// with fast retries.
func newClient(t *testing.T, meter *sdkmetric.MeterProvider, handle func(w http.ResponseWriter, r *http.Request, call int)) (*Client, *fake) {
	t.Helper()
	key, err := hostedauth.GenerateSigningKey("test-api")
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
	verifier, err := hostedauth.NewVerifier(keys, region)
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{t: t, verifier: verifier, handle: handle}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	m := noop.NewMeterProvider().Meter("test")
	if meter != nil {
		m = meter.Meter("test")
	}
	c, err := New([]config.Region{{ID: region, Endpoint: srv.URL}}, hostedauth.NewMinter(key), m)
	if err != nil {
		t.Fatal(err)
	}
	c.backoff = time.Millisecond
	c.timeout = 5 * time.Second
	return c, f
}

func TestListBlobsPagesUntilTheCursorIsNull(t *testing.T) {
	olderThan := time.Date(2026, 11, 14, 12, 0, 0, 500, time.UTC)
	pages := []string{
		`{"blobs":[{"key":"k1","size":1,"last_modified":"2026-11-01T00:00:00Z"},{"key":"k2","size":2,"last_modified":"2026-11-01T00:00:00Z"}],"next_cursor":"k2"}`,
		`{"blobs":[],"next_cursor":"k5"}`,
		`{"blobs":[],"next_cursor":"k7"}`,
		`{"blobs":[{"key":"k9","size":9,"last_modified":"2026-11-01T00:00:00Z"}],"next_cursor":null}`,
	}
	wantCursors := []string{"", "k2", "k5", "k7"}
	c, f := newClient(t, nil, func(w http.ResponseWriter, r *http.Request, call int) {
		i := (call - 1) % len(pages)
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/internal/blobs" || q.Get("cursor") != wantCursors[i] ||
			q.Get("older_than") != "2026-11-14T12:00:00.0000005Z" || q.Has("prefix") {
			t.Errorf("call %d: %s %s", call, r.Method, r.URL)
		}
		_, _ = w.Write([]byte(pages[i]))
	})
	var got []string
	pagesSeen := 0
	err := c.ListBlobs(t.Context(), region, "", olderThan, func(blobs []adminapi.Blob) error {
		pagesSeen++
		for _, b := range blobs {
			got = append(got, b.Key)
		}
		return nil
	})
	if err != nil || strings.Join(got, ",") != "k1,k2,k9" || pagesSeen != 2 || f.calls.Load() != 4 {
		t.Fatalf("listed %v in %d pages over %d calls, err %v", got, pagesSeen, f.calls.Load(), err)
	}

	stuck, _ := newClient(t, nil, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`{"blobs":[],"next_cursor":"same"}`))
	})
	if err := stuck.ListBlobs(t.Context(), region, "", olderThan, func([]adminapi.Blob) error { return nil }); err == nil {
		t.Fatal("a cursor that never advances listed forever or succeeded")
	}

	stop := errors.New("stop")
	if err := c.ListBlobs(t.Context(), region, "", olderThan, func([]adminapi.Blob) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("page error = %v, want it returned", err)
	}
}

func TestListBlobsSendsThePrefix(t *testing.T) {
	const prefix = "01a1e744-1d44-7ff3-83a1-e2dcd2e62496/"
	c, f := newClient(t, nil, func(w http.ResponseWriter, r *http.Request, call int) {
		if got := r.URL.Query().Get("prefix"); got != prefix {
			t.Errorf("call %d: prefix %q, want %q", call, got, prefix)
		}
		_, _ = w.Write([]byte(`{"blobs":[],"next_cursor":null}`))
	})
	if err := c.ListBlobs(t.Context(), region, prefix, time.Now(), func([]adminapi.Blob) error { return nil }); err != nil || f.calls.Load() != 1 {
		t.Fatalf("listing a prefix: %d calls, err %v", f.calls.Load(), err)
	}
}

func TestDeleteBlobsSendsAtMostAThousandKeysPerCall(t *testing.T) {
	var sizes []int
	c, _ := newClient(t, nil, func(w http.ResponseWriter, r *http.Request, call int) {
		var body struct {
			Keys []string `json:"keys"`
		}
		if r.Method != http.MethodDelete || r.URL.Path != "/internal/blobs" || json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Errorf("call %d: %s %s", call, r.Method, r.URL)
		}
		sizes = append(sizes, len(body.Keys))
		writeJSON(w, map[string]any{"deleted": len(body.Keys) - 1, "failed": []adminapi.KeyFailure{{Key: body.Keys[0], Message: "denied"}}})
	})
	keys := make([]string, 2500)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s/%04d", user, i)
	}
	failed, err := c.DeleteBlobs(t.Context(), region, keys)
	if err != nil || fmt.Sprint(sizes) != "[1000 1000 500]" || len(failed) != 3 ||
		failed[0].Key != keys[0] || failed[1].Key != keys[1000] || failed[2].Key != keys[2000] || failed[0].Message != "denied" {
		t.Fatalf("calls %v, failed %+v, err %v", sizes, failed, err)
	}
}

func TestPurgeUserCallsUntilDone(t *testing.T) {
	answers := []string{`{"deleted":1000,"done":false}`, `{"deleted":3,"done":true}`}
	c, f := newClient(t, nil, func(w http.ResponseWriter, r *http.Request, call int) {
		if r.Method != http.MethodDelete || r.URL.Path != "/internal/users/"+user {
			t.Errorf("call %d: %s %s", call, r.Method, r.URL)
		}
		_, _ = w.Write([]byte(answers[call-1]))
	})
	if deleted, err := c.PurgeUser(t.Context(), region, user); err != nil || deleted != 1003 || f.calls.Load() != 2 {
		t.Fatalf("purge = %d, %v over %d calls", deleted, err, f.calls.Load())
	}

	stalled, _ := newClient(t, nil, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`{"deleted":0,"done":false}`))
	})
	if _, err := stalled.PurgeUser(t.Context(), region, user); err == nil {
		t.Fatal("a purge that makes no progress looped forever or succeeded")
	}
}

func TestCallsRetryTransientFailuresOnly(t *testing.T) {
	c, f := newClient(t, nil, func(w http.ResponseWriter, _ *http.Request, call int) {
		if call < 3 {
			w.WriteHeader([]int{http.StatusServiceUnavailable, http.StatusTooManyRequests}[call-1])
			return
		}
		_, _ = w.Write([]byte(`{"deleted":0,"done":true}`))
	})
	if _, err := c.PurgeUser(t.Context(), region, user); err != nil || f.calls.Load() != 3 {
		t.Fatalf("purge after two transient failures = %v over %d calls", err, f.calls.Load())
	}

	down, f := newClient(t, nil, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := down.PurgeUser(t.Context(), region, user)
	if statusErr, ok := errors.AsType[*StatusError](err); !ok || statusErr.Status != http.StatusInternalServerError || f.calls.Load() != attempts {
		t.Fatalf("purge against a failing service = %v over %d calls, want %d attempts", err, f.calls.Load(), attempts)
	}

	refused, f := newClient(t, nil, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadRequest)
	})
	if _, err := refused.PurgeUser(t.Context(), region, user); err == nil || f.calls.Load() != 1 {
		t.Fatalf("purge refused with 400 = %v over %d calls, want no retry", err, f.calls.Load())
	}

	if _, err := c.PurgeUser(t.Context(), "nowhere", user); err == nil {
		t.Fatal("a call to an unconfigured region succeeded")
	}
}

func TestEachAttemptTimesOut(t *testing.T) {
	c, f := newClient(t, nil, func(w http.ResponseWriter, r *http.Request, _ int) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			_, _ = w.Write([]byte(`{"deleted":0,"done":true}`))
		}
	})
	c.timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := c.PurgeUser(t.Context(), region, user)
	if !errors.Is(err, context.DeadlineExceeded) || f.calls.Load() != attempts || time.Since(start) > 3*time.Second {
		t.Fatalf("purge against a hung service = %v over %d calls in %s", err, f.calls.Load(), time.Since(start))
	}
}

func TestCallsAreRecordedByRegionAndOperation(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	c, _ := newClient(t, provider, func(w http.ResponseWriter, _ *http.Request, call int) {
		if call == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"deleted":0,"done":true}`))
	})
	if _, err := c.PurgeUser(t.Context(), region, user); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	want := attribute.NewSet(attribute.String("operation", opPurgeUser), attribute.String("region", region))
	counts := map[string]int64{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					if !point.Attributes.Equals(&want) {
						t.Errorf("%s recorded with %v", m.Name, point.Attributes.ToSlice())
					}
					counts[m.Name] += point.Value
				}
			case metricdata.Histogram[float64]:
				for _, point := range data.DataPoints {
					counts[m.Name] += int64(point.Count)
				}
			}
		}
	}
	if counts["storage_client.calls"] != 2 || counts["storage_client.errors"] != 1 || counts["storage_client.duration"] != 2 {
		t.Fatalf("recorded %v", counts)
	}
}
