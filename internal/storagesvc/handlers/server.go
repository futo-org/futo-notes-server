// Package handlers serves the storage service's HTTP API.
package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/hlog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/storagesvc/auth"
	"futo-notes-server/internal/storagesvc/httputil"
	"futo-notes-server/internal/storagesvc/metrics"
	"futo-notes-server/internal/storagesvc/storage"
)

// Server is one region's storage service.
type Server struct {
	Storage  storage.Storage
	Verifier *hostedauth.Verifier
	Signer   *hostedauth.ReceiptSigner
	// Metrics may be nil.
	Metrics *metrics.Metrics

	draining atomic.Bool
}

// readyPath is the kubelet's readiness probe. Every other route is under
// /blobs or /internal, so nothing can collide with it.
const readyPath = "/readyz"

// BeginDrain fails readiness from here on. Requests still arriving are served
// in full; the point is to leave the Service's endpoints before the listener
// closes, so a rollout moves clients to another replica instead of resetting
// their transfers.
func (s *Server) BeginDrain() { s.draining.Store(true) }

// Handler returns the service's HTTP handler: the readiness probe in front of
// the router.
func (s *Server) Handler() http.Handler {
	return s.withReadiness(s.routes())
}

func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	r.Use(hlog.NewHandler(log.Logger))
	r.Use(hlog.RequestIDHandler("request_id", "X-Request-Id"))
	r.Use(hlog.RemoteAddrHandler("remote_ip"))
	r.Use(hlog.UserAgentHandler("user_agent"))
	r.Use(hlog.AccessHandler(func(r *http.Request, status, size int, duration time.Duration) {
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = r.URL.Path
		}
		hlog.FromRequest(r).Info().
			Int("status", status).
			Int("size", size).
			Dur("duration", duration).
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Msgf("%s %s (%d)", r.Method, route, status)
	}))
	r.Use(chimw.Recoverer)
	if s.Metrics != nil {
		r.Use(metrics.Middleware(s.Metrics))
	}
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httputil.WriteError(w, r, http.StatusNotFound, "Not Found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httputil.WriteError(w, r, http.StatusMethodNotAllowed, "Method Not Allowed")
	})

	r.Group(func(r chi.Router) {
		r.Use(auth.Access(s.Verifier, s.Metrics.AuthFailure))
		if s.Metrics != nil {
			r.Use(metrics.CaptureUser)
		}
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

// withReadiness answers the readiness probe ahead of the router, keeping a
// probe every couple of seconds out of the access log and the metrics.
//
// S3 health is deliberately not part of the answer: an S3 outage is shared
// fate across every replica, so failing readiness on it would empty the
// Service and turn a degraded region into an unreachable one.
func (s *Server) withReadiness(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != readyPath {
			next.ServeHTTP(w, r)
			return
		}
		if s.draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// statusClientClosedRequest is logged when the client left before the
// answer; the client never sees it.
const statusClientClosedRequest = 499

// net/http cancels the request context when the connection fails, while the
// storage call may surface the body's EOF first. A deadline is the server
// giving up, not the client leaving.
func clientGone(r *http.Request) bool { return errors.Is(r.Context().Err(), context.Canceled) }

// storageFailure answers a failed storage call.
func storageFailure(w http.ResponseWriter, r *http.Request, err error, msg string) {
	if clientGone(r) {
		hlog.FromRequest(r).Warn().Err(err).Msg(msg + ": client disconnected")
		httputil.WriteError(w, r, statusClientClosedRequest, "Client closed request")
		return
	}
	hlog.FromRequest(r).Error().Err(err).Msg(msg)
	httputil.WriteError(w, r, http.StatusInternalServerError, "Storage error")
}
