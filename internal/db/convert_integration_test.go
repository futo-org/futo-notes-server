package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/collections"
	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/jobs"
	"futo-notes-server/internal/objects"
	"uuid"
)

func TestMigratePostgresToSQLitePreservesApplicationState(t *testing.T) {
	sourceURL := os.Getenv("MIGRATION_TEST_DATABASE_URL")
	if sourceURL == "" {
		t.Skip("MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	source, err := db.Open(config.Config{DatabaseURL: sourceURL})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := db.Migrate(ctx, source); err != nil {
		t.Fatal(err)
	}

	runID := uuid.NewV7().String()
	user, err := auth.UpsertUserByEmail(ctx, source, "migration-"+runID+"@example.invalid", "Migration Fixture")
	if err != nil {
		t.Fatal(err)
	}
	configKey := "migration-test-" + runID
	defer func() {
		_, _ = source.ExecContext(context.Background(), `DELETE FROM server_config WHERE key = $1`, configKey)
		_, _ = source.ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, user.ID)
	}()

	collection, created, err := collections.Claim(ctx, source, user.ID)
	if err != nil || !created {
		t.Fatalf("Claim() = %#v, %t, %v", collection, created, err)
	}
	wantKDF := json.RawMessage(`{"z":1,"a":2,"z":3}`)
	if outcome, _, err := collections.PutKey(ctx, source, user.ID, collection.ID, collections.KeyInput{
		KeySalt: "migration-salt", KeyKDF: wantKDF, EncryptedVaultKey: "migration-wrapped-key",
	}, nil); err != nil || outcome != collections.PutKeyOK {
		t.Fatalf("PutKey() = %v, %v", outcome, err)
	}
	token, err := auth.CreateSession(ctx, source, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	store := &blobs.Store{Dir: filepath.Join(t.TempDir(), "blobs")}
	blobKey, err := blobs.Stage(ctx, source, store, user.ID, []byte("migration encrypted payload"))
	if err != nil {
		t.Fatal(err)
	}
	createdObject, err := objects.Create(ctx, source, events.PostgresPublisher{}, user.ID, collection.ID, "migration-create", blobKey)
	if err != nil || createdObject.Code != objects.OK {
		t.Fatalf("Create() = %#v, %v", createdObject, err)
	}
	if _, err := source.ExecContext(ctx, `INSERT INTO server_config (key, value) VALUES ($1, $2)`, configKey, "migration-value"); err != nil {
		t.Fatal(err)
	}

	targetPath := filepath.Join(t.TempDir(), "notes.db")
	var summary bytes.Buffer
	if err := db.MigratePostgresToSQLite(ctx, sourceURL, "sqlite:"+targetPath, &summary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary.String(), "row counts, collection versions, blob bytes, integrity, foreign keys: ok") {
		t.Fatalf("migration summary did not report verification:\n%s", summary.String())
	}

	target, err := db.Open(config.Config{
		DatabaseURL: "sqlite:" + targetPath,
		BlobDir:     store.Dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if session, err := auth.ValidateSession(ctx, target, token); err != nil || session == nil || session.User.ID != user.ID {
		t.Fatalf("ValidateSession() = %#v, %v", session, err)
	}
	found, key, err := collections.GetKey(ctx, target, user.ID, collection.ID)
	if err != nil || !found || key == nil {
		t.Fatalf("GetKey() = %t, %#v, %v", found, key, err)
	}
	var wantKDFValue, gotKDFValue any
	if err := json.Unmarshal(wantKDF, &wantKDFValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(key.KeyKDF, &gotKDFValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotKDFValue, wantKDFValue) {
		t.Fatalf("key KDF = %s, want semantic JSON %s", key.KeyKDF, wantKDF)
	}
	gotObject, err := objects.Get(ctx, target, user.ID, collection.ID, createdObject.Response.Object.ID)
	if err != nil || gotObject == nil || gotObject.BlobKey == nil || *gotObject.BlobKey != blobKey {
		t.Fatalf("Get() = %#v, %v", gotObject, err)
	}
	recovered, err := objects.RecoverCreate(ctx, target, user.ID, collection.ID, "migration-create")
	if err != nil || recovered.Code != objects.OK || recovered.Response.Object.ID != createdObject.Response.Object.ID {
		t.Fatalf("RecoverCreate() = %#v, %v", recovered, err)
	}
	var configValue string
	if err := target.QueryRowContext(ctx, `SELECT value FROM server_config WHERE key = $1`, configKey).Scan(&configValue); err != nil || configValue != "migration-value" {
		t.Fatalf("server_config value = %q, %v", configValue, err)
	}
	file, err := store.Open(blobKey)
	if err != nil {
		t.Fatalf("opening unchanged blob: %v", err)
	}
	_ = file.Close()
	if _, err := jobs.ExpireMutationResults(ctx, target); err != nil {
		t.Fatalf("running retention after migration: %v", err)
	}
	if sourceObject, err := objects.Get(ctx, source, user.ID, collection.ID, createdObject.Response.Object.ID); err != nil || sourceObject == nil {
		t.Fatalf("source was modified: object=%#v err=%v", sourceObject, err)
	}
}
