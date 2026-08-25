package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestMigrateToSQLiteRequiresPostgresSource(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "sqlite:source.db")
	var stdout, stderr bytes.Buffer
	if code := runMigrateToSQLite(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires DATABASE_URL to be postgres") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
