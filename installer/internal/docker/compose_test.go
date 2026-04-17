package docker

import (
	"os"
	"path/filepath"
	"testing"

	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/config"
)

func TestGenerateAndParseCompose_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := config.Config{
		Port:             4242,
		DataPath:         "./my-notes",
		PostgresPassword: "deadbeef1234",
	}
	if err := WriteCompose(dir, want); err != nil {
		t.Fatalf("WriteCompose: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ComposeFilename)); err != nil {
		t.Fatalf("compose not written: %v", err)
	}
	got, err := ParseExistingCompose(dir)
	if err != nil {
		t.Fatalf("ParseExistingCompose: %v", err)
	}
	if got == nil {
		t.Fatalf("expected parsed config, got nil")
	}
	if *got != want {
		t.Fatalf("round-trip mismatch:\n  got:  %+v\n  want: %+v", *got, want)
	}
}

func TestParseExistingCompose_Missing(t *testing.T) {
	cfg, err := ParseExistingCompose(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil for missing compose, got %+v", cfg)
	}
}

func TestGenerateCompose_UsesBindMount(t *testing.T) {
	out := GenerateCompose(config.Config{Port: 3005, DataPath: "/srv/stonefruit", PostgresPassword: "pw"})
	if !contains(out, `"/srv/stonefruit:/data/blobs"`) {
		t.Fatalf("expected bind mount for data_path, got:\n%s", out)
	}
	if contains(out, "blob-data:/data/blobs") {
		t.Fatalf("should not use named volume for blobs")
	}
}

func TestWriteAndParseEnv_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	hash := "scrypt:N=16384,r=8,p=1:abcd:ef01"
	if err := WriteEnvFile(dir, hash); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}
	got, err := ParseExistingEnv(dir)
	if err != nil {
		t.Fatalf("ParseExistingEnv: %v", err)
	}
	if got != hash {
		t.Fatalf("round-trip mismatch:\n  got:  %q\n  want: %q", got, hash)
	}
}

func TestParseExistingEnv_Missing(t *testing.T) {
	got, err := ParseExistingEnv(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty for missing .env, got %q", got)
	}
}

func TestGenerateCompose_ReferencesPasswordHashEnv(t *testing.T) {
	out := GenerateCompose(config.Config{Port: 3005, DataPath: "/d", PostgresPassword: "pw"})
	if !contains(out, "STONEFRUIT_PASSWORD_HASH: ${STONEFRUIT_PASSWORD_HASH}") {
		t.Fatalf("expected compose to reference ${STONEFRUIT_PASSWORD_HASH}, got:\n%s", out)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (stringIndex(s, sub) >= 0))
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
