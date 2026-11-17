package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"futo-notes-server/internal/blobframe"
	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/storagesvc/metrics"
)

// uploadBatch posts body as a batch upload.
func (e *env) uploadBatch(token string, body []byte) response {
	e.t.Helper()
	return e.do(http.MethodPost, "/blobs/batch-upload", token, bytes.NewReader(body),
		map[string]string{"Content-Type": "application/octet-stream"})
}

// batchResult is a BatchUploadResult with its body left encoded.
type batchResult struct {
	ID     string          `json:"id"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func (r response) batchResults(t *testing.T) []batchResult {
	t.Helper()
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("batch upload = %d %q: %s", r.status, r.header.Get("Content-Type"), r.body)
	}
	var body struct {
		Results []batchResult `json:"results"`
	}
	r.json(t, &body)
	return body.Results
}

func TestBatchUploadStoresEachFrameAndSignsItsReceipt(t *testing.T) {
	e := newEnv(t)
	e.store.putFailBody = "broken"
	var body []byte
	for _, f := range []struct{ id, blob string }{{"m-1", "one"}, {"m-2", ""}, {"m-3", "broken"}, {"m-1", "two"}} {
		body = blobframe.AppendUpload(body, f.id, []byte(f.blob))
	}
	resp := e.uploadBatch(e.access(userA), body)
	results := resp.batchResults(t)

	ids := make([]string, 0, len(results))
	statuses := make([]int, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ID)
		statuses = append(statuses, r.Status)
	}
	if !slices.Equal(ids, []string{"m-1", "m-2", "m-3", "m-1"}) ||
		!slices.Equal(statuses, []int{http.StatusCreated, http.StatusBadRequest, http.StatusInternalServerError, http.StatusCreated}) {
		t.Fatalf("results %s", resp.body)
	}

	for i, want := range map[int]string{0: "one", 3: "two"} {
		var stored UploadResponse
		if err := json.Unmarshal(results[i].Body, &stored); err != nil {
			t.Fatal(err)
		}
		receipt, err := e.receipts.Verify(stored.Receipt, testRegion)
		if err != nil {
			t.Fatalf("frame %d: receipt does not verify: %v", i, err)
		}
		if !strings.HasPrefix(stored.Key, userA+"/") || stored.Size != int64(len(want)) ||
			receipt.UserID != userA || receipt.Key != stored.Key || receipt.Size != stored.Size {
			t.Fatalf("frame %d: %s, receipt %+v", i, results[i].Body, receipt)
		}
		if o, ok := e.store.get(stored.Key); !ok || string(o.data) != want {
			t.Fatalf("frame %d: stored %q, %v", i, o.data, ok)
		}
	}
	if len(e.store.keys()) != 2 {
		t.Fatalf("stored %v, want only the two good frames", e.store.keys())
	}

	// A failed frame carries the error body a single upload answers, with
	// the batch request's id.
	requestID := resp.header.Get("X-Request-Id")
	for i, want := range map[int]httputil.ErrorResponse{
		1: {StatusCode: http.StatusBadRequest, Code: "invalid_request", Message: "empty body", RequestID: requestID},
		2: {StatusCode: http.StatusInternalServerError, Code: "internal_error", Message: "Storage error", RequestID: requestID},
	} {
		var got httputil.ErrorResponse
		if err := json.Unmarshal(results[i].Body, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("frame %d: %+v, want %+v", i, got, want)
		}
	}
	single := e.upload(e.access(userA), nil)
	if body := single.errorBody(t); single.status != http.StatusBadRequest || body.Code != "invalid_request" || body.Message != "empty body" {
		t.Fatalf("single empty upload = %d %s, want the batch frame's failure", single.status, single.body)
	}
}

func TestBatchUploadValidation(t *testing.T) {
	e := newEnv(t)
	token := e.access(userA)
	frame := blobframe.AppendUpload(nil, "f", []byte("x"))
	var full []byte
	for range blobframe.MaxUploadFrames {
		full = append(full, frame...)
	}
	cases := []struct {
		name    string
		body    []byte
		status  int
		message string
	}{
		{"no frames", nil, http.StatusBadRequest, "batch must contain at least one frame"},
		{"truncated id", []byte{0, 5, 'a'}, http.StatusBadRequest, "truncated id"},
		{"invalid id", blobframe.AppendUpload(nil, "not ok", []byte("x")), http.StatusBadRequest, "invalid id"},
		{"truncated blob after a good frame", append(slices.Clone(frame), 0, 1, 'g', 0, 0, 0, 9, 'x'), http.StatusBadRequest, "truncated blob"},
		{"too many frames", append(slices.Clone(full), frame...), http.StatusBadRequest, "too many frames (max 200)"},
		{"body too large", blobframe.AppendUpload(nil, "big", make([]byte, blobframe.MaxBatchBytes)), http.StatusRequestEntityTooLarge, "request too large"},
	}
	for _, tc := range cases {
		resp := e.uploadBatch(token, tc.body)
		code := "invalid_request"
		if tc.status == http.StatusRequestEntityTooLarge {
			code = "payload_too_large"
		}
		if body := resp.errorBody(t); resp.status != tc.status || body.Code != code || body.Message != tc.message {
			t.Fatalf("%s: status %d: %s", tc.name, resp.status, resp.body)
		}
	}
	if keys := e.store.keys(); len(keys) != 0 {
		t.Fatalf("a rejected batch stored %v", keys)
	}

	if results := e.uploadBatch(token, full).batchResults(t); len(results) != blobframe.MaxUploadFrames {
		t.Fatalf("%d frames: %d results", blobframe.MaxUploadFrames, len(results))
	}
	// One blob filling the cap but for its frame header.
	exact := blobframe.AppendUpload(nil, "f", make([]byte, blobframe.MaxBatchBytes-7))
	if results := e.uploadBatch(token, exact).batchResults(t); results[0].Status != http.StatusCreated {
		t.Fatalf("a body of exactly 32 MiB: %+v", results)
	}
}

// Upload and fetch have their own paths: an upload body sent to the fetch
// path is refused as a fetch, and stores nothing.
func TestBatchUploadAndFetchPathsAreSeparate(t *testing.T) {
	e := newEnv(t)
	token := e.access(userA)
	resp := e.do(http.MethodPost, "/blobs/batch", token, bytes.NewReader(blobframe.AppendUpload(nil, "f", []byte("x"))),
		map[string]string{"Content-Type": "application/octet-stream"})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("upload frames posted to the fetch path = %d: %s", resp.status, resp.body)
	}
	if keys := e.store.keys(); len(keys) != 0 {
		t.Fatalf("the fetch path stored %v", keys)
	}
	if results := e.uploadBatch(token, blobframe.AppendUpload(nil, "f", []byte("x"))).batchResults(t); len(results) != 1 || results[0].Status != http.StatusCreated {
		t.Fatalf("batch upload: %+v", results)
	}
}

func TestBatchUploadMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	m, err := metrics.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWithMetrics(t, m)
	body := blobframe.AppendUpload(blobframe.AppendUpload(blobframe.AppendUpload(nil, "a", []byte("123")), "b", nil), "c", []byte("4567"))
	e.uploadBatch(e.access(userA), body).batchResults(t)

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
	if got := sum(t, byName["storage.uploaded_bytes"]); got != 7 {
		t.Errorf("uploaded bytes %d, want 7", got)
	}
	if got := sum(t, byName["storage.receipts.signed"]); got != 2 {
		t.Errorf("receipts %d, want 2", got)
	}
}
