// Package mockoidctest runs a mockoidc Provider for tests. It lives apart from
// mockoidc so the cmd/mock-oidc binary does not link testing and httptest.
package mockoidctest

import (
	"net/http/httptest"
	"testing"

	"futo-notes-server/internal/mockoidc"
)

// Start runs a Provider on an httptest server for the duration of t.
// cfg.Issuer is filled in with the server's URL. It returns the server; its
// URL is the issuer.
func Start(t testing.TB, cfg mockoidc.Config) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	cfg.Issuer = "http://" + srv.Listener.Addr().String()
	p, err := mockoidc.New(cfg)
	if err != nil {
		srv.Close()
		t.Fatalf("mockoidc: %v", err)
	}
	srv.Config.Handler = p.Handler()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}
