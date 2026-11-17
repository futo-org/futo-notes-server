package hosted

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"futo-notes-server/internal/blobframe"
)

const (
	requestTimeout = 30 * time.Second
	eventDeadline  = 15 * time.Second
	// appRedirect is where the app's loopback listener would be. The
	// scenario reads the code off the API's redirect and never connects.
	appRedirect = "http://127.0.0.1:1/callback"
)

// device is one signed-in app install: its own cookie jar for the sign-in
// round trip, its session token and its storage refresh token.
type device struct {
	t         *testing.T
	s         *stack
	sub       string
	userAgent string
	client    *http.Client
	token     string
	refresh   string
	userID    string
}

// login signs a new device in as sub the way a native app does: start at the
// API with its own PKCE challenge, sign in at the issuer (through the mock's
// no-UI shortcut), follow the callback to the app redirect, and swap the
// one-time code and verifier for the session.
func (s *stack) login(t *testing.T, sub, userAgent string) *device {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &device{t: t, s: s, sub: sub, userAgent: userAgent, client: &http.Client{
		Jar:           jar,
		Timeout:       requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}

	verifier := randomToken(t)
	challenge := oauth2.S256ChallengeFromVerifier(verifier)
	state := randomToken(t)
	start := s.apiURL + "/api/auth/oidc/start?" + url.Values{
		"redirect_uri":          {appRedirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}.Encode()

	authorize := d.redirect(start)
	if !strings.HasPrefix(authorize.String(), s.issuer+"/authorize?") {
		t.Fatalf("sign-in start redirected to %s, want the issuer's /authorize", authorize)
	}
	authorize.Path = "/test/authorize"
	q := authorize.Query()
	q.Set("sub", sub)
	q.Set("email", sub+"@example.test")
	q.Set("name", "E2E "+sub)
	authorize.RawQuery = q.Encode()

	callback := d.redirect(authorize.String())
	if !strings.HasPrefix(callback.String(), s.apiURL+"/api/auth/oidc/callback?") {
		t.Fatalf("issuer redirected to %s, want the API callback", callback)
	}
	back := d.redirect(callback.String())
	if got := back.Scheme + "://" + back.Host + back.Path; got != appRedirect {
		t.Fatalf("callback redirected to %s, want %s", back, appRedirect)
	}
	if back.Query().Get("state") != state || back.Query().Get("code") == "" {
		t.Fatalf("app redirect %s lacks the code or the app's state", back)
	}

	var session struct {
		Token               string `json:"token"`
		StorageRefreshToken string `json:"storage_refresh_token"`
		User                struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	d.expectJSON(http.MethodPost, "/api/auth/oidc/token",
		map[string]string{"code": back.Query().Get("code"), "code_verifier": verifier}, &session)
	if session.Token == "" || session.StorageRefreshToken == "" || session.User.ID == "" {
		t.Fatalf("token exchange returned %+v", session)
	}
	d.token, d.refresh, d.userID = session.Token, session.StorageRefreshToken, session.User.ID
	return d
}

// redirect GETs target and returns where its 302 points.
func (d *device) redirect(target string) *url.URL {
	d.t.Helper()
	resp, err := d.client.Get(target)
	if err != nil {
		d.t.Fatalf("GET %s: %v", target, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		d.t.Fatalf("GET %s: status %d, want 302: %s", target, resp.StatusCode, body)
	}
	location, err := resp.Location()
	if err != nil {
		d.t.Fatalf("GET %s: %v", target, err)
	}
	return location
}

func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// request is one HTTP call a device makes.
type request struct {
	method string
	url    string
	// bearer is the Authorization token; "" sends none.
	bearer     string
	body       io.Reader
	length     int64
	mutationID string
	// contentType is sent when set; a body without a length is JSON.
	contentType string
}

type response struct {
	status int
	body   []byte
}

func (d *device) do(r request) response {
	d.t.Helper()
	req, err := http.NewRequest(r.method, r.url, r.body)
	if err != nil {
		d.t.Fatal(err)
	}
	req.ContentLength = r.length
	req.Header.Set("User-Agent", d.userAgent)
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.mutationID != "" {
		req.Header.Set("Mutation-Id", r.mutationID)
	}
	if r.body != nil && r.length == 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", r.method, r.url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		d.t.Fatalf("%s %s: reading body: %v", r.method, r.url, err)
	}
	return response{status: resp.StatusCode, body: body}
}

// api calls an API path with the device's session token and a JSON body
// (nil for none).
func (d *device) api(method, path string, body any, mutationID string) response {
	d.t.Helper()
	r := request{method: method, url: d.s.apiURL + path, bearer: d.token, mutationID: mutationID}
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			d.t.Fatal(err)
		}
		r.body = bytes.NewReader(data)
	}
	return d.do(r)
}

// expect fails the test unless resp has status.
func (d *device) expect(what string, resp response, status int) {
	d.t.Helper()
	if resp.status != status {
		d.t.Fatalf("%s: status %d, want %d: %s", what, resp.status, status, resp.body)
	}
}

// expectJSON calls an API path and decodes its body into out (nil to skip),
// failing unless the status is 200.
func (d *device) expectJSON(method, path string, body, out any) {
	d.t.Helper()
	resp := d.api(method, path, body, "")
	d.expect(method+" "+path, resp, http.StatusOK)
	decode(d.t, resp, out)
}

func decode(t *testing.T, resp response, out any) {
	t.Helper()
	if out == nil {
		return
	}
	if err := json.Unmarshal(resp.body, out); err != nil {
		t.Fatalf("decoding %s: %v", resp.body, err)
	}
}

// storageGrant is the body of POST /api/storage/token.
type storageGrant struct {
	AccessToken  string    `json:"access_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	RefreshToken string    `json:"refresh_token"`
	Region       struct {
		ID       string `json:"id"`
		Endpoint string `json:"endpoint"`
	} `json:"region"`
}

// exchange spends refresh at POST /api/storage/token.
func (d *device) exchange(refresh string) response {
	d.t.Helper()
	data, err := json.Marshal(map[string]string{"refresh_token": refresh})
	if err != nil {
		d.t.Fatal(err)
	}
	return d.do(request{method: http.MethodPost, url: d.s.apiURL + "/api/storage/token", body: bytes.NewReader(data)})
}

// storageToken exchanges the device's refresh token for an access token and
// keeps the replacement refresh token.
func (d *device) storageToken() storageGrant {
	d.t.Helper()
	resp := d.exchange(d.refresh)
	d.expect("storage token", resp, http.StatusOK)
	var grant storageGrant
	decode(d.t, resp, &grant)
	if grant.AccessToken == "" || grant.RefreshToken == "" || grant.RefreshToken == d.refresh {
		d.t.Fatalf("storage token response %s", resp.body)
	}
	d.refresh = grant.RefreshToken
	return grant
}

// upload is the body of a storage POST /blobs.
type upload struct {
	Key     string `json:"key"`
	Size    int64  `json:"size"`
	Receipt string `json:"receipt"`
}

// upload stores data at the grant's region and returns the receipt.
func (d *device) upload(grant storageGrant, data []byte) upload {
	d.t.Helper()
	resp := d.do(request{method: http.MethodPost, url: grant.Region.Endpoint + "/blobs", bearer: grant.AccessToken,
		body: bytes.NewReader(data), length: int64(len(data))})
	d.expect("upload", resp, http.StatusCreated)
	var u upload
	decode(d.t, resp, &u)
	if !strings.HasPrefix(u.Key, d.userID+"/") || u.Size != int64(len(data)) || u.Receipt == "" {
		d.t.Fatalf("upload returned %s", resp.body)
	}
	return u
}

// uploadBatch stores blobs at the grant's region in one batch upload, the
// frame of blobs[i] named ids[i], and returns their receipts in order.
func (d *device) uploadBatch(grant storageGrant, ids []string, blobs [][]byte) []upload {
	d.t.Helper()
	var body []byte
	for i, blob := range blobs {
		body = blobframe.AppendUpload(body, ids[i], blob)
	}
	resp := d.do(request{method: http.MethodPost, url: grant.Region.Endpoint + "/blobs/batch-upload", bearer: grant.AccessToken,
		body: bytes.NewReader(body), length: int64(len(body)), contentType: "application/octet-stream"})
	d.expect("batch upload", resp, http.StatusOK)
	var answer struct {
		Results []struct {
			ID     string `json:"id"`
			Status int    `json:"status"`
			Body   upload `json:"body"`
		} `json:"results"`
	}
	decode(d.t, resp, &answer)
	if len(answer.Results) != len(blobs) {
		d.t.Fatalf("batch upload of %d blobs answered %d results", len(blobs), len(answer.Results))
	}
	uploads := make([]upload, 0, len(blobs))
	for i, r := range answer.Results {
		u := r.Body
		if r.ID != ids[i] || r.Status != http.StatusCreated || !strings.HasPrefix(u.Key, d.userID+"/") ||
			u.Size != int64(len(blobs[i])) || u.Receipt == "" {
			d.t.Fatalf("batch upload frame %d (%s): %d %+v", i, ids[i], r.Status, u)
		}
		uploads = append(uploads, u)
	}
	return uploads
}

// batchResult is one entry's result from the API's object batch.
type batchResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// claimBatch sends entries to the collection's object batch and returns
// their results.
func (d *device) claimBatch(collectionID string, entries []map[string]any) []batchResult {
	d.t.Helper()
	resp := d.api(http.MethodPost, objectPath(collectionID, "")+"/batch", map[string]any{"entries": entries}, "")
	d.expect("object batch", resp, http.StatusOK)
	var answer struct {
		Results []batchResult `json:"results"`
	}
	decode(d.t, resp, &answer)
	if len(answer.Results) != len(entries) {
		d.t.Fatalf("object batch of %d entries answered %d results", len(entries), len(answer.Results))
	}
	return answer.Results
}

// download GETs key from the grant's region.
func (d *device) download(grant storageGrant, key string) []byte {
	d.t.Helper()
	resp := d.do(request{method: http.MethodGet, url: grant.Region.Endpoint + "/blobs/" + key, bearer: grant.AccessToken})
	d.expect("download "+key, resp, http.StatusOK)
	return resp.body
}

// downloadBatch fetches keys from the grant's region in one batch and
// returns the decoded frames.
func (d *device) downloadBatch(grant storageGrant, keys ...string) []blobframe.Frame {
	d.t.Helper()
	data, err := json.Marshal(map[string][]string{"keys": keys})
	if err != nil {
		d.t.Fatal(err)
	}
	resp := d.do(request{method: http.MethodPost, url: grant.Region.Endpoint + "/blobs/batch", bearer: grant.AccessToken,
		body: bytes.NewReader(data)})
	d.expect("batch download", resp, http.StatusOK)
	var frames []blobframe.Frame
	r := bytes.NewReader(resp.body)
	for {
		f, err := blobframe.ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			d.t.Fatalf("reading batch frame %d: %v", len(frames), err)
		}
		frames = append(frames, f)
	}
}

// object is an object as the sync contract returns it.
type object struct {
	ID        string  `json:"id"`
	Version   string  `json:"version"`
	ChangeSeq string  `json:"change_seq"`
	Deleted   bool    `json:"deleted"`
	BlobKey   *string `json:"blob_key"`
	SizeBytes *string `json:"size_bytes"`
}

// mutation is the body of an object create or update.
type mutation struct {
	Object            object `json:"object"`
	CollectionVersion int64  `json:"collectionVersion"`
	Replayed          *bool  `json:"replayed"`
}

// claimCollection returns the device's collection, creating it if needed.
func (d *device) claimCollection() string {
	d.t.Helper()
	resp := d.api(http.MethodPost, "/api/collections", nil, "")
	if resp.status != http.StatusCreated && resp.status != http.StatusOK {
		d.t.Fatalf("claim collection: status %d: %s", resp.status, resp.body)
	}
	var body struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	decode(d.t, resp, &body)
	return body.Collection.ID
}

// event is one server-sent event.
type event struct {
	name string
	data string
}

// events is a device's open sync event stream.
type events struct {
	t  *testing.T
	ch chan event
}

// subscribe opens the device's sync event stream and waits for its ready
// event, after which every change is delivered. The stream closes when the
// test ends.
func (d *device) subscribe() *events {
	d.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.s.apiURL+"/api/sync/events", nil)
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("User-Agent", d.userAgent)
	// No client timeout: it would cut the stream.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		d.t.Fatalf("opening sync events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		d.t.Fatalf("opening sync events: status %d: %s", resp.StatusCode, body)
	}
	e := &events{t: d.t, ch: make(chan event, 16)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(e.ch)
		scanner := bufio.NewScanner(resp.Body)
		var current event
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				e.ch <- current
				current = event{}
			case strings.HasPrefix(line, "event: "):
				current.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				current.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	d.t.Cleanup(func() {
		cancel()
		resp.Body.Close()
		for range e.ch {
		}
		<-done
	})
	e.next("ready")
	return e
}

// next returns the next event named name, skipping pings, and fails the test
// if another event or none arrives within eventDeadline.
func (e *events) next(name string) event {
	e.t.Helper()
	deadline := time.After(eventDeadline)
	for {
		select {
		case ev, open := <-e.ch:
			if !open {
				e.t.Fatalf("sync event stream closed while waiting for %q", name)
			}
			if ev.name == "ping" {
				continue
			}
			if ev.name != name {
				e.t.Fatalf("sync event %q (%s), want %q", ev.name, ev.data, name)
			}
			return ev
		case <-deadline:
			e.t.Fatalf("no %q sync event within %s", name, eventDeadline)
		}
	}
}

// runJob triggers one of the API's DEV_UI job endpoints and decodes its
// result.
func (s *stack) runJob(t *testing.T, path string, out any) {
	t.Helper()
	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Post(s.apiURL+path, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("POST %s: decoding %s: %v", path, body, err)
	}
	t.Logf("%s: %s", path, strings.TrimSpace(string(body)))
}

func objectPath(collectionID, objectID string) string {
	path := fmt.Sprintf("/api/collections/%s/objects", collectionID)
	if objectID != "" {
		path += "/" + objectID
	}
	return path
}
