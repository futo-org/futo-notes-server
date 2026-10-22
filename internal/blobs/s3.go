package blobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// S3Config names one bucket on an S3-compatible server.
type S3Config struct {
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	Region          string
}

// Bounds on a stalled S3 server, so a request fails instead of hanging. A
// body being streamed to a client is bounded by that request's context
// instead. Variables so a test can shorten them.
var (
	s3DialTimeout           = 5 * time.Second
	s3ResponseHeaderTimeout = 30 * time.Second
)

// S3Store keeps each blob as an object whose key is the storage key, in one
// bucket. It uses only PutObject, GetObject, DeleteObject and HeadBucket, so
// any S3-compatible server will do.
type S3Store struct {
	client *s3.Client
	// Endpoint and Bucket are shown on /dev.
	Endpoint, Bucket string
}

func NewS3Store(cfg S3Config) *S3Store {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: s3DialTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = s3ResponseHeaderTimeout
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cfg.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		HTTPClient:   &http.Client{Transport: transport},
		// Garage, SeaweedFS, Ceph RGW and R2 all take bucket-in-path, and a
		// bare tailnet address has no DNS for bucket-in-host.
		UsePathStyle: true,
		// The SDK's default adds CRC checksums that not every S3-compatible
		// server accepts. Signing already covers the payload.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &S3Store{client: client, Endpoint: cfg.Endpoint, Bucket: cfg.Bucket}
}

// CheckBucket confirms the bucket exists and the credentials reach it. The
// server runs it once at boot and refuses to start without it.
func (s *S3Store) CheckBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.Bucket)})
	return err
}

func (s *S3Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.Bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	return err
}

func (s *S3Store) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key)})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		var response *smithyhttp.ResponseError
		if errors.As(err, &noSuchKey) || errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	// The batch route writes the length into a frame header before the
	// bytes, so a size it cannot trust is a failure, never a zero.
	if out.ContentLength == nil || *out.ContentLength < 0 {
		out.Body.Close()
		return nil, 0, fmt.Errorf("S3 returned no Content-Length for %q", key)
	}
	return out.Body, *out.ContentLength, nil
}

func (s *S3Store) Remove(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key)})
	return err
}
