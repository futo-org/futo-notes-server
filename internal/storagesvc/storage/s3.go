package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"futo-notes-server/internal/storagesvc/adminapi"
)

// Observer is told how each S3 call went. failed is false for the answers a
// caller expects (missing key, taken key, bad range, caller gone), so it
// counts backend trouble only.
type Observer interface {
	ObserveS3(ctx context.Context, operation string, duration time.Duration, failed bool)
}

// S3Options configures an S3Storage.
type S3Options struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
	// Observer may be nil.
	Observer Observer
}

// S3Storage is a Storage backed by one S3 bucket.
type S3Storage struct {
	client   *s3.Client
	bucket   string
	observer Observer
}

// NewS3Storage returns an S3Storage for opts.Bucket.
func NewS3Storage(opts S3Options) *S3Storage {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The default per-host idle cap of 2 makes every concurrent blob past the
	// second pay a fresh handshake to the single S3 endpoint.
	transport.MaxIdleConns = 0
	transport.MaxIdleConnsPerHost = 128
	transport.IdleConnTimeout = 120 * time.Second
	transport.WriteBufferSize = 64 << 10
	transport.ReadBufferSize = 64 << 10

	client := s3.New(s3.Options{
		Region:       opts.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(opts.AccessKeyID, opts.SecretAccessKey, ""),
		BaseEndpoint: aws.String(opts.Endpoint),
		UsePathStyle: opts.ForcePathStyle,
		// No SDK retries: an upload body is the client's request stream and
		// cannot be rewound, and a failed rewind would mask the real error.
		// Every caller (apps, the API's jobs) retries on its own.
		RetryMaxAttempts: 1,
		// Flexible checksums would send uploads aws-chunked with a trailer,
		// which not every S3-compatible store accepts; TLS covers transit.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		HTTPClient:                 &http.Client{Transport: transport},
	})
	return &S3Storage{client: client, bucket: opts.Bucket, observer: opts.Observer}
}

func (s *S3Storage) observe(ctx context.Context, operation string, start time.Time, err error) {
	if s.observer == nil {
		return
	}
	failed := err != nil &&
		!errors.Is(err, ErrNotFound) &&
		!errors.Is(err, ErrExists) &&
		!errors.Is(err, ErrBody) &&
		!errors.Is(err, context.Canceled)
	if _, ok := errors.AsType[*RangeError](err); ok {
		failed = false
	}
	s.observer.ObserveS3(ctx, operation, time.Since(start), failed)
}

// PutObject implements Storage.
func (s *S3Storage) PutObject(ctx context.Context, key string, body io.Reader, size int64) (err error) {
	defer func(start time.Time) { s.observe(ctx, "put", start, err) }(time.Now())
	tracked := &trackedReader{r: body}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          tracked,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/octet-stream"),
		IfNoneMatch:   aws.String("*"),
	},
		// Signing the payload hash would need a seekable body; the request
		// stream is not, and is read exactly once.
		s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware),
	)
	if err != nil {
		if bodyErr := tracked.failure(size); bodyErr != nil {
			return bodyErr
		}
		if statusCode(err) == http.StatusPreconditionFailed {
			return ErrExists
		}
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

// trackedReader remembers how an upload body ended, so a PUT that failed
// because the client's stream did can be told apart from an S3 failure.
type trackedReader struct {
	r   io.Reader
	n   int64
	err error
}

func (t *trackedReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.n += int64(n)
	if err != nil {
		t.err = err
	}
	return n, err
}

func (t *trackedReader) failure(size int64) error {
	switch {
	case t.err != nil && !errors.Is(t.err, io.EOF):
		return fmt.Errorf("%w: %w", ErrBody, t.err)
	case errors.Is(t.err, io.EOF) && t.n < size:
		return fmt.Errorf("%w: got %d of %d bytes", ErrBody, t.n, size)
	}
	return nil
}

// GetObject implements Storage.
func (s *S3Storage) GetObject(ctx context.Context, key, byteRange string) (obj *Object, err error) {
	defer func(start time.Time) { s.observe(ctx, "get", start, err) }(time.Now())
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if byteRange != "" {
		input.Range = aws.String(byteRange)
	}
	out, err := s.client.GetObject(ctx, input)
	if err != nil {
		switch {
		case isNotFound(err):
			return nil, ErrNotFound
		case statusCode(err) == http.StatusRequestedRangeNotSatisfiable:
			return nil, s.rangeError(ctx, key)
		}
		return nil, fmt.Errorf("get object: %w", err)
	}
	return &Object{
		Body:         out.Body,
		Length:       aws.ToInt64(out.ContentLength),
		ContentRange: aws.ToString(out.ContentRange),
	}, nil
}

// rangeError looks up the object's size for the 416's Content-Range, which
// S3 leaves out of its error.
func (s *S3Storage) rangeError(ctx context.Context, key string) (err error) {
	defer func(start time.Time) { s.observe(ctx, "head", start, err) }(time.Now())
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("head object: %w", err)
	}
	return &RangeError{Size: aws.ToInt64(out.ContentLength)}
}

// ListObjects implements Storage.
func (s *S3Storage) ListObjects(ctx context.Context, prefix, startAfter string, limit int) (page ListPage, err error) {
	defer func(start time.Time) { s.observe(ctx, "list", start, err) }(time.Now())
	input := &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		MaxKeys: aws.Int32(int32(limit)),
	}
	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}
	if startAfter != "" {
		input.StartAfter = aws.String(startAfter)
	}
	out, err := s.client.ListObjectsV2(ctx, input)
	if err != nil {
		return ListPage{}, fmt.Errorf("list objects: %w", err)
	}
	page.Truncated = aws.ToBool(out.IsTruncated)
	page.Objects = make([]ObjectInfo, 0, len(out.Contents))
	for _, o := range out.Contents {
		page.Objects = append(page.Objects, ObjectInfo{
			Key:          aws.ToString(o.Key),
			Size:         aws.ToInt64(o.Size),
			LastModified: aws.ToTime(o.LastModified).UTC(),
		})
	}
	if n := len(page.Objects); n > 0 {
		page.LastKey = page.Objects[n-1].Key
	}
	return page, nil
}

// DeleteObjects implements Storage.
func (s *S3Storage) DeleteObjects(ctx context.Context, keys []string) (failed []DeleteError, err error) {
	defer func(start time.Time) { s.observe(ctx, "delete", start, err) }(time.Now())
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > adminapi.MaxDeleteKeys {
		return nil, fmt.Errorf("delete objects: %d keys, at most %d", len(keys), adminapi.MaxDeleteKeys)
	}
	objects := make([]types.ObjectIdentifier, len(keys))
	for i, key := range keys {
		objects[i] = types.ObjectIdentifier{Key: aws.String(key)}
	}
	out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(s.bucket),
		Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return nil, fmt.Errorf("delete objects: %w", err)
	}
	for _, e := range out.Errors {
		// S3 reports a missing key as deleted, but some compatible stores
		// answer NoSuchKey; either way the key is gone.
		if aws.ToString(e.Code) == "NoSuchKey" {
			continue
		}
		failed = append(failed, DeleteError{
			Key:     aws.ToString(e.Key),
			Message: aws.ToString(e.Code) + ": " + aws.ToString(e.Message),
		})
	}
	return failed, nil
}

// isNotFound reports a missing object. A missing bucket is a deployment
// error, not an absent blob, so it is excluded.
func isNotFound(err error) bool {
	if _, ok := errors.AsType[*types.NoSuchBucket](err); ok {
		return false
	}
	if _, ok := errors.AsType[*types.NoSuchKey](err); ok {
		return true
	}
	if _, ok := errors.AsType[*types.NotFound](err); ok {
		return true
	}
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok && apiErr.ErrorCode() == "NoSuchBucket" {
		return false
	}
	return statusCode(err) == http.StatusNotFound
}

func statusCode(err error) int {
	if e, ok := errors.AsType[interface {
		error
		HTTPStatusCode() int
	}](err); ok {
		return e.HTTPStatusCode()
	}
	return 0
}
