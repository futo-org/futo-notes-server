package handlers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"uuid"

	"futo-notes-server/internal/blobframe"
)

func TestUploadStoresTheBlobAndSignsAReceipt(t *testing.T) {
	e := newEnv(t)
	resp := e.upload(e.access(userA), []byte("ciphertext"))
	if resp.status != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.status, resp.body)
	}
	var body UploadResponse
	resp.json(t, &body)

	blobID, ok := strings.CutPrefix(body.Key, userA+"/")
	if !ok {
		t.Fatalf("key %q is not under the caller's prefix", body.Key)
	}
	if id, err := uuid.Parse(blobID); err != nil || id[6]>>4 != 7 || id.String() != blobID {
		t.Fatalf("blob id %q is not a canonical UUIDv7", blobID)
	}
	if body.Size != int64(len("ciphertext")) {
		t.Fatalf("size %d", body.Size)
	}
	receipt, err := e.receipts.Verify(body.Receipt, testRegion)
	if err != nil {
		t.Fatalf("receipt does not verify: %v", err)
	}
	if receipt.UserID != userA || receipt.Key != body.Key || receipt.Size != body.Size {
		t.Fatalf("receipt %+v does not match %+v", receipt, body)
	}
	if o, ok := e.store.get(body.Key); !ok || string(o.data) != "ciphertext" {
		t.Fatalf("stored %q, %v", o.data, ok)
	}
}

func TestUploadsGetDistinctKeys(t *testing.T) {
	e := newEnv(t)
	token := e.access(userA)
	seen := map[string]bool{}
	for range 20 {
		var body UploadResponse
		e.upload(token, []byte("x")).json(t, &body)
		if seen[body.Key] {
			t.Fatalf("key %s minted twice", body.Key)
		}
		seen[body.Key] = true
	}
}

func TestUploadRejectsAnEmptyBody(t *testing.T) {
	e := newEnv(t)
	resp := e.upload(e.access(userA), nil)
	if resp.status != http.StatusBadRequest || resp.errorCode(t) != "invalid_request" {
		t.Fatalf("status %d: %s", resp.status, resp.body)
	}
	if keys := e.store.keys(); len(keys) != 0 {
		t.Fatalf("stored %v", keys)
	}
}

func TestUploadRequiresContentLength(t *testing.T) {
	e := newEnv(t)
	// An io.Reader of unknown length goes out chunked.
	chunked := e.do(http.MethodPost, "/blobs", e.access(userA), io.MultiReader(strings.NewReader("abc")), nil)
	if chunked.status != http.StatusLengthRequired || chunked.errorCode(t) != "length_required" {
		t.Fatalf("chunked: status %d: %s", chunked.status, chunked.body)
	}

	status, _ := e.raw(t, "POST /blobs HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+e.access(userA)+"\r\nConnection: close\r\n\r\n", false)
	if status != http.StatusLengthRequired {
		t.Fatalf("no Content-Length: status %d", status)
	}
}

func TestUploadSizeLimit(t *testing.T) {
	e := newEnv(t)
	e.store.discardPuts = true
	token := e.access(userA)

	tooLarge := fmt.Sprintf("POST /blobs HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n", token, blobframe.MaxBlobBytes+1)
	status, body := e.raw(t, tooLarge, false)
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(body, "blob too large") {
		t.Fatalf("over the limit: status %d: %s", status, body)
	}

	req, err := http.NewRequest(http.MethodPost, e.url+"/blobs", io.LimitReader(zeros{}, blobframe.MaxBlobBytes))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = blobframe.MaxBlobBytes
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.Size != blobframe.MaxBlobBytes {
		t.Fatalf("at the limit: status %d, %+v", resp.StatusCode, created)
	}
}

func TestUploadWithAShortBodyStoresNothing(t *testing.T) {
	e := newEnv(t)
	head := "POST /blobs HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + e.access(userA) + "\r\nContent-Length: 100\r\n\r\n"
	status, body := e.raw(t, head+strings.Repeat("x", 40), true)
	if status != 0 && status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", status, body)
	}
	if keys := e.store.keys(); len(keys) != 0 {
		t.Fatalf("a truncated upload stored %v", keys)
	}
}

// raw sends a hand-written HTTP/1.1 request. With halfClose the write side is
// closed after it, as a client that gives up mid-body would. It returns 0
// when no response arrives.
func (e *env) raw(t *testing.T, request string, halfClose bool) (int, string) {
	t.Helper()
	u, err := url.Parse(e.url)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	if halfClose {
		if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestGetBlob(t *testing.T) {
	e := newEnv(t)
	own := userA + "/" + uuid.NewV7().String()
	theirs := userB + "/" + uuid.NewV7().String()
	e.store.put(own, []byte("mine"))
	e.store.put(theirs, []byte("theirs"))
	token := e.access(userA)

	resp := e.do(http.MethodGet, "/blobs/"+own, token, nil, nil)
	if resp.status != http.StatusOK || string(resp.body) != "mine" {
		t.Fatalf("own blob: status %d: %q", resp.status, resp.body)
	}
	if ct := resp.header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type %q", ct)
	}

	for name, path := range map[string]string{
		"another user's blob": "/blobs/" + theirs,
		"absent blob":         "/blobs/" + userA + "/" + uuid.NewV7().String(),
		"malformed blob id":   "/blobs/" + userA + "/not-a-uuid",
		"malformed user id":   "/blobs/nobody/" + uuid.NewV7().String(),
	} {
		resp := e.do(http.MethodGet, path, token, nil, nil)
		if resp.status != http.StatusNotFound || resp.errorCode(t) != "not_found" {
			t.Fatalf("%s: status %d: %s", name, resp.status, resp.body)
		}
	}
}

func TestGetBlobRanges(t *testing.T) {
	e := newEnv(t)
	key := userA + "/" + uuid.NewV7().String()
	e.store.put(key, []byte("0123456789"))
	token := e.access(userA)

	cases := []struct {
		rangeHeader  string
		status       int
		body         string
		contentRange string
	}{
		{"bytes=2-4", http.StatusPartialContent, "234", "bytes 2-4/10"},
		{"bytes=7-", http.StatusPartialContent, "789", "bytes 7-9/10"},
		{"bytes=-3", http.StatusPartialContent, "789", "bytes 7-9/10"},
		{"bytes=8-100", http.StatusPartialContent, "89", "bytes 8-9/10"},
		{"bytes=0-9", http.StatusPartialContent, "0123456789", "bytes 0-9/10"},
		{"bytes=10-", http.StatusRequestedRangeNotSatisfiable, "", "bytes */10"},
		{"bytes=-0", http.StatusRequestedRangeNotSatisfiable, "", "bytes */10"},
		// Ignored, so served whole.
		{"bytes=5-2", http.StatusOK, "0123456789", ""},
		{"bytes=0-1,4-5", http.StatusOK, "0123456789", ""},
		{"items=0-1", http.StatusOK, "0123456789", ""},
		{"bytes=99999999999999999999-", http.StatusOK, "0123456789", ""},
	}
	for _, tc := range cases {
		resp := e.do(http.MethodGet, "/blobs/"+key, token, nil, map[string]string{"Range": tc.rangeHeader})
		if resp.status != tc.status || resp.header.Get("Content-Range") != tc.contentRange {
			t.Fatalf("%s: status %d, Content-Range %q", tc.rangeHeader, resp.status, resp.header.Get("Content-Range"))
		}
		if tc.status == http.StatusRequestedRangeNotSatisfiable {
			if code := resp.errorCode(t); code != "range_not_satisfiable" {
				t.Fatalf("%s: code %q", tc.rangeHeader, code)
			}
		} else if string(resp.body) != tc.body {
			t.Fatalf("%s: body %q", tc.rangeHeader, resp.body)
		}
	}
}

func TestBatchValidation(t *testing.T) {
	e := newEnv(t)
	token := e.access(userA)
	cases := []struct {
		name    string
		body    string
		status  int
		message string
	}{
		{"invalid json", `{"keys":`, http.StatusBadRequest, "invalid json"},
		{"not an object", `[]`, http.StatusBadRequest, "invalid json"},
		{"no keys", `{}`, http.StatusBadRequest, "keys must not be empty"},
		{"empty keys", `{"keys":[]}`, http.StatusBadRequest, "keys must not be empty"},
		{"null key", `{"keys":[null]}`, http.StatusBadRequest, "keys must be strings"},
		{"number key", `{"keys":[1]}`, http.StatusBadRequest, "invalid json"},
		{"too many keys", `{"keys":[` + strings.TrimSuffix(strings.Repeat(`"k",`, blobframe.MaxKeys+1), ",") + `]}`,
			http.StatusBadRequest, "too many keys (max 200)"},
		{"key too long", `{"keys":["` + strings.Repeat("é", blobframe.MaxKeyChars+1) + `"]}`,
			http.StatusBadRequest, "key too long (max 128 chars)"},
		{"body too large", `{"keys":["` + strings.Repeat("x", blobframe.MaxRequestBytes) + `"]}`,
			http.StatusRequestEntityTooLarge, "request too large"},
	}
	for _, tc := range cases {
		resp := e.do(http.MethodPost, "/blobs/batch", token, strings.NewReader(tc.body), nil)
		code := "invalid_request"
		if tc.status == http.StatusRequestEntityTooLarge {
			code = "payload_too_large"
		}
		// The message names which check failed; blobframe's messages are
		// shared with the self-hosted batch route.
		if body := resp.errorBody(t); resp.status != tc.status || body.Code != code || body.Message != tc.message {
			t.Fatalf("%s: status %d: %s", tc.name, resp.status, resp.body)
		}
	}

	// 128 characters is allowed even when it is more than 128 bytes.
	resp := e.batch(token, map[string][]string{"keys": {strings.Repeat("é", blobframe.MaxKeyChars)}})
	if resp.status != http.StatusOK {
		t.Fatalf("128-char key: status %d: %s", resp.status, resp.body)
	}
	resp = e.batch(token, map[string][]string{"keys": make([]string, blobframe.MaxKeys)})
	if resp.status != http.StatusOK {
		t.Fatalf("200 keys: status %d: %s", resp.status, resp.body)
	}
}

func TestBatchFrameBytes(t *testing.T) {
	e := newEnv(t)
	own := userA + "/" + uuid.NewV7().String()
	absent := userA + "/" + uuid.NewV7().String()
	theirs := userB + "/" + uuid.NewV7().String()
	e.store.put(own, []byte("abc"))
	e.store.put(theirs, []byte("secret"))

	resp := e.batch(e.access(userA), map[string][]string{"keys": {own, absent, theirs, "junk", own}})
	if resp.status != http.StatusOK || resp.header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("status %d, Content-Type %q", resp.status, resp.header.Get("Content-Type"))
	}
	var want bytes.Buffer
	frame := func(key string, status byte, blob string) {
		want.Write([]byte{byte(len(key) >> 8), byte(len(key))})
		want.WriteString(key)
		want.WriteByte(status)
		n := len(blob)
		want.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		want.WriteString(blob)
	}
	frame(own, 0, "abc")
	frame(absent, 1, "")
	frame(theirs, 1, "")
	frame("junk", 1, "")
	frame(own, 0, "abc")
	if !bytes.Equal(resp.body, want.Bytes()) {
		t.Fatalf("got  % x\nwant % x", resp.body, want.Bytes())
	}
}

func TestBatchCapsThePayloadButAlwaysShipsTheFirstBlob(t *testing.T) {
	e := newEnv(t)
	big := userA + "/" + uuid.NewV7().String()
	small := userA + "/" + uuid.NewV7().String()
	e.store.putSynthetic(big, blobframe.MaxBatchBytes+1)
	e.store.put(small, []byte("x"))

	resp := e.batch(e.access(userA), map[string][]string{"keys": {big, small}})
	frames := decodeFrames(t, resp.body)
	if len(frames) != 2 || frames[0].Status != blobframe.OK || len(frames[0].Blob) != blobframe.MaxBatchBytes+1 ||
		frames[1].Status != blobframe.Omitted {
		t.Fatalf("frames: %d, statuses %v", len(frames), statuses(frames))
	}

	resp = e.batch(e.access(userA), map[string][]string{"keys": {small, big, small}})
	frames = decodeFrames(t, resp.body)
	if got := statuses(frames); !slices.Equal(got, []blobframe.Status{blobframe.OK, blobframe.Omitted, blobframe.OK}) {
		t.Fatalf("statuses %v", got)
	}
}

func TestBatchReportsBlobsOverTheUploadLimitMissing(t *testing.T) {
	e := newEnv(t)
	huge := userA + "/" + uuid.NewV7().String()
	e.store.putSynthetic(huge, blobframe.MaxBlobBytes+1)
	resp := e.batch(e.access(userA), map[string][]string{"keys": {huge}})
	frames := decodeFrames(t, resp.body)
	if len(frames) != 1 || frames[0].Status != blobframe.Missing {
		t.Fatalf("statuses %v", statuses(frames))
	}
}

func TestBatchAbortsTheConnectionOnAStorageFailure(t *testing.T) {
	e := newEnv(t)
	first := userA + "/" + uuid.NewV7().String()
	broken := userA + "/" + uuid.NewV7().String()
	e.store.put(first, []byte("ok"))
	e.store.put(broken, []byte("never"))
	e.store.getErr = map[string]error{broken: errors.New("s3 down")}

	data, err := json.Marshal(map[string][]string{"keys": {first, broken}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.url+"/blobs/batch", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.access(userA))
	resp, err := e.client.Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("a failure mid-stream must abort the transfer, not end it cleanly")
	}
}

func decodeFrames(t *testing.T, data []byte) []blobframe.Frame {
	t.Helper()
	r := bytes.NewReader(data)
	var frames []blobframe.Frame
	for {
		f, err := blobframe.ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, f)
	}
}

func statuses(frames []blobframe.Frame) []blobframe.Status {
	out := make([]blobframe.Status, len(frames))
	for i, f := range frames {
		out[i] = f.Status
	}
	return out
}

func TestReadiness(t *testing.T) {
	e := newEnv(t)
	resp := e.do(http.MethodGet, "/readyz", "", nil, nil)
	if resp.status != http.StatusOK || resp.header.Get("X-Request-Id") != "" {
		t.Fatalf("status %d, X-Request-Id %q", resp.status, resp.header.Get("X-Request-Id"))
	}
	e.server.BeginDrain()
	if resp := e.do(http.MethodGet, "/readyz", "", nil, nil); resp.status != http.StatusServiceUnavailable {
		t.Fatalf("draining: status %d", resp.status)
	}
	// Draining only fails readiness; requests are still served.
	if resp := e.upload(e.access(userA), []byte("x")); resp.status != http.StatusCreated {
		t.Fatalf("upload while draining: status %d", resp.status)
	}
}

func TestUnknownRoutesUseTheErrorShape(t *testing.T) {
	e := newEnv(t)
	resp := e.do(http.MethodGet, "/nope", "", nil, nil)
	if resp.status != http.StatusNotFound || resp.errorCode(t) != "not_found" {
		t.Fatalf("status %d: %s", resp.status, resp.body)
	}
	resp = e.do(http.MethodPut, "/blobs", e.access(userA), nil, nil)
	if resp.status != http.StatusMethodNotAllowed || resp.errorCode(t) != "method_not_allowed" {
		t.Fatalf("status %d: %s", resp.status, resp.body)
	}
}
