// Command hostedrehearsal walks the whole hosted sign-up path against a real
// server process on localhost, before that path is pointed at a real Zitadel
// application and a real Polar organization.
//
// What it proves that the unit tests do not: a server built from the working
// tree boots from the documented hosted environment contract, applies its
// migrations to a Postgres database, discovers its identity provider at
// startup, mounts the hosted routes, and then answers the exact sequence a
// person goes through —
// Login Hand-off, the email-domain allowlist, a refused write, a checkout, a
// signed Polar delivery, an accepted write, and billing status with usage.
//
// Zitadel and Polar are stood in for by the local stand-ins in
// internal/standins, so the rehearsal needs no credentials and no
// network. What it therefore does NOT
// prove is anything on the far side of those two: that a Zitadel application
// is registered for the right redirect URI, that a Polar webhook endpoint
// exists, or that the discount reads 100%-forever in Polar's dashboard.
// Those are dashboard checks, listed in docs/Hosted deployment.md.
//
// Usage:
//
//	HOSTED_REHEARSAL_DATABASE_URL=postgres://... go run ./cmd/hostedrehearsal
//
// The database is a scratch one: the rehearsal drops and recreates its public
// schema on every run, so never point it at anything you want to keep.
package main

import (
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"futo-notes-server/internal/config"
	"futo-notes-server/internal/standins"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	// The rehearsal's stand-in credentials. They are never sent anywhere but
	// the local fakes, which do check them.
	clientID     = "hosted-rehearsal-client"
	clientSecret = "hosted-rehearsal-secret"
	accessToken  = "polar_oat_hosted_rehearsal"
	productID    = "polar_prod_hosted_rehearsal"
	discountID   = "polar_disc_hosted_rehearsal"
	// A Standard Webhooks secret: the "whsec_" prefix followed by base64.
	webhookSecret = "whsec_aG9zdGVkLXJlaGVhcnNhbC1zaWduaW5nLWtleQ=="

	// healthTimeout is how long the server gets to answer /health. It
	// migrates a schema on the way up, so this is generous.
	healthTimeout = 30 * time.Second

	// allowedDomain is what HOSTED_ALLOWED_EMAIL_DOMAINS is set to, matching
	// the internal MVP's futo.org gate.
	allowedDomain = "futo.org"

	// port is this project's local development port. The rehearsal stops
	// rather than move to another one, the same way the rest of local
	// development does.
	port = 3005
)

func main() {
	databaseURL := os.Getenv("HOSTED_REHEARSAL_DATABASE_URL")
	if databaseURL == "" {
		fail("HOSTED_REHEARSAL_DATABASE_URL is required, and must name a scratch Postgres database this run may erase")
	}

	if err := run(databaseURL); err != nil {
		fail(err.Error())
	}
	fmt.Println("\nhosted rehearsal passed")
}

func fail(message string) {
	fmt.Fprintf(os.Stderr, "hosted rehearsal failed: %s\n", message)
	os.Exit(1)
}

func run(databaseURL string) error {
	if err := freePort(); err != nil {
		return err
	}
	if err := resetDatabase(databaseURL); err != nil {
		return fmt.Errorf("resetting the scratch database: %w", err)
	}
	step("scratch Postgres database reset to empty")

	// The server's origin is known before it is started, because the port is
	// this project's fixed development one. The stand-in Polar needs it in
	// order to deliver a webhook back.
	serverOrigin := fmt.Sprintf("http://127.0.0.1:%d", port)

	issuer, stopIssuer, err := standins.StartIssuer(clientID, clientSecret, personSub, personEmail, personName)
	if err != nil {
		return fmt.Errorf("starting the stand-in issuer: %w", err)
	}
	defer stopIssuer()

	polar, stopPolar, err := standins.StartPolar(accessToken, standins.Webhook{
		URL:    serverOrigin + "/api/billing/polar/webhook",
		Secret: webhookSecret,
	})
	if err != nil {
		return fmt.Errorf("starting the stand-in Polar: %w", err)
	}
	defer stopPolar()
	step("stand-in Zitadel at %s, stand-in Polar at %s", issuer.Origin, polar.Origin)

	server, err := startServer(databaseURL, serverOrigin, issuer.Origin, polar.Origin)
	if err != nil {
		return err
	}
	defer server.stop()
	step("server booted in hosted mode on %s", server.origin)

	client, err := newClient()
	if err != nil {
		return err
	}
	rehearsalErr := walk(&session{
		client:      client,
		server:      server,
		issuer:      issuer,
		polar:       polar,
		databaseURL: databaseURL,
	})

	if rehearsalErr != nil {
		server.printLog()
	}
	return rehearsalErr
}

// freePort refuses to start when something already holds the port, rather
// than talking to whatever that something is. A stale development server is
// the likely occupant.
func freePort() error {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("port %d is already in use; stop whatever holds it and run again", port)
	}
	return listener.Close()
}

// resetDatabase empties the scratch database so every run starts from no
// migrations, which is what makes the run a real test of applying them.
func resetDatabase(databaseURL string) error {
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	_, err = database.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public")
	return err
}

// serverProcess is the binary under test.
type serverProcess struct {
	origin  string
	command *exec.Cmd
	logFile string
	workDir string
	// exited is closed once the child has been reaped, with exitErr holding
	// what it exited with. cmd.Wait may only be called once, so one
	// goroutine owns it and everything else waits on the close — which,
	// unlike a value on a channel, every reader sees. It is what lets the
	// health wait notice a server that died on its configuration instead of
	// waiting out the full timeout, and it still lets stop reap a server
	// that is already gone.
	exited  chan struct{}
	exitErr error
	once    sync.Once
}

// startServer builds the server from source and runs it with the hosted
// environment contract: AUTH_MODE=oidc, the four OIDC settings, the four
// required Polar settings plus the discount, and the internal MVP's
// email-domain gate.
//
// It runs in an empty working directory on purpose. config.Load reads a .env
// file from the process's directory, and a rehearsal that silently inherited
// a developer's .env would not be testing the contract this documents.
func startServer(databaseURL, origin, issuerOrigin, polarOrigin string) (*serverProcess, error) {
	workDir, err := os.MkdirTemp("", "hosted-rehearsal-*")
	if err != nil {
		return nil, err
	}
	binary := filepath.Join(workDir, "futo-notes-server")
	build := exec.Command("go", "build", "-o", binary, "./cmd/server")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		return nil, fmt.Errorf("building the server: %w", err)
	}

	logPath := filepath.Join(workDir, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()

	command := exec.Command(binary)
	command.Dir = workDir
	command.Stdout, command.Stderr = logFile, logFile
	command.Env = append(os.Environ(),
		"DATABASE_URL="+databaseURL,
		"PORT="+strconv.Itoa(port),
		"BLOB_DIR="+filepath.Join(workDir, "blobs"),
		// Blobs stay in BLOB_DIR even when this shell has S3 configured.
		// Later entries win, so this overrides an inherited S3_ENDPOINT.
		"S3_ENDPOINT=",
		"AUTH_MODE=oidc",
		// Served over plain http on loopback, so Secure cookies would never
		// come back. The deployed instance is behind TLS and leaves this on.
		"COOKIE_SECURE=false",
		"OIDC_ISSUER="+issuerOrigin,
		"OIDC_CLIENT_ID="+clientID,
		"OIDC_CLIENT_SECRET="+clientSecret,
		"OIDC_REDIRECT_URI="+origin+config.OIDCCallbackPath,
		"POLAR_BASE_URL="+polarOrigin,
		"POLAR_ACCESS_TOKEN="+accessToken,
		"POLAR_PRODUCT_ID="+productID,
		"POLAR_WEBHOOK_SECRET="+webhookSecret,
		"POLAR_DISCOUNT_ID="+discountID,
		"HOSTED_ALLOWED_EMAIL_DOMAINS="+allowedDomain,
	)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("starting the server: %w", err)
	}

	server := &serverProcess{
		origin:  origin,
		command: command,
		logFile: logPath,
		workDir: workDir,
		exited:  make(chan struct{}),
	}
	go func() {
		server.exitErr = command.Wait()
		close(server.exited)
	}()

	if err := server.waitForHealth(); err != nil {
		server.printLog()
		server.stop()
		return nil, err
	}
	return server, nil
}

// waitForHealth blocks until the server answers /health, it dies, or the
// timeout runs out. A server given a setting it refuses exits in
// milliseconds, and saying so beats waiting thirty seconds to report a
// timeout that was never the real problem.
func (s *serverProcess) waitForHealth() error {
	deadline := time.NewTimer(healthTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.exited:
			return fmt.Errorf("the server exited before becoming healthy: %w", s.exitErr)
		case <-deadline.C:
			return fmt.Errorf("the server did not answer /health within %s", healthTimeout)
		case <-ticker.C:
			response, err := http.Get(s.origin + "/health")
			if err != nil {
				continue
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

// printLog puts the server's whole log on stderr. It is only ever reached on
// a failure, where the reason is usually the last line the server wrote.
func (s *serverProcess) printLog() {
	contents, err := os.ReadFile(s.logFile)
	if err != nil {
		contents = []byte("(log unavailable: " + err.Error() + ")\n")
	}
	fmt.Fprintf(os.Stderr, "\n--- server log ---\n%s------------------\n", contents)
}

func (s *serverProcess) stop() {
	s.once.Do(func() {
		if s.command.Process != nil {
			_ = s.command.Process.Kill()
			<-s.exited
		}
		_ = os.RemoveAll(s.workDir)
	})
}

// newClient is the app and the browser at once: it keeps cookies, so the
// OIDC flow cookie survives the redirect chain the way a browser's would.
func newClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{Jar: jar, Timeout: 20 * time.Second}, nil
}

// step reports one thing that held. Both the setup in this file and the walk
// in steps.go print through it, so the run reads as one list.
func step(format string, args ...any) {
	fmt.Printf("  ok  %s\n", fmt.Sprintf(format, args...))
}
