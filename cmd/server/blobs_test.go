package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/blobs"
)

const (
	fuzzUser  = "0195e2a1-0000-7000-8000-000000000001"
	otherUser = "0195e2a1-0000-7000-8000-0000000000ff"
)

func blobKey(userID string, n int) string {
	return fmt.Sprintf("%s/0195e2a1-0000-7000-8000-%012d", userID, n)
}

// sparseStore writes each requested size as a hole, so a cap test can use
// multi-megabyte blobs without spending the disk or the time.
func sparseStore(t testing.TB, sizes map[string]int64) *blobs.Store {
	t.Helper()
	store := &blobs.Store{Dir: t.TempDir()}
	for key, size := range sizes {
		path := filepath.Join(store.Dir, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(size); err != nil {
			t.Fatal(err)
		}
		file.Close()
	}
	return store
}

type frame struct {
	Key    string
	Status byte
	Blob   []byte
}

func decodeFrames(t testing.TB, payload []byte) []frame {
	t.Helper()
	frames := []frame{}
	for offset := 0; offset < len(payload); {
		if len(payload)-offset < 2 {
			t.Fatalf("truncated key length at %d", offset)
		}
		keyLen := int(binary.BigEndian.Uint16(payload[offset:]))
		offset += 2
		if len(payload)-offset < keyLen+5 {
			t.Fatalf("truncated frame header at %d", offset)
		}
		f := frame{Key: string(payload[offset : offset+keyLen])}
		offset += keyLen
		f.Status = payload[offset]
		offset++
		blobLen := int(binary.BigEndian.Uint32(payload[offset:]))
		offset += 4
		if blobLen > len(payload)-offset {
			t.Fatalf("truncated blob at %d: want %d, have %d", offset, blobLen, len(payload)-offset)
		}
		f.Blob = payload[offset : offset+blobLen]
		offset += blobLen
		frames = append(frames, f)
	}
	return frames
}

func TestStreamBlobBatch(t *testing.T) {
	present, absent := blobKey(fuzzUser, 1), blobKey(fuzzUser, 2)
	foreign := blobKey(otherUser, 3)
	store := sparseStore(t, nil)
	if err := store.Put(present, []byte("ciphertext")); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(foreign, []byte("not yours")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	keys := []string{present, absent, foreign, "../../etc/passwd", "", present}
	if err := streamBlobBatch(&out, store, fuzzUser, keys); err != nil {
		t.Fatal(err)
	}
	frames := decodeFrames(t, out.Bytes())
	if len(frames) != len(keys) {
		t.Fatalf("got %d frames, want %d", len(frames), len(keys))
	}
	wantStatus := []byte{frameOK, frameMissing, frameMissing, frameMissing, frameMissing, frameOK}
	for i, f := range frames {
		if f.Key != keys[i] {
			t.Errorf("frame %d key = %q, want %q", i, f.Key, keys[i])
		}
		if f.Status != wantStatus[i] {
			t.Errorf("frame %d status = %d, want %d", i, f.Status, wantStatus[i])
		}
		if f.Status != frameOK && len(f.Blob) != 0 {
			t.Errorf("frame %d carries %d bytes with status %d", i, len(f.Blob), f.Status)
		}
	}
	if string(frames[0].Blob) != "ciphertext" || string(frames[5].Blob) != "ciphertext" {
		t.Errorf("payload mismatch: %q, %q", frames[0].Blob, frames[5].Blob)
	}
}

func TestStreamBlobBatchCap(t *testing.T) {
	const twelveMiB = 12 << 20
	first, second, third := blobKey(fuzzUser, 1), blobKey(fuzzUser, 2), blobKey(fuzzUser, 3)
	small := blobKey(fuzzUser, 4)
	store := sparseStore(t, map[string]int64{
		first: twelveMiB, second: twelveMiB, third: twelveMiB, small: 16,
	})

	var out bytes.Buffer
	keys := []string{first, second, third, small}
	if err := streamBlobBatch(&out, store, fuzzUser, keys); err != nil {
		t.Fatal(err)
	}
	if out.Len() > maxBatchBytes {
		t.Fatalf("payload %d exceeds the %d cap", out.Len(), maxBatchBytes)
	}
	frames := decodeFrames(t, out.Bytes())
	// Two 12 MiB blobs fit under 32 MiB; the third would not. A later blob
	// small enough to fit still ships.
	want := []byte{frameOK, frameOK, frameOmitted, frameOK}
	for i, f := range frames {
		if f.Status != want[i] {
			t.Fatalf("frame %d status = %d, want %d", i, f.Status, want[i])
		}
	}
}

func TestStreamBlobBatchShipsFirstOversizeBlob(t *testing.T) {
	oversize, next := blobKey(fuzzUser, 1), blobKey(fuzzUser, 2)
	store := sparseStore(t, map[string]int64{oversize: maxBatchBytes + 1, next: 8})

	var out bytes.Buffer
	if err := streamBlobBatch(&out, store, fuzzUser, []string{oversize, next}); err != nil {
		t.Fatal(err)
	}
	frames := decodeFrames(t, out.Bytes())
	if frames[0].Status != frameOK || len(frames[0].Blob) != maxBatchBytes+1 {
		t.Fatalf("first frame did not ship: status %d, %d bytes", frames[0].Status, len(frames[0].Blob))
	}
	if frames[1].Status != frameOmitted {
		t.Fatalf("second frame status = %d, want omitted", frames[1].Status)
	}
}

func TestStreamBlobBatchSkipsUnframeableFile(t *testing.T) {
	huge := blobKey(fuzzUser, 1)
	store := sparseStore(t, map[string]int64{huge: maxBlobBytes + 1})

	var out bytes.Buffer
	if err := streamBlobBatch(&out, store, fuzzUser, []string{huge}); err != nil {
		t.Fatal(err)
	}
	if frames := decodeFrames(t, out.Bytes()); frames[0].Status != frameMissing {
		t.Fatalf("status = %d, want missing", frames[0].Status)
	}
}

func batchRequest(store *blobs.Store, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/blobs/batch", strings.NewReader(body))
	session := &auth.Session{ID: "session", User: auth.User{ID: fuzzUser}}
	r = r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, session))
	w := httptest.NewRecorder()
	handleBatchFetchBlobs(store)(w, r)
	return w
}

func TestBatchFetchValidation(t *testing.T) {
	store := sparseStore(t, map[string]int64{blobKey(fuzzUser, 1): 4})
	longKey := strings.Repeat("k", maxBatchKeyChars+1)
	manyKeys := make([]string, maxBatchFetchKeys+1)
	for i := range manyKeys {
		manyKeys[i] = `"` + blobKey(fuzzUser, i) + `"`
	}

	tests := []struct {
		name string
		body string
		want int
	}{
		{"not json", "{", http.StatusBadRequest},
		{"keys not an array", `{"keys": "a"}`, http.StatusBadRequest},
		{"key not a string", `{"keys": [1]}`, http.StatusBadRequest},
		{"null key", `{"keys": [null]}`, http.StatusBadRequest},
		{"trailing garbage", `{"keys": ["a"]}!`, http.StatusBadRequest},
		{"missing keys", `{}`, http.StatusBadRequest},
		{"empty keys", `{"keys": []}`, http.StatusBadRequest},
		{"key too long", `{"keys": ["` + longKey + `"]}`, http.StatusBadRequest},
		{"too many keys", `{"keys": [` + strings.Join(manyKeys, ",") + `]}`, http.StatusBadRequest},
		{"body too large", `{"keys": ["` + strings.Repeat("x", maxBatchFetchBody) + `"]}`, http.StatusRequestEntityTooLarge},
		{"ok", `{"keys": ["` + blobKey(fuzzUser, 1) + `"]}`, http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := batchRequest(store, test.body).Code; got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
}

func TestBatchFetchMaximumKeys(t *testing.T) {
	store := sparseStore(t, map[string]int64{blobKey(fuzzUser, 0): 3})
	keys := make([]string, maxBatchFetchKeys)
	for i := range keys {
		keys[i] = `"` + blobKey(fuzzUser, i) + `"`
	}
	w := batchRequest(store, `{"keys": [`+strings.Join(keys, ",")+`]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("content type = %q", w.Header().Get("Content-Type"))
	}
	if frames := decodeFrames(t, w.Body.Bytes()); len(frames) != maxBatchFetchKeys {
		t.Fatalf("got %d frames, want %d", len(frames), maxBatchFetchKeys)
	}
}

// FuzzBatchFetchBody drives the handler with arbitrary request bodies: every
// answer must be one of the documented statuses, and a 200 must decode as a
// well-formed frame stream.
func FuzzBatchFetchBody(f *testing.F) {
	store := sparseStore(f, map[string]int64{blobKey(fuzzUser, 1): 5})
	f.Add(`{"keys": ["` + blobKey(fuzzUser, 1) + `"]}`)
	f.Add(`{"keys": []}`)
	f.Add(`{"keys": ["../../../etc/passwd"]}`)
	f.Add(`{"keys": [null]}`)
	f.Add(`{"keys": {"a": 1}}`)
	f.Add("\x00\xff")

	f.Fuzz(func(t *testing.T, body string) {
		w := batchRequest(store, body)
		switch w.Code {
		case http.StatusOK:
			frames := decodeFrames(t, w.Body.Bytes())
			if len(frames) == 0 {
				t.Fatalf("200 with no frames for %q", body)
			}
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		default:
			t.Fatalf("status %d for %q", w.Code, body)
		}
	})
}

// FuzzStreamBlobBatch checks the framing invariants directly: one frame per
// key, in order, keys echoed verbatim, and bytes only ever from the caller's
// own directory.
func FuzzStreamBlobBatch(f *testing.F) {
	store := sparseStore(f, nil)
	if err := store.Put(blobKey(fuzzUser, 1), []byte("mine")); err != nil {
		f.Fatal(err)
	}
	if err := store.Put(blobKey(otherUser, 1), []byte("theirs")); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "loose"), []byte("loose"), 0o644); err != nil {
		f.Fatal(err)
	}
	outside := filepath.Join(f.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		f.Fatal(err)
	}

	f.Add(blobKey(fuzzUser, 1) + "\n" + blobKey(otherUser, 1))
	f.Add("../loose\n..\n/\n" + outside)
	f.Add(fuzzUser + "/" + strings.Repeat("f", 36))
	f.Add("\n\n\n")

	f.Fuzz(func(t *testing.T, joined string) {
		keys := strings.Split(joined, "\n")
		if len(keys) > maxBatchFetchKeys {
			keys = keys[:maxBatchFetchKeys]
		}
		for _, key := range keys {
			if utf8.RuneCountInString(key) > maxBatchKeyChars {
				t.Skip()
			}
		}
		var out bytes.Buffer
		if err := streamBlobBatch(&out, store, fuzzUser, keys); err != nil {
			t.Fatalf("stream failed for %q: %v", keys, err)
		}
		frames := decodeFrames(t, out.Bytes())
		if len(frames) != len(keys) {
			t.Fatalf("got %d frames for %d keys", len(frames), len(keys))
		}
		for i, f := range frames {
			if f.Key != keys[i] {
				t.Fatalf("frame %d key = %q, want %q", i, f.Key, keys[i])
			}
			if f.Status > frameOmitted {
				t.Fatalf("frame %d status = %d", i, f.Status)
			}
			if f.Status != frameOK && len(f.Blob) != 0 {
				t.Fatalf("frame %d carries bytes with status %d", i, f.Status)
			}
			if string(f.Blob) != "" && string(f.Blob) != "mine" {
				t.Fatalf("frame %d served %q", i, f.Blob)
			}
		}
	})
}
