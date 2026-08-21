package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type runConfig struct {
	name         string
	authMode     string
	password     string
	passwordHash string
}

type environment struct {
	tmpDir   string
	db       *sql.DB
	dbTS     string
	dbGo     string
	tsBlob   string
	goBlob   string
	goBinary string
	ts       *managedProcess
	goSrv    *managedProcess
}

type managedProcess struct {
	name    string
	cmd     *exec.Cmd
	logPath string
	logFile *os.File
	wait    chan error
	once    sync.Once
}

var safeDatabaseName = regexp.MustCompile(`^[a-z0-9_]+$`)

func runComparison(ctx context.Context, opts options, cfg runConfig) (result runResult, returnedErr error) {
	env, err := startEnvironment(ctx, opts, cfg)
	if err != nil {
		return result, err
	}
	keep := opts.keep
	defer func() {
		if err := env.close(context.Background(), opts, keep || len(result.Divergences) != 0 || returnedErr != nil); err != nil {
			result.Infrastructure = append(result.Infrastructure, cfg.name+" teardown: "+err.Error())
		}
	}()

	runner := newRunner(cfg.name, opts, env)
	if err := runner.run(ctx, cfg); err != nil {
		returnedErr = err
	}
	return runner.result, returnedErr
}

func startEnvironment(ctx context.Context, opts options, cfg runConfig) (*environment, error) {
	if err := requireFreePort(opts.goPort); err != nil {
		return nil, fmt.Errorf("Go port: %w", err)
	}
	if err := requireFreePort(opts.tsPort); err != nil {
		return nil, fmt.Errorf("TypeScript port: %w", err)
	}
	if _, err := exec.LookPath("bun"); err != nil {
		return nil, fmt.Errorf("bun is required: %w", err)
	}
	if info, err := os.Stat(opts.tsRepo); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("TypeScript repo %q is unavailable", opts.tsRepo)
	}
	if err := warnTSDrift(ctx, opts.tsRepo); err != nil {
		fmt.Printf("warning: unable to check TypeScript source drift: %v\n", err)
	}

	tmpDir, err := os.MkdirTemp("", "futo-notes-compare-")
	if err != nil {
		return nil, err
	}
	env := &environment{tmpDir: tmpDir}
	fail := func(cause error) (*environment, error) {
		env.close(context.Background(), opts, true)
		return nil, cause
	}

	env.db, err = sql.Open("pgx", opts.adminURL)
	if err != nil {
		return fail(err)
	}
	if err := env.db.PingContext(ctx); err != nil {
		return fail(fmt.Errorf("connecting to Postgres: %w", err))
	}
	runID := strconv.FormatInt(time.Now().UnixNano(), 36)
	modeName := strings.ReplaceAll(cfg.name, "/", "_")
	env.dbTS = "futo_notes_cmp_ts_" + modeName + "_" + runID
	env.dbGo = "futo_notes_cmp_go_" + modeName + "_" + runID
	for _, name := range []string{env.dbTS, env.dbGo} {
		if err := createDatabase(ctx, env.db, name); err != nil {
			return fail(err)
		}
	}

	env.goBinary = filepath.Join(tmpDir, "server")
	build := exec.CommandContext(ctx, "go", "build", "-o", env.goBinary, "./cmd/server")
	build.Env = withEnv(os.Environ(), map[string]string{"GOTOOLCHAIN": "auto"})
	if output, err := build.CombinedOutput(); err != nil {
		return fail(fmt.Errorf("building Go server: %w\n%s", err, output))
	}
	env.tsBlob = filepath.Join(tmpDir, "blobs-ts")
	env.goBlob = filepath.Join(tmpDir, "blobs-go")
	if err := os.MkdirAll(env.tsBlob, 0o700); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(env.goBlob, 0o700); err != nil {
		return fail(err)
	}

	common := map[string]string{
		"AUTH_MODE": cfg.authMode, "COOKIE_SECURE": "false", "BLOB_GC_ENABLED": "false",
		"LOG_LEVEL": "warn", "DEV_UI": "false", "FUTO_NOTES_PASSWORD": cfg.password,
		"FUTO_NOTES_PASSWORD_HASH": cfg.passwordHash,
	}
	tsEnv := cloneMap(common)
	tsEnv["DATABASE_URL"] = databaseURL(opts.adminURL, env.dbTS)
	tsEnv["PORT"] = strconv.Itoa(opts.tsPort)
	tsEnv["BLOB_DIR"] = env.tsBlob
	goEnv := cloneMap(common)
	goEnv["DATABASE_URL"] = databaseURL(opts.adminURL, env.dbGo)
	goEnv["PORT"] = strconv.Itoa(opts.goPort)
	goEnv["BLOB_DIR"] = env.goBlob

	env.ts, err = startProcess("ts", opts.tsRepo, filepath.Join(tmpDir, "ts.log"), tsEnv, "bun", "src/index.ts")
	if err != nil {
		return fail(err)
	}
	env.goSrv, err = startProcess("go", ".", filepath.Join(tmpDir, "go.log"), goEnv, env.goBinary)
	if err != nil {
		return fail(err)
	}
	healthResults := make(chan error, 2)
	go func() {
		healthResults <- waitHealthy(ctx, env.ts, fmt.Sprintf("http://127.0.0.1:%d", opts.tsPort), opts.healthWait)
	}()
	go func() {
		healthResults <- waitHealthy(ctx, env.goSrv, fmt.Sprintf("http://127.0.0.1:%d", opts.goPort), opts.healthWait)
	}()
	var healthErr error
	for range 2 {
		if err := <-healthResults; err != nil && healthErr == nil {
			healthErr = err
		}
	}
	if healthErr != nil {
		return fail(healthErr)
	}
	return env, nil
}

func startProcess(name, dir, logPath string, env map[string]string, program string, args ...string) (*managedProcess, error) {
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(program, args...)
	cmd.Dir = dir
	cmd.Env = withEnv(os.Environ(), env)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("starting %s server: %w", name, err)
	}
	p := &managedProcess{name: name, cmd: cmd, logPath: logPath, logFile: logFile, wait: make(chan error, 1)}
	go func() { p.wait <- cmd.Wait() }()
	return p, nil
}

func waitHealthy(ctx context.Context, process *managedProcess, baseURL string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		select {
		case err := <-process.wait:
			return fmt.Errorf("%s server exited during startup (%v); log: %s\n%s", process.name, err, process.logPath, tail(process.logPath))
		case <-deadline.C:
			return fmt.Errorf("%s server was not healthy after %s; log: %s\n%s", process.name, timeout, process.logPath, tail(process.logPath))
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			response, err := client.Get(baseURL + "/health")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

func (e *environment) close(ctx context.Context, opts options, keep bool) error {
	var errs []error
	for _, process := range []*managedProcess{e.ts, e.goSrv} {
		if process != nil {
			if err := process.stop(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if keep {
		if e.tmpDir != "" {
			fmt.Printf("kept artifacts: %s\n", e.tmpDir)
		}
		if e.dbTS != "" || e.dbGo != "" {
			fmt.Printf("kept databases: %s %s\n", e.dbTS, e.dbGo)
		}
	} else {
		for _, name := range []string{e.dbTS, e.dbGo} {
			if name != "" && e.db != nil {
				if err := dropDatabase(ctx, e.db, name); err != nil {
					errs = append(errs, err)
				}
			}
		}
		if e.tmpDir != "" {
			if err := os.RemoveAll(e.tmpDir); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if e.db != nil {
		errs = append(errs, e.db.Close())
	}
	return errors.Join(errs...)
}

func (p *managedProcess) stop() error {
	var stopErr error
	p.once.Do(func() {
		_ = p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.wait:
		case <-time.After(3 * time.Second):
			if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				stopErr = err
			}
			<-p.wait
		}
		stopErr = errors.Join(stopErr, p.logFile.Close())
	})
	return stopErr
}

func createDatabase(ctx context.Context, db *sql.DB, name string) error {
	if !safeDatabaseName.MatchString(name) {
		return fmt.Errorf("unsafe scratch database name %q", name)
	}
	_, err := db.ExecContext(ctx, `CREATE DATABASE "`+name+`"`)
	if err != nil {
		return fmt.Errorf("creating database %s: %w", name, err)
	}
	return nil
}

func dropDatabase(ctx context.Context, db *sql.DB, name string) error {
	if !safeDatabaseName.MatchString(name) {
		return fmt.Errorf("unsafe scratch database name %q", name)
	}
	_, _ = db.ExecContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
	_, err := db.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+name+`"`)
	if err != nil {
		return fmt.Errorf("dropping database %s: %w", name, err)
	}
	return nil
}

func databaseURL(adminURL, database string) string {
	u, err := url.Parse(adminURL)
	if err != nil {
		return adminURL
	}
	u.Path = "/" + database
	return u.String()
}

func withEnv(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, item := range base {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[key]; !replaced {
			result = append(result, item)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func requireFreePort(port int) error {
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("port %d is occupied (stop that process or choose another port): %w", port, err)
	}
	return listener.Close()
}

func warnTSDrift(ctx context.Context, repo string) error {
	cmd := exec.CommandContext(ctx, "git", "diff", "--quiet", "main", "--", "src")
	cmd.Dir = repo
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			fmt.Printf("warning: TypeScript src differs from main in %s\n", repo)
			return nil
		}
		return err
	}
	return nil
}

func tail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	if len(data) > 8000 {
		data = data[len(data)-8000:]
	}
	return string(bytes.TrimSpace(data))
}
