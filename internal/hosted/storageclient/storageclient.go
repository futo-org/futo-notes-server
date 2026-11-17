// Package storageclient calls the regional storage services' admin endpoints
// (/internal/*, docs/openapi-storage.yaml) for the hosted API's background
// jobs, with admin tokens the API mints for each call.
package storageclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"

	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hostedauth"
)

// MaxDeleteKeys is the most keys one DELETE /internal/blobs call takes.
const MaxDeleteKeys = 1000

const (
	// defaultTimeout bounds one HTTP attempt. A call scans or deletes at most
	// 1000 S3 keys, which takes seconds even on a slow bucket.
	defaultTimeout = 60 * time.Second
	// attempts is how many times a call is tried before its error is
	// returned. Jobs run again on their next interval, so this only rides out
	// a blip such as a storage pod restarting.
	attempts = 3
	// defaultBackoff is the wait before the first retry; it doubles after.
	defaultBackoff = time.Second
	// maxResponseBytes bounds a response body: a full listing page of 1000
	// keys is about 150 KiB.
	maxResponseBytes = 4 << 20
)

// Operations, as recorded in metrics.
const (
	opDeleteBlobs = "delete_blobs"
	opListBlobs   = "list_blobs"
	opPurgeUser   = "purge_user"
)

// Client calls the storage services of the configured regions.
type Client struct {
	endpoints map[string]string
	minter    *hostedauth.Minter
	http      *http.Client
	timeout   time.Duration
	backoff   time.Duration
	calls     otelmetric.Int64Counter
	errors    otelmetric.Int64Counter
	duration  otelmetric.Float64Histogram
}

// durationBuckets are in seconds, up to the attempt timeout.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// New returns a client for regions' storage services, at each region's
// endpoint, signing admin tokens with minter. It records its calls on meter.
func New(regions []config.Region, minter *hostedauth.Minter, meter otelmetric.Meter) (*Client, error) {
	c := &Client{
		endpoints: make(map[string]string, len(regions)),
		minter:    minter,
		http:      &http.Client{},
		timeout:   defaultTimeout,
		backoff:   defaultBackoff,
	}
	for _, region := range regions {
		c.endpoints[region.ID] = region.Endpoint
	}
	var err error
	if c.calls, err = meter.Int64Counter("storage_client.calls",
		otelmetric.WithDescription("Storage admin HTTP attempts by region and operation")); err != nil {
		return nil, fmt.Errorf("creating storage call counter: %w", err)
	}
	if c.errors, err = meter.Int64Counter("storage_client.errors",
		otelmetric.WithDescription("Storage admin HTTP attempts that failed, by region and operation")); err != nil {
		return nil, fmt.Errorf("creating storage error counter: %w", err)
	}
	if c.duration, err = meter.Float64Histogram("storage_client.duration",
		otelmetric.WithDescription("Storage admin HTTP attempt duration by region and operation"), otelmetric.WithUnit("s"),
		otelmetric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		return nil, fmt.Errorf("creating storage duration histogram: %w", err)
	}
	return c, nil
}

// KeyFailure is a key storage could not delete.
type KeyFailure struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// DeleteBlobs deletes keys from region's bucket, MaxDeleteKeys per call. Keys
// already gone count as deleted; it returns the keys storage reported it
// could not delete. On error, keys in calls that did not complete may or may
// not have been deleted.
func (c *Client) DeleteBlobs(ctx context.Context, region string, keys []string) ([]KeyFailure, error) {
	var failed []KeyFailure
	for start := 0; start < len(keys); start += MaxDeleteKeys {
		body, err := json.Marshal(map[string][]string{"keys": keys[start:min(start+MaxDeleteKeys, len(keys))]})
		if err != nil {
			return failed, err
		}
		var response struct {
			Failed []KeyFailure `json:"failed"`
		}
		if err := c.call(ctx, region, opDeleteBlobs, http.MethodDelete, "/internal/blobs", nil, body, &response); err != nil {
			return failed, err
		}
		failed = append(failed, response.Failed...)
	}
	return failed, nil
}

// Blob is a blob in a region's bucket.
type Blob struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
}

// ListBlobs lists region's blobs under prefix ("" for all, or "<userId>/")
// last modified before olderThan, in key order, calling page with each
// non-empty page until the listing ends.
func (c *Client) ListBlobs(ctx context.Context, region, prefix string, olderThan time.Time, page func([]Blob) error) error {
	cursor := ""
	for {
		query := url.Values{"older_than": {olderThan.UTC().Format(time.RFC3339Nano)}}
		if prefix != "" {
			query.Set("prefix", prefix)
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var response struct {
			Blobs      []Blob  `json:"blobs"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := c.call(ctx, region, opListBlobs, http.MethodGet, "/internal/blobs", query, nil, &response); err != nil {
			return err
		}
		// Storage filters by age after S3 returns a page, so a page can be
		// short or empty while the listing goes on.
		if len(response.Blobs) > 0 {
			if err := page(response.Blobs); err != nil {
				return err
			}
		}
		if response.NextCursor == nil {
			return nil
		}
		// The cursor is the last key scanned, and keys list in order.
		if *response.NextCursor <= cursor {
			return fmt.Errorf("listing %s: cursor %q does not advance past %q", region, *response.NextCursor, cursor)
		}
		cursor = *response.NextCursor
	}
}

// PurgeUser deletes every blob under userID's prefix in region, calling
// storage until it reports the prefix empty, and returns how many it deleted.
// Purging an empty prefix succeeds at once.
func (c *Client) PurgeUser(ctx context.Context, region, userID string) (int, error) {
	total := 0
	for {
		var response struct {
			Deleted int  `json:"deleted"`
			Done    bool `json:"done"`
		}
		if err := c.call(ctx, region, opPurgeUser, http.MethodDelete, "/internal/users/"+url.PathEscape(userID), nil, nil, &response); err != nil {
			return total, err
		}
		total += response.Deleted
		if response.Done {
			return total, nil
		}
		if response.Deleted == 0 {
			return total, fmt.Errorf("purging a user in %s: storage reports blobs left but deleted none", region)
		}
	}
}

// StatusError is a storage answer other than 200.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("storage answered %d: %s", e.Status, e.Body)
}

// call runs one admin request, retrying network failures, timeouts, 429s and
// 5xx answers, and decodes a 200 body into out.
func (c *Client) call(ctx context.Context, region, operation, method, path string, query url.Values, body []byte, out any) error {
	endpoint, ok := c.endpoints[region]
	if !ok {
		return fmt.Errorf("region %q is not configured", region)
	}
	target := endpoint + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var err error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return errors.Join(err, ctx.Err())
			case <-time.After(c.backoff << (attempt - 1)):
			}
		}
		var retry bool
		retry, err = c.attempt(ctx, region, operation, method, target, body, out)
		if err == nil || !retry || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("storage %s in %s: %w", operation, region, err)
	}
	return nil
}

// attempt makes one HTTP request and reports whether a failure is worth
// retrying.
func (c *Client) attempt(ctx context.Context, region, operation, method, target string, body []byte, out any) (retry bool, err error) {
	token, err := c.minter.MintAdmin(region)
	if err != nil {
		return false, fmt.Errorf("minting admin token: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	attrs := otelmetric.WithAttributes(attribute.String("region", region), attribute.String("operation", operation))
	recordCtx := context.WithoutCancel(ctx)
	start := time.Now()
	defer func() {
		c.duration.Record(recordCtx, time.Since(start).Seconds(), attrs)
		c.calls.Add(recordCtx, 1, attrs)
		if err != nil {
			c.errors.Add(recordCtx, 1, attrs)
		}
	}()

	resp, err := c.http.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return true, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests,
			&StatusError{Status: resp.StatusCode, Body: string(data)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, fmt.Errorf("decoding response: %w", err)
	}
	return false, nil
}
