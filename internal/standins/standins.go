// Package standins holds local stand-ins for the hosted service's two
// upstreams, Zitadel and Polar. They answer exactly the calls the server
// makes, over loopback, with no credentials and no network.
//
// Two things use them. `cmd/hostedrehearsal` boots a real server process
// against them to walk the whole hosted sign-up path. And the server itself
// runs them in-process under STANDIN_MODE, so a client's automated suite can
// drive that same path on localhost — see `cmd/server/standin.go` and
// `docs/adr/0009-stand-in-test-mode.md`.
//
// They are not rubber stamps. The issuer checks client credentials and
// recomputes the PKCE challenge; Polar checks its access token and refuses a
// second customer for an external id it has seen. A run that passes here has
// been through the real exchange rather than only the right redirect shape.
package standins

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
)

// serve starts an HTTP server on a free loopback port and returns its origin
// plus a shutdown function. Loopback and nothing else: everything these two
// answer is a fake, so nothing off the machine may reach them.
func serve(handler http.Handler) (origin string, stop func(), err error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String(), func() { _ = server.Close() }, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func randomToken() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
