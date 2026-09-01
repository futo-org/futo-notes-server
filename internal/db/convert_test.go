package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefuseNonemptyTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.db")
	if err := os.WriteFile(path, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refuseNonemptyTarget(path); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("refuseNonemptyTarget() = %v", err)
	}
}

func TestSQLiteCopyValue(t *testing.T) {
	got, err := sqliteCopyValue([]byte("{ \"status\" : 200 }"), true)
	if err != nil || got != `{"status":200}` {
		t.Fatalf("JSON = %#v, %v", got, err)
	}
	want := time.Date(2026, 8, 25, 12, 0, 0, 123456789, time.UTC)
	got, err = sqliteCopyValue(want, false)
	if err != nil || got != "2026-08-25T12:00:00.123Z" {
		t.Fatalf("timestamp = %#v, %v", got, err)
	}
}
