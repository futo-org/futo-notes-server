package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"
)

// testBucket is the MinIO bucket the S3 integration tests use; compose and
// the verification gate create it.
const testBucket = "notes-us-east"

// newTestS3Storage connects to the MinIO named by S3_TEST_ENDPOINT, or skips
// the test when it is unset.
func newTestS3Storage(t *testing.T) *S3Storage {
	t.Helper()
	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT not set")
	}
	region := os.Getenv("S3_TEST_REGION")
	if region == "" {
		region = "us-east-1"
	}
	return NewS3Storage(S3Options{
		Endpoint:        endpoint,
		Region:          region,
		Bucket:          testBucket,
		AccessKeyID:     os.Getenv("S3_TEST_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("S3_TEST_SECRET_ACCESS_KEY"),
		ForcePathStyle:  true,
	})
}

// testUser returns a fresh user id whose prefix is emptied when the test
// ends, so runs never see each other's objects.
func testUser(t *testing.T, s *S3Storage) string {
	t.Helper()
	user := uuid.NewV4().String()
	t.Cleanup(func() {
		ctx := context.Background()
		for {
			page, err := s.ListObjects(ctx, user+"/", "", MaxDeleteKeys)
			if err != nil {
				t.Errorf("cleanup list: %v", err)
				return
			}
			keys := make([]string, len(page.Objects))
			for i, o := range page.Objects {
				keys[i] = o.Key
			}
			if failed, err := s.DeleteObjects(ctx, keys); err != nil || len(failed) > 0 {
				t.Errorf("cleanup delete: %v %v", failed, err)
				return
			}
			if !page.Truncated {
				return
			}
		}
	})
	return user
}

func newKey(user string) string { return user + "/" + uuid.NewV7().String() }

func readObject(t *testing.T, s *S3Storage, key, byteRange string) (*Object, string) {
	t.Helper()
	obj, err := s.GetObject(context.Background(), key, byteRange)
	if err != nil {
		t.Fatalf("get %s %q: %v", key, byteRange, err)
	}
	defer obj.Body.Close()
	data, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	return obj, string(data)
}

func TestS3PutGetAndRanges(t *testing.T) {
	s := newTestS3Storage(t)
	ctx := context.Background()
	key := newKey(testUser(t, s))
	if err := s.PutObject(ctx, key, strings.NewReader("0123456789"), 10); err != nil {
		t.Fatal(err)
	}

	obj, data := readObject(t, s, key, "")
	if data != "0123456789" || obj.Length != 10 || obj.ContentRange != "" {
		t.Fatalf("whole object: %q, %+v", data, obj)
	}
	for _, tc := range []struct{ rng, body, contentRange string }{
		{"bytes=2-4", "234", "bytes 2-4/10"},
		{"bytes=7-", "789", "bytes 7-9/10"},
		{"bytes=-3", "789", "bytes 7-9/10"},
		{"bytes=8-100", "89", "bytes 8-9/10"},
	} {
		obj, data := readObject(t, s, key, tc.rng)
		if data != tc.body || obj.ContentRange != tc.contentRange || obj.Length != int64(len(tc.body)) {
			t.Fatalf("%s: %q, %+v", tc.rng, data, obj)
		}
	}

	_, err := s.GetObject(ctx, key, "bytes=10-")
	if rangeErr, ok := errors.AsType[*RangeError](err); !ok || rangeErr.Size != 10 {
		t.Fatalf("unsatisfiable range: %v", err)
	}
	if _, err := s.GetObject(ctx, newKey(testUser(t, s)), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent key: %v", err)
	}
	if _, err := s.GetObject(ctx, newKey(testUser(t, s)), "bytes=0-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent key with a range: %v", err)
	}
}

func TestS3PutIsCreateOnly(t *testing.T) {
	s := newTestS3Storage(t)
	ctx := context.Background()
	key := newKey(testUser(t, s))
	if err := s.PutObject(ctx, key, strings.NewReader("first"), 5); err != nil {
		t.Fatal(err)
	}
	if err := s.PutObject(ctx, key, strings.NewReader("second"), 6); !errors.Is(err, ErrExists) {
		t.Fatalf("second put: %v", err)
	}
	if _, data := readObject(t, s, key, ""); data != "first" {
		t.Fatalf("object overwritten: %q", data)
	}
}

// failAfter yields n bytes, then fails, as a client connection dropping
// mid-upload does.
type failAfter struct {
	n   int
	err error
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, f.err
	}
	n := min(len(p), f.n)
	clear(p[:n])
	f.n -= n
	return n, nil
}

func TestS3ShortBodyStoresNothing(t *testing.T) {
	s := newTestS3Storage(t)
	ctx := context.Background()
	user := testUser(t, s)

	short := newKey(user)
	err := s.PutObject(ctx, short, &failAfter{n: 3 << 20, err: io.EOF}, 4<<20)
	if !errors.Is(err, ErrBody) {
		t.Fatalf("short body: %v", err)
	}
	failing := newKey(user)
	err = s.PutObject(ctx, failing, &failAfter{n: 1 << 20, err: io.ErrUnexpectedEOF}, 4<<20)
	if !errors.Is(err, ErrBody) {
		t.Fatalf("failing body: %v", err)
	}
	for _, key := range []string{short, failing} {
		if _, err := s.GetObject(ctx, key, ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s was stored truncated: %v", key, err)
		}
	}
}

func TestS3ListAndDelete(t *testing.T) {
	s := newTestS3Storage(t)
	ctx := context.Background()
	user := testUser(t, s)
	other := testUser(t, s)
	var keys []string
	for range 5 {
		key := newKey(user)
		if err := s.PutObject(ctx, key, bytes.NewReader([]byte("abc")), 3); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if err := s.PutObject(ctx, newKey(other), strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	slices.Sort(keys)

	var listed []string
	startAfter := ""
	for {
		page, err := s.ListObjects(ctx, user+"/", startAfter, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Objects {
			if o.Size != 3 || time.Since(o.LastModified) > time.Hour || o.LastModified.Location() != time.UTC {
				t.Fatalf("object %+v", o)
			}
			listed = append(listed, o.Key)
		}
		if !page.Truncated {
			break
		}
		startAfter = page.LastKey
	}
	if !slices.Equal(listed, keys) {
		t.Fatalf("listed %v\nwant   %v", listed, keys)
	}

	absent := newKey(user)
	failed, err := s.DeleteObjects(ctx, append(keys[:3:3], absent))
	if err != nil || len(failed) != 0 {
		t.Fatalf("delete: %v %v", failed, err)
	}
	page, err := s.ListObjects(ctx, user+"/", "", MaxDeleteKeys)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Objects) != 2 || page.Truncated {
		t.Fatalf("after delete: %+v", page)
	}
	if page, err := s.ListObjects(ctx, other+"/", "", MaxDeleteKeys); err != nil || len(page.Objects) != 1 {
		t.Fatalf("other user's prefix: %+v %v", page, err)
	}
}
