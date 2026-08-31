package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
)

type mutationTestServer struct {
	url        string
	client     *http.Client
	token      string
	collection string
	database   *db.DB
	blobDir    string
}

// startMutationTestServer boots a SQLite-backed server with blobs under
// blobDir, logs a dev user in, and claims that user's collection.
func startMutationTestServer(t *testing.T, blobDir string) mutationTestServer {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{
		DatabaseURL:  "sqlite:" + filepath.Join(root, "notes.db"),
		AuthMode:     "dev",
		CookieSecure: false,
		BlobDir:      blobDir,
	}
	database, err := db.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	hub := events.NewHub()
	server := httptest.NewServer(routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, hub,
		events.NewPublisher(database.Dialect(), hub)))
	t.Cleanup(server.Close)
	client := serverClient(server)

	login := doRequest(t, client, http.MethodPost, server.URL+"/api/auth/dev/login",
		`{"email":"mutation-order@example.invalid","name":"Mutation Order"}`, "", nil)
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login = %d %q", login.StatusCode, login.Body)
	}
	var loggedIn struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(login.Body), &loggedIn); err != nil {
		t.Fatal(err)
	}
	claimed := doRequest(t, client, http.MethodPost, server.URL+"/api/collections", "", loggedIn.Token, nil)
	if claimed.StatusCode != http.StatusCreated {
		t.Fatalf("claim = %d %q", claimed.StatusCode, claimed.Body)
	}
	var claimBody struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	if err := json.Unmarshal([]byte(claimed.Body), &claimBody); err != nil {
		t.Fatal(err)
	}
	return mutationTestServer{
		url:        server.URL,
		client:     client,
		token:      loggedIn.Token,
		collection: claimBody.Collection.ID,
		database:   database,
		blobDir:    blobDir,
	}
}

func (s mutationTestServer) post(t *testing.T, path string, body []byte, mutationID string) routeTestResponse {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, s.url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	request.Header.Set("Content-Type", "application/octet-stream")
	if mutationID != "" {
		request.Header.Set("Mutation-Id", mutationID)
	}
	response, err := s.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return routeTestResponse{StatusCode: response.StatusCode, Header: response.Header.Clone(), Body: string(responseBody)}
}

func (s mutationTestServer) blobFileCount(t *testing.T) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(s.blobDir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return count
}

func (s mutationTestServer) stagedLedgerRows(t *testing.T) int {
	t.Helper()
	var count int
	if err := s.database.QueryRowContext(context.Background(),
		`SELECT count(*) FROM blob_ledger WHERE state = 'staged'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func batchCreateEntry(mutationID string, blob []byte) []byte {
	frame := []byte{0}
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(mutationID)))
	frame = append(frame, mutationID...)
	frame = binary.BigEndian.AppendUint32(frame, 0)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(blob)))
	return append(frame, blob...)
}

// A completed create Mutation ID is checked before the replay stages
// ciphertext, so the replay leaves no second blob behind (docs/API.md).
func TestInlineCreateReplayDoesNotStageAnotherBlob(t *testing.T) {
	server := startMutationTestServer(t, filepath.Join(t.TempDir(), "blobs"))
	path := "/api/collections/" + server.collection + "/blob-objects"

	first := server.post(t, path, []byte("ciphertext"), "inline-replay")
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d %q", first.StatusCode, first.Body)
	}
	second := server.post(t, path, []byte("ciphertext"), "inline-replay")
	if second.StatusCode != http.StatusCreated || !strings.Contains(second.Body, `"replayed":true`) {
		t.Fatalf("replayed create = %d %q", second.StatusCode, second.Body)
	}
	if files := server.blobFileCount(t); files != 1 {
		t.Errorf("blob files = %d, want 1", files)
	}
	if staged := server.stagedLedgerRows(t); staged != 0 {
		t.Errorf("staged ledger rows = %d, want 0", staged)
	}
}

func TestBatchCreateReplayDoesNotStageAnotherBlob(t *testing.T) {
	server := startMutationTestServer(t, filepath.Join(t.TempDir(), "blobs"))
	path := "/api/collections/" + server.collection + "/blob-objects/batch"
	frame := batchCreateEntry("batch-replay", []byte("ciphertext"))

	first := server.post(t, path, frame, "")
	if first.StatusCode != http.StatusOK || !strings.Contains(first.Body, `"status":"created"`) {
		t.Fatalf("first batch = %d %q", first.StatusCode, first.Body)
	}
	second := server.post(t, path, frame, "")
	if second.StatusCode != http.StatusOK || !strings.Contains(second.Body, `"status":"replayed"`) {
		t.Fatalf("replayed batch = %d %q", second.StatusCode, second.Body)
	}
	if files := server.blobFileCount(t); files != 1 {
		t.Errorf("blob files = %d, want 1", files)
	}
	if staged := server.stagedLedgerRows(t); staged != 0 {
		t.Errorf("staged ledger rows = %d, want 0", staged)
	}
}

// The in-progress claim is recorded before the ciphertext is staged, so
// recovery reports a pending mutation rather than a definitive 404 for a
// create that has begun staging (docs/API.md). Staging is forced to fail here
// to observe that ordering from outside: the blob directory is replaced with a
// regular file after the server starts, so the store's MkdirAll fails and the
// request stops between the claim and the object row.
func TestInlineCreateRecordsClaimBeforeStaging(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	if err := os.Mkdir(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	server := startMutationTestServer(t, blobDir)
	if err := os.Remove(blobDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	created := server.post(t, "/api/collections/"+server.collection+"/blob-objects", []byte("ciphertext"), "stalled-create")
	if created.StatusCode != http.StatusInternalServerError {
		t.Fatalf("create = %d %q, want 500", created.StatusCode, created.Body)
	}
	recovered := doRequest(t, server.client, http.MethodGet,
		server.url+"/api/collections/"+server.collection+"/create-mutations/stalled-create", "", server.token, nil)
	if recovered.StatusCode != http.StatusConflict || !strings.Contains(recovered.Body, "mutation still in progress") {
		t.Fatalf("recovery = %d %q, want 409 mutation still in progress", recovered.StatusCode, recovered.Body)
	}
}
