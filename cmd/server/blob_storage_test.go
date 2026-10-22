package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/blobs/blobstest"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
)

// TestBlobRoutesOnEachBackend runs every route that writes or reads blob
// bytes against each backend: the filesystem always, S3 when
// BLOBS_TEST_S3_ENDPOINT is set. The client sees the same responses on both.
func TestBlobRoutesOnEachBackend(t *testing.T) {
	for _, backend := range blobstest.Backends(t) {
		t.Run(backend.Name, func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Config{
				DatabaseURL: "sqlite:" + filepath.Join(root, "notes.db"),
				AuthMode:    "dev",
				BlobDir:     filepath.Join(root, "blobs"),
			}
			database, err := db.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if _, err := db.Migrate(context.Background(), database); err != nil {
				t.Fatal(err)
			}
			hub := events.NewHub()
			server := httptest.NewServer(routes(cfg, database, backend.Store, hub, events.NewPublisher(database.Dialect(), hub), nil))
			defer server.Close()
			client := serverClient(server)

			login := doRequest(t, client, http.MethodPost, server.URL+"/api/auth/dev/login",
				`{"email":"blob-routes@example.invalid","name":"Blob Routes"}`, "", nil)
			var loggedIn struct {
				Token string `json:"token"`
				User  struct {
					ID string `json:"id"`
				} `json:"user"`
			}
			if err := json.Unmarshal([]byte(login.Body), &loggedIn); err != nil || login.StatusCode != http.StatusOK {
				t.Fatalf("login = %d %q, %v", login.StatusCode, login.Body, err)
			}
			token := loggedIn.Token
			send := func(method, path, body, mutationID string) routeTestResponse {
				t.Helper()
				request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+token)
				if mutationID != "" {
					request.Header.Set("Mutation-Id", mutationID)
				}
				return readResponse(t, client, request)
			}
			download := func(key, want string) {
				t.Helper()
				got := send(http.MethodGet, "/api/blobs/"+key, "", "")
				if got.StatusCode != http.StatusOK || got.Body != want {
					t.Fatalf("GET %s = %d %q, want %q", key, got.StatusCode, got.Body, want)
				}
				if got.Header.Get("Content-Type") != "application/octet-stream" || got.Header.Get("Content-Length") != strconv.Itoa(len(want)) {
					t.Fatalf("GET %s headers = %v", key, got.Header)
				}
				head := send(http.MethodHead, "/api/blobs/"+key, "", "")
				if head.StatusCode != http.StatusOK || head.Body != "" || head.Header.Get("Content-Length") != strconv.Itoa(len(want)) {
					t.Fatalf("HEAD %s = %d %q %v", key, head.StatusCode, head.Body, head.Header)
				}
			}

			staged := send(http.MethodPost, "/api/blobs", "staged ciphertext", "")
			var uploaded struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal([]byte(staged.Body), &uploaded); err != nil || staged.StatusCode != http.StatusCreated {
				t.Fatalf("upload = %d %q, %v", staged.StatusCode, staged.Body, err)
			}
			download(uploaded.Key, "staged ciphertext")

			missing := loggedIn.User.ID + "/0195e2a1-0000-7000-8000-000000000000"
			if got := send(http.MethodGet, "/api/blobs/"+missing, "", ""); got.StatusCode != http.StatusNotFound {
				t.Fatalf("GET missing = %d %q, want 404", got.StatusCode, got.Body)
			}

			claimed := send(http.MethodPost, "/api/collections", "", "")
			var claim struct {
				Collection struct {
					ID string `json:"id"`
				} `json:"collection"`
			}
			if err := json.Unmarshal([]byte(claimed.Body), &claim); err != nil || claimed.StatusCode != http.StatusCreated {
				t.Fatalf("claim = %d %q, %v", claimed.StatusCode, claimed.Body, err)
			}
			objectsPath := "/api/collections/" + claim.Collection.ID + "/blob-objects"
			type objectResponse struct {
				Object struct {
					ID      string `json:"id"`
					BlobKey string `json:"blob_key"`
				} `json:"object"`
			}

			createdResponse := send(http.MethodPost, objectsPath, "first version", "blob-routes-create")
			var created objectResponse
			if err := json.Unmarshal([]byte(createdResponse.Body), &created); err != nil || createdResponse.StatusCode != http.StatusCreated {
				t.Fatalf("create = %d %q, %v", createdResponse.StatusCode, createdResponse.Body, err)
			}
			download(created.Object.BlobKey, "first version")

			updatedResponse := send(http.MethodPut, objectsPath+"/"+created.Object.ID+"?version=2", "second version", "blob-routes-update")
			var updated objectResponse
			if err := json.Unmarshal([]byte(updatedResponse.Body), &updated); err != nil || updatedResponse.StatusCode != http.StatusOK {
				t.Fatalf("update = %d %q, %v", updatedResponse.StatusCode, updatedResponse.Body, err)
			}
			download(updated.Object.BlobKey, "second version")

			keys := []string{created.Object.BlobKey, missing, updated.Object.BlobKey}
			body, _ := json.Marshal(map[string][]string{"keys": keys})
			batch := send(http.MethodPost, "/api/blobs/batch", string(body), "")
			if batch.StatusCode != http.StatusOK {
				t.Fatalf("batch = %d %q", batch.StatusCode, batch.Body)
			}
			frames := decodeFrames(t, []byte(batch.Body))
			want := []frame{
				{created.Object.BlobKey, frameOK, []byte("first version")},
				{missing, frameMissing, []byte{}},
				{updated.Object.BlobKey, frameOK, []byte("second version")},
			}
			if len(frames) != len(want) {
				t.Fatalf("batch frames = %+v", frames)
			}
			for i := range want {
				if frames[i].Key != want[i].Key || frames[i].Status != want[i].Status || !bytes.Equal(frames[i].Blob, want[i].Blob) {
					t.Fatalf("frame %d = %+v, want %+v", i, frames[i], want[i])
				}
			}

			if got := send(http.MethodDelete, "/api/blobs/"+uploaded.Key, "", ""); got.StatusCode != http.StatusNoContent {
				t.Fatalf("DELETE = %d %q", got.StatusCode, got.Body)
			}
			if got := send(http.MethodGet, "/api/blobs/"+uploaded.Key, "", ""); got.StatusCode != http.StatusNotFound {
				t.Fatalf("GET after DELETE = %d %q, want 404", got.StatusCode, got.Body)
			}
		})
	}
}

// fakeS3 serves GetObject from handler, for failures a real server will not
// produce on demand.
func fakeS3(t *testing.T, handler http.HandlerFunc) *blobs.S3Store {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return blobs.NewS3Store(blobs.S3Config{Endpoint: server.URL, Bucket: "blobs", AccessKeyID: "GK", SecretAccessKey: "secret", Region: "us-east-1"})
}

// truncated declares ten bytes, sends five, and drops the connection.
func truncated(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", "10")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("short"))
	w.(http.Flusher).Flush()
	connection, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		connection.Close()
	}
}

// shortStore reports a size its reader does not deliver, with no error.
type shortStore struct{ blobs.Store }

func (shortStore) Open(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader("short")), 10, nil
}

func TestStreamBlobBatchAbortsOnABrokenS3Body(t *testing.T) {
	for _, test := range []struct {
		name  string
		store blobs.Store
	}{
		{"no Content-Length", fakeS3(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			w.Write([]byte("ciphertext"))
		})},
		{"connection dropped partway", fakeS3(t, truncated)},
		{"body shorter than its length", shortStore{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := streamBlobBatch(context.Background(), &out, test.store, fuzzUser, []string{blobKey(fuzzUser, 1)}); err == nil {
				t.Fatalf("streamBlobBatch succeeded with %q", out.Bytes())
			}
		})
	}
}

func downloadRequest(store blobs.Store, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/blobs/"+key, nil)
	userID, blobID, _ := strings.Cut(key, "/")
	r.SetPathValue("userId", userID)
	r.SetPathValue("blobId", blobID)
	r = r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, &auth.Session{ID: "session", User: auth.User{ID: fuzzUser}}))
	w := httptest.NewRecorder()
	handleDownloadBlob(store)(w, r)
	return w
}

func TestDownloadReportsS3FailureAsAServerError(t *testing.T) {
	store := fakeS3(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<Error><Code>InternalError</Code></Error>`))
	})
	if got := downloadRequest(store, blobKey(fuzzUser, 1)); got.Code != http.StatusInternalServerError {
		t.Fatalf("download = %d %q, want 500", got.Code, got.Body.String())
	}
}

func TestDownloadAbortsWhenTheS3BodyBreaks(t *testing.T) {
	store := fakeS3(t, truncated)
	defer func() {
		if recovered := recover(); !errors.Is(recovered.(error), http.ErrAbortHandler) {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", recovered)
		}
	}()
	downloadRequest(store, blobKey(fuzzUser, 1))
	t.Fatal("download finished normally after a broken body")
}
