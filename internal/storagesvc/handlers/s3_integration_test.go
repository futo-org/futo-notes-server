package handlers

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"uuid"

	"futo-notes-server/internal/blobframe"
	"futo-notes-server/internal/storagesvc/storage"
)

// newS3Env runs the service against the MinIO named by S3_TEST_ENDPOINT, or
// skips the test when it is unset. It returns a fresh user whose prefix is
// purged through the admin endpoint when the test ends.
func newS3Env(t *testing.T) (*env, string) {
	t.Helper()
	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT not set")
	}
	region := os.Getenv("S3_TEST_REGION")
	if region == "" {
		region = "us-east-1"
	}
	store := storage.NewS3Storage(storage.S3Options{
		Endpoint:        endpoint,
		Region:          region,
		Bucket:          "notes-us-east",
		AccessKeyID:     os.Getenv("S3_TEST_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("S3_TEST_SECRET_ACCESS_KEY"),
		ForcePathStyle:  true,
	})
	e := newEnvWith(t, store, nil)
	user := uuid.NewV4().String()
	t.Cleanup(func() {
		for range 100 {
			resp := e.do(http.MethodDelete, "/internal/users/"+user, e.admin(), nil, nil)
			var body DeleteUserResponse
			resp.json(t, &body)
			if body.Done {
				return
			}
		}
		t.Error("purge did not finish")
	})
	return e, user
}

func TestS3EndToEnd(t *testing.T) {
	e, user := newS3Env(t)
	token := e.access(user)

	var up UploadResponse
	resp := e.upload(token, []byte("0123456789"))
	if resp.status != http.StatusCreated {
		t.Fatalf("upload: %d %s", resp.status, resp.body)
	}
	resp.json(t, &up)
	if r, err := e.receipts.Verify(up.Receipt, testRegion); err != nil || r.Key != up.Key || r.Size != 10 {
		t.Fatalf("receipt %+v %v", r, err)
	}

	if resp := e.do(http.MethodGet, "/blobs/"+up.Key, token, nil, nil); resp.status != http.StatusOK || string(resp.body) != "0123456789" {
		t.Fatalf("get: %d %q", resp.status, resp.body)
	}
	resp = e.do(http.MethodGet, "/blobs/"+up.Key, token, nil, map[string]string{"Range": "bytes=3-5"})
	if resp.status != http.StatusPartialContent || string(resp.body) != "345" || resp.header.Get("Content-Range") != "bytes 3-5/10" {
		t.Fatalf("range: %d %q %q", resp.status, resp.body, resp.header.Get("Content-Range"))
	}
	resp = e.do(http.MethodGet, "/blobs/"+up.Key, token, nil, map[string]string{"Range": "bytes=10-"})
	if resp.status != http.StatusRequestedRangeNotSatisfiable || resp.header.Get("Content-Range") != "bytes */10" {
		t.Fatalf("unsatisfiable range: %d %q", resp.status, resp.header.Get("Content-Range"))
	}
	absent := user + "/" + uuid.NewV7().String()
	if resp := e.do(http.MethodGet, "/blobs/"+absent, token, nil, nil); resp.status != http.StatusNotFound {
		t.Fatalf("absent: %d", resp.status)
	}

	resp = e.postJSON("/blobs/batch", token, map[string][]string{"keys": {up.Key, absent}})
	frames := decodeFrames(t, resp.body)
	if len(frames) != 2 || frames[0].Status != blobframe.OK || string(frames[0].Blob) != "0123456789" ||
		frames[1].Status != blobframe.Missing {
		t.Fatalf("batch: %v", statuses(frames))
	}

	list, _ := e.list(url.Values{"prefix": {user + "/"}})
	if len(list.Blobs) != 1 || list.Blobs[0].Key != up.Key || list.Blobs[0].Size != 10 || list.NextCursor != nil {
		t.Fatalf("list: %+v", list)
	}

	resp = e.deleteJSON("/internal/blobs", e.admin(), map[string][]string{"keys": {up.Key, absent}})
	var deleted DeleteBlobsResponse
	resp.json(t, &deleted)
	if deleted.Deleted != 2 || len(deleted.Failed) != 0 {
		t.Fatalf("delete: %+v", deleted)
	}
	if resp := e.do(http.MethodGet, "/blobs/"+up.Key, token, nil, nil); resp.status != http.StatusNotFound {
		t.Fatalf("after delete: %d", resp.status)
	}
}

func TestS3TruncatedUploadStoresNothing(t *testing.T) {
	e, user := newS3Env(t)
	head := "POST /blobs HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + e.access(user) + "\r\nContent-Length: 1000000\r\n\r\n"
	status, body := e.raw(t, head+strings.Repeat("x", 400000), true)
	if status != 0 && status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", status, body)
	}
	list, _ := e.list(url.Values{"prefix": {user + "/"}})
	if len(list.Blobs) != 0 {
		t.Fatalf("a truncated upload stored %+v", list.Blobs)
	}
}

func TestS3PurgeUserPages(t *testing.T) {
	e, user := newS3Env(t)
	store := e.server.Storage
	ctx := context.Background()
	for range storage.MaxDeleteKeys + 3 {
		if err := store.PutObject(ctx, user+"/"+uuid.NewV7().String(), strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}
	var got []DeleteUserResponse
	for range 3 {
		var body DeleteUserResponse
		e.do(http.MethodDelete, "/internal/users/"+user, e.admin(), nil, nil).json(t, &body)
		got = append(got, body)
	}
	want := []DeleteUserResponse{{Deleted: storage.MaxDeleteKeys}, {Deleted: 3, Done: true}, {Deleted: 0, Done: true}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls %+v, want %+v", got, want)
		}
	}
}
