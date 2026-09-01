package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
)

func TestSQLiteRoutesEndToEnd(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		DatabaseURL:  "sqlite:" + filepath.Join(root, "notes.db"),
		AuthMode:     "dev",
		CookieSecure: false,
		BlobDir:      filepath.Join(root, "blobs"),
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
	server := httptest.NewServer(routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, hub,
		events.NewPublisher(database.Dialect(), hub)))
	defer server.Close()
	client := serverClient(server)

	login := doRequest(t, client, http.MethodPost, server.URL+"/api/auth/dev/login",
		`{"email":"sqlite-routes@example.invalid","name":"SQLite Routes"}`, "", nil)
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

	uploadRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/blobs", strings.NewReader("ciphertext"))
	if err != nil {
		t.Fatal(err)
	}
	uploadRequest.Header.Set("Authorization", "Bearer "+loggedIn.Token)
	uploadRequest.Header.Set("Content-Type", "application/octet-stream")
	uploadResponse, err := client.Do(uploadRequest)
	if err != nil {
		t.Fatal(err)
	}
	uploadBody, err := io.ReadAll(uploadResponse.Body)
	uploadResponse.Body.Close()
	if err != nil || uploadResponse.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d %q, %v", uploadResponse.StatusCode, uploadBody, err)
	}
	var uploaded struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(uploadBody, &uploaded); err != nil {
		t.Fatal(err)
	}

	createRequest, err := http.NewRequest(http.MethodPost,
		server.URL+"/api/collections/"+claimBody.Collection.ID+"/objects",
		strings.NewReader(`{"blob_key":"`+uploaded.Key+`","size_bytes":10}`))
	if err != nil {
		t.Fatal(err)
	}
	createRequest.Header.Set("Authorization", "Bearer "+loggedIn.Token)
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("Mutation-Id", "sqlite-http-create")
	createResponse, err := client.Do(createRequest)
	if err != nil {
		t.Fatal(err)
	}
	createBody, err := io.ReadAll(createResponse.Body)
	createResponse.Body.Close()
	if err != nil || createResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %q, %v", createResponse.StatusCode, createBody, err)
	}

	listed := doRequest(t, client, http.MethodGet,
		server.URL+"/api/collections/"+claimBody.Collection.ID+"/objects", "", loggedIn.Token, nil)
	if listed.StatusCode != http.StatusOK || !strings.Contains(listed.Body, uploaded.Key) {
		t.Fatalf("list = %d %q", listed.StatusCode, listed.Body)
	}
}
