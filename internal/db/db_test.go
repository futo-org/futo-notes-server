package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/config"
)

func TestSQLiteRebindPreservesPostgresArgumentNumbers(t *testing.T) {
	dialect, err := ParseDialect("sqlite:notes.db")
	if err != nil {
		t.Fatal(err)
	}
	query := `UPDATE objects SET collection_id = $2 WHERE blob_key = $1 OR owner = $2`
	want := `UPDATE objects SET collection_id = ?2 WHERE blob_key = ?1 OR owner = ?2`
	if got := dialect.Rebind(query); got != want {
		t.Fatalf("Rebind() = %q, want %q", got, want)
	}
}

func TestPostgresDialectLeavesQueryAndAddsLocks(t *testing.T) {
	dialect, err := ParseDialect("postgresql://example.test/notes")
	if err != nil {
		t.Fatal(err)
	}
	if got := dialect.Rebind(`SELECT $2, $1`); got != `SELECT $2, $1` {
		t.Fatalf("Rebind() = %q", got)
	}
	if dialect.ForUpdate() != " FOR UPDATE" || dialect.ForUpdateSkipLocked() != " FOR UPDATE SKIP LOCKED" {
		t.Fatal("Postgres lock clauses missing")
	}
	if dialect.JSONCast() != "::jsonb" {
		t.Fatalf("JSONCast() = %q", dialect.JSONCast())
	}
}

func TestFreshSQLiteGuard(t *testing.T) {
	root := t.TempDir()
	blobDir := filepath.Join(root, "blobs")
	if err := os.MkdirAll(filepath.Join(blobDir, "owner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, "owner", "blob"), []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "db", "notes.db")
	err := prepareSQLiteFile(databasePath, blobDir, false)
	if err == nil || !strings.Contains(err.Error(), "ALLOW_FRESH_DATABASE=true") {
		t.Fatalf("prepareSQLiteFile() error = %v, want fresh-database guard", err)
	}
	if err := prepareSQLiteFile(databasePath, blobDir, true); err != nil {
		t.Fatalf("override: %v", err)
	}
	if err := os.WriteFile(databasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareSQLiteFile(databasePath, blobDir, false); err == nil {
		t.Fatal("zero-byte database bypassed the fresh-database guard")
	}
}

// In S3 mode BLOB_DIR is unused, so leftover files in it say nothing about
// whether DATABASE_URL was lost.
func TestFreshSQLiteGuardStandsDownInS3Mode(t *testing.T) {
	root := t.TempDir()
	blobDir := filepath.Join(root, "blobs")
	if err := os.MkdirAll(filepath.Join(blobDir, "owner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, "owner", "blob"), []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DatabaseURL: "sqlite:" + filepath.Join(root, "notes.db"), BlobDir: blobDir}
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open() on the filesystem backend skipped the fresh-database guard")
	}
	cfg.S3Endpoint = "http://100.76.177.70:3900"
	database, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() in S3 mode: %v", err)
	}
	database.Close()
}

func TestTimestampRoundTrip(t *testing.T) {
	want := time.Date(2026, 8, 25, 12, 34, 56, 987654321, time.FixedZone("offset", -5*60*60))
	encoded := Timestamp(want)
	if encoded != "2026-08-25T17:34:56.987Z" {
		t.Fatalf("Timestamp() = %q", encoded)
	}
	var decoded Time
	if err := decoded.Scan(encoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Format(TimestampFormat) != encoded {
		t.Fatalf("decoded = %s", decoded.Time)
	}
}
