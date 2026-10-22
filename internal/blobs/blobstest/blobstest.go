// Package blobstest gives tests every blob backend they can reach: always a
// FileStore, plus an S3Store when BLOBS_TEST_S3_ENDPOINT is set, the way the
// Postgres tests run only when their *_TEST_DATABASE_URL is.
package blobstest

import (
	"os"
	"testing"

	"futo-notes-server/internal/blobs"
)

type Backend struct {
	Name  string
	Store blobs.Store
}

// Backends returns a fresh FileStore and, when configured, the S3 bucket
// named by BLOBS_TEST_S3_ENDPOINT, BLOBS_TEST_S3_BUCKET,
// BLOBS_TEST_S3_ACCESS_KEY_ID and BLOBS_TEST_S3_SECRET_ACCESS_KEY. Keys are
// minted UUIDv7s, so tests sharing the bucket never collide.
func Backends(t testing.TB) []Backend {
	t.Helper()
	backends := []Backend{{Name: "filesystem", Store: &blobs.FileStore{Dir: t.TempDir()}}}
	endpoint := os.Getenv("BLOBS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Log("BLOBS_TEST_S3_ENDPOINT is not set; skipping the S3 backend")
		return backends
	}
	return append(backends, Backend{Name: "S3", Store: blobs.NewS3Store(blobs.S3Config{
		Endpoint:        endpoint,
		Bucket:          os.Getenv("BLOBS_TEST_S3_BUCKET"),
		AccessKeyID:     os.Getenv("BLOBS_TEST_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("BLOBS_TEST_S3_SECRET_ACCESS_KEY"),
		Region:          "us-east-1",
	})})
}
