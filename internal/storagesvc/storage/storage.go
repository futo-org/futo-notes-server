// Package storage is the storage service's view of its regional S3 bucket.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	// ErrNotFound reports an object that does not exist.
	ErrNotFound = errors.New("object not found")
	// ErrExists reports a create-only write to a key that is already taken.
	ErrExists = errors.New("object already exists")
	// ErrBody reports an upload whose body failed or ended before its
	// declared size; nothing was stored.
	ErrBody = errors.New("upload body ended early or failed")
)

// RangeError reports a byte range that does not overlap the object.
type RangeError struct {
	// Size is the object's full length.
	Size int64
}

func (e *RangeError) Error() string {
	return fmt.Sprintf("range not satisfiable for an object of %d bytes", e.Size)
}

// Object is an open object, or the requested range of one.
type Object struct {
	Body io.ReadCloser
	// Length is the number of bytes Body yields.
	Length int64
	// ContentRange is set, as "bytes first-last/size", when Body is a range.
	ContentRange string
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ListPage is one page of a listing in key order.
type ListPage struct {
	Objects []ObjectInfo
	// LastKey is the last key the page scanned, which resumes the listing.
	LastKey string
	// Truncated reports that keys after LastKey remain.
	Truncated bool
}

// DeleteError is a key a batch delete failed to remove.
type DeleteError struct {
	Key     string
	Message string
}

// Storage is the regional bucket. Keys are "<userId>/<blobId>".
type Storage interface {
	// PutObject stores exactly size bytes from body at key, failing with
	// ErrExists when the key is taken. A body that ends early or fails
	// stores nothing and fails with ErrBody.
	PutObject(ctx context.Context, key string, body io.Reader, size int64) error
	// GetObject opens key, or the single range byteRange ("bytes=a-b",
	// "bytes=a-" or "bytes=-n") of it when non-empty. It fails with
	// ErrNotFound or *RangeError.
	GetObject(ctx context.Context, key, byteRange string) (*Object, error)
	// ListObjects lists up to limit keys under prefix that sort after
	// startAfter.
	ListObjects(ctx context.Context, prefix, startAfter string, limit int) (ListPage, error)
	// DeleteObjects removes up to 1000 keys. Keys already gone count as
	// removed; the result lists only keys that could not be removed.
	DeleteObjects(ctx context.Context, keys []string) ([]DeleteError, error)
}

// MaxDeleteKeys is the most keys one DeleteObjects call accepts, S3's limit.
const MaxDeleteKeys = 1000
