// Package hosted is the end-to-end scenario test of the hosted service. It
// builds cmd/mock-oidc, cmd/storage and cmd/api, runs them as subprocesses
// (three storage regions against S3, the API against Postgres), and drives
// them over HTTP only, as the native app would.
//
// It runs when HOSTED_E2E_DATABASE_URL (a scratch Postgres database) and
// S3_TEST_ENDPOINT (with S3_TEST_ACCESS_KEY_ID, S3_TEST_SECRET_ACCESS_KEY,
// S3_TEST_REGION, and buckets notes-<region>) are set, and is skipped
// otherwise.
package hosted

import (
	"bufio"
	"context"
	"encoding/json"

	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/storagesvc/storage"
)

const (
	startupDeadline  = 60 * time.Second
	shutdownDeadline = 15 * time.Second
	logTailLines     = 60
	oidcClientID     = "notes-e2e"
	oidcClientSecret = "notes-e2e-secret"
)

var regionIDs = []string{"us-east", "eu-central", "ap-southeast"}

// stack is the running service: mock OIDC, one storage service per region,
// and the API, with direct access to the API's database and the regions'
// buckets for setup and checks.
type stack struct {
	apiURL   string
	issuer   string
	storage  map[string]string // region id -> storage base URL
	database *db.DB
	buckets  map[string]*storage.S3Storage
}

// startStack builds and starts every service, or skips the test when the
// environment does not name a database and an S3 endpoint.
func startStack(t *testing.T) *stack {
	t.Helper()
	databaseURL := os.Getenv("HOSTED_E2E_DATABASE_URL")
	s3Endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if databaseURL == "" || s3Endpoint == "" {
		t.Skip("HOSTED_E2E_DATABASE_URL and S3_TEST_ENDPOINT not set")
	}
	s3Region := os.Getenv("S3_TEST_REGION")
	if s3Region == "" {
		s3Region = "us-east-1"
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	bin := t.TempDir()
	var procs []*process
	// Registered first so it runs last, after every process has stopped and
	// its log is complete.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, p := range procs {
			p.logTail(t)
		}
	})

	build := exec.Command(goTool(t), "build", "-o", bin+string(filepath.Separator),
		"./cmd/mock-oidc", "./cmd/storage", "./cmd/api")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building services: %v\n%s", err, out)
	}

	s := &stack{storage: map[string]string{}, buckets: map[string]*storage.S3Storage{}}
	s.database, err = db.Open(databaseURL, db.Options{PoolMax: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.database.Close() })
	for _, region := range regionIDs {
		s.buckets[region] = storage.NewS3Storage(storage.S3Options{
			Endpoint:        s3Endpoint,
			Region:          s3Region,
			Bucket:          "notes-" + region,
			AccessKeyID:     os.Getenv("S3_TEST_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("S3_TEST_SECRET_ACCESS_KEY"),
			ForcePathStyle:  true,
		})
	}

	apiPort, oidcPort := freePort(t), freePort(t)
	s.apiURL = fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	s.issuer = fmt.Sprintf("http://127.0.0.1:%d", oidcPort)
	start := func(name string, args []string, env ...string) *process {
		p := startProcess(t, logDir, name, filepath.Join(bin, args[0]), args[1:], env)
		procs = append(procs, p)
		return p
	}

	oidc := start("mock-oidc", []string{"mock-oidc"},
		fmt.Sprintf("MOCK_OIDC_PORT=%d", oidcPort),
		"MOCK_OIDC_ISSUER="+s.issuer,
		"MOCK_OIDC_CLIENT_ID="+oidcClientID,
		"MOCK_OIDC_CLIENT_SECRET="+oidcClientSecret,
		"MOCK_OIDC_REDIRECT_URIS="+s.apiURL+"/api/auth/oidc/callback",
		"MOCK_OIDC_POST_LOGOUT_REDIRECT_URIS="+s.apiURL+"/",
		"LOG_LEVEL=debug")

	apiPublicKeys := readFile(t, root, "dev/hosted-keys/api.pub.pem")
	type regionEntry struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		Description       string `json:"description"`
		Endpoint          string `json:"endpoint"`
		ReceiptPublicKeys string `json:"receipt_public_keys"`
	}
	var regions []regionEntry
	var storageProcs []*process
	for _, region := range regionIDs {
		port := freePort(t)
		s.storage[region] = fmt.Sprintf("http://127.0.0.1:%d", port)
		regions = append(regions, regionEntry{
			ID:                region,
			Name:              "E2E " + region,
			Description:       "End-to-end test region " + region + ".",
			Endpoint:          s.storage[region],
			ReceiptPublicKeys: readFile(t, root, "dev/hosted-keys/"+region+".pub.pem"),
		})
		storageProcs = append(storageProcs, start("storage-"+region, []string{"storage"},
			"REGION_ID="+region,
			fmt.Sprintf("STORAGE_PORT=%d", port),
			"S3_ENDPOINT="+s3Endpoint,
			"S3_REGION="+s3Region,
			"S3_BUCKET=notes-"+region,
			"S3_ACCESS_KEY_ID="+os.Getenv("S3_TEST_ACCESS_KEY_ID"),
			"S3_SECRET_ACCESS_KEY="+os.Getenv("S3_TEST_SECRET_ACCESS_KEY"),
			"S3_FORCE_PATH_STYLE=true",
			"API_PUBLIC_KEYS="+apiPublicKeys,
			"RECEIPT_SIGNING_KEY="+readFile(t, root, "dev/hosted-keys/"+region+".key.pem"),
			"DRAIN_DELAY_MS=0",
			"LOG_LEVEL=debug"))
	}
	regionsJSON, err := json.Marshal(regions)
	if err != nil {
		t.Fatal(err)
	}
	regionsFile := filepath.Join(t.TempDir(), "regions.json")
	if err := os.WriteFile(regionsFile, regionsJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	pg, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("HOSTED_E2E_DATABASE_URL: %v", err)
	}
	password, _ := pg.User.Password()
	pgPort := pg.Port()
	if pgPort == "" {
		pgPort = "5432"
	}
	apiEnv := []string{
		fmt.Sprintf("API_PORT=%d", apiPort),
		"PUBLIC_URL=" + s.apiURL,
		"POSTGRES_HOST=" + pg.Hostname(),
		"POSTGRES_PORT=" + pgPort,
		"POSTGRES_DATABASE=" + strings.TrimPrefix(pg.Path, "/"),
		"POSTGRES_USERNAME=" + pg.User.Username(),
		"POSTGRES_PASSWORD=" + password,
		"OIDC_ISSUER=" + s.issuer,
		"OIDC_CLIENT_ID=" + oidcClientID,
		"OIDC_CLIENT_SECRET=" + oidcClientSecret,
		"REGIONS_FILE=" + regionsFile,
		"API_SIGNING_KEY=" + readFile(t, root, "dev/hosted-keys/api.key.pem"),
		"COOKIE_SECURE=false",
		"DEV_UI=true",
		"DRAIN_DELAY_MS=0",
		"LOG_LEVEL=debug",
	}
	migrate := exec.Command(filepath.Join(bin, "api"), "migrate")
	migrate.Env = apiEnv
	if out, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("api migrate: %v\n%s", err, out)
	}
	api := start("api", []string{"api"}, apiEnv...)

	waitReady(t, oidc, s.issuer+"/.well-known/openid-configuration")
	for i, region := range regionIDs {
		waitReady(t, storageProcs[i], s.storage[region]+"/readyz")
	}
	waitReady(t, api, s.apiURL+"/readyz")
	return s
}

// goTool returns the go command that runs this test's toolchain.
func goTool(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("finding the go command: %v", err)
	}
	return path
}

func readFile(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// freePort returns a loopback port nothing listens on. The services take
// their ports from configuration, so the port is released before they bind
// it; another process could claim it in between, which the CI container
// does not have.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// process is a service running as a subprocess, its output in a log file.
type process struct {
	name    string
	logPath string
	cmd     *exec.Cmd
	done    chan struct{}
	err     error
}

// startProcess starts path with only env as its environment, so nothing the
// test runner inherited configures it, and stops it when the test ends.
func startProcess(t *testing.T, logDir, name, path string, args, env []string) *process {
	t.Helper()
	logPath := filepath.Join(logDir, name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("starting %s: %v", name, err)
	}
	p := &process{name: name, logPath: logPath, cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		logFile.Close()
		close(p.done)
	}()
	t.Cleanup(p.stop)
	return p
}

// stop asks the process to drain and exit, and kills it if it has not by
// shutdownDeadline.
func (p *process) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(shutdownDeadline):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// logTail logs the last logTailLines lines of the process's output.
func (p *process) logTail(t *testing.T) {
	t.Helper()
	f, err := os.Open(p.logPath)
	if err != nil {
		t.Logf("--- %s: %v", p.name, err)
		return
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > logTailLines {
			lines = lines[1:]
		}
	}
	t.Logf("--- last %d lines of %s (exit: %v)\n%s", len(lines), p.name, p.err, strings.Join(lines, "\n"))
}

// waitReady polls url until it answers 200, failing the test if p exits or
// startupDeadline passes first.
func waitReady(t *testing.T, p *process, url string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(startupDeadline)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		if p.exited() {
			t.Fatalf("%s exited before it was ready: %v", p.name, p.err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not ready at %s after %s: %v", p.name, url, startupDeadline, err)
		}
		<-tick.C
	}
}

// keys lists every key under prefix in region's bucket.
func (s *stack) keys(t *testing.T, region, prefix string) []string {
	t.Helper()
	var keys []string
	after := ""
	for {
		page, err := s.buckets[region].ListObjects(context.Background(), prefix, after, storage.MaxDeleteKeys)
		if err != nil {
			t.Fatalf("listing %s/%s: %v", region, prefix, err)
		}
		for _, o := range page.Objects {
			keys = append(keys, o.Key)
		}
		if !page.Truncated {
			return keys
		}
		after = page.LastKey
	}
}

// cleanupUser removes, when the test ends, whatever of userID's the scenario
// left behind: their prefix in every region's bucket and their rows.
func (s *stack) cleanupUser(t *testing.T, userID string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := s.database.ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, userID); err != nil {
			t.Errorf("cleaning up user %s: %v", userID, err)
		}
		for _, region := range regionIDs {
			keys := s.keys(t, region, userID+"/")
			if len(keys) == 0 {
				continue
			}
			if failed, err := s.buckets[region].DeleteObjects(context.Background(), keys); err != nil || len(failed) > 0 {
				t.Errorf("cleaning up %s in %s: %v %v", userID, region, failed, err)
			}
		}
	})
}

// exec runs a setup statement against the API's database.
func (s *stack) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := s.database.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// count runs a SELECT count(*) against the API's database.
func (s *stack) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.database.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
