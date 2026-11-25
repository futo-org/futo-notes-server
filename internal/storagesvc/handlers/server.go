// Package handlers serves the storage service's HTTP API.
package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/hlog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/storagesvc/auth"
	"futo-notes-server/internal/storagesvc/metrics"
	"futo-notes-server/internal/storagesvc/storage"
)

// Server is one region's storage service. Its embedded Readiness answers the
// readiness probe and drains it.
type Server struct {
	httputil.Readiness

	Storage  storage.Storage
	Verifier *hostedauth.Verifier
	Signer   *hostedauth.ReceiptSigner
	// Metrics are built on a no-op meter provider when metrics are off.
	Metrics *metrics.Metrics
}

// Handler returns the service's HTTP handler: the readiness probe in front of
// the router.
func (s *Server) Handler() http.Handler {
	return s.Wrap(s.routes())
}

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	r.Use(hlog.NewHandler(log.Logger))
	r.Use(hlog.RequestIDHandler("request_id", "X-Request-Id"))
	r.Use(hlog.RemoteAddrHandler("remote_ip"))
	r.Use(hlog.UserAgentHandler("user_agent"))
	r.Use(hlog.AccessHandler(httputil.LogAccess))
	r.Use(chimw.Recoverer)
	r.Use(s.Metrics.Middleware)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, "Not Found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httputil.WriteError(w, r, http.StatusMethodNotAllowed, httputil.CodeMethodNotAllowed, "Method Not Allowed")
	})

	r.Group(func(r chi.Router) {
		r.Use(auth.Access(s.Verifier, s.Metrics.AuthFailure))
		r.Post("/blobs", s.uploadBlob)
		r.Post("/blobs/batch", s.batchBlobs)
		r.Get("/blobs/{userId}/{blobId}", s.getBlob)
	})
	r.Group(func(r chi.Router) {
		r.Use(auth.Admin(s.Verifier, s.Metrics.AuthFailure))
		r.Delete("/internal/blobs", s.deleteBlobs)
		r.Get("/internal/blobs", s.listBlobs)
		r.Delete("/internal/users/{userId}", s.deleteUser)
	})
	return r
}

// statusClientClosedRequest is logged when the client left before the
// answer; the client never sees it.
const statusClientClosedRequest = 499

// Error codes only the storage service uses, besides httputil's.
const (
	codeClientClosedRequest = "client_closed_request"
	codeLengthRequired      = "length_required"
	codePayloadTooLarge     = "payload_too_large"
	codeRangeNotSatisfiable = "range_not_satisfiable"
)

// net/http cancels the request context when the connection fails, while the
// storage call may surface the body's EOF first. A deadline is the server
// giving up, not the client leaving.
func clientGone(r *http.Request) bool { return errors.Is(r.Context().Err(), context.Canceled) }

// storageFailure answers a failed storage call.
func storageFailure(w http.ResponseWriter, r *http.Request, err error, msg string) {
	if clientGone(r) {
		hlog.FromRequest(r).Warn().Err(err).Msg(msg + ": client disconnected")
		httputil.WriteError(w, r, statusClientClosedRequest, codeClientClosedRequest, "Client closed request")
		return
	}
	hlog.FromRequest(r).Error().Err(err).Msg(msg)
	httputil.WriteError(w, r, http.StatusInternalServerError, httputil.CodeInternalError, "Storage error")
}
