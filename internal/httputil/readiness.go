package httputil

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// ReadyPath is the kubelet's readiness probe.
const ReadyPath = "/readyz"

// Readiness answers the readiness probe and retires a server in two phases.
// The zero value is ready.
type Readiness struct {
	draining atomic.Bool
}

// BeginDrain fails readiness from here on, while requests are still served.
func (rd *Readiness) BeginDrain() { rd.draining.Store(true) }

// Wrap answers ReadyPath ahead of next, so probes stay out of the access log
// and metrics: 200 while serving, 503 once draining. It checks no
// dependency: an outage of the database or of S3 is shared by every replica,
// and failing them all would only empty the Service.
func (rd *Readiness) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ReadyPath {
			next.ServeHTTP(w, r)
			return
		}
		if rd.draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// Drain retires srv. Phase one fails readiness while serving as before for
// delay, giving the probe and the EndpointSlice time to move new requests to
// another replica. Only then does phase two close the listener and wait up
// to timeout for in-flight requests; closing it first would reset live
// transfers. Connections still open at the deadline are closed.
func (rd *Readiness) Drain(srv *http.Server, delay, timeout time.Duration) error {
	rd.BeginDrain()
	log.Info().Dur("drain_delay", delay).Msg("draining: readiness failing, still serving")
	time.Sleep(delay)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	log.Info().Dur("timeout", timeout).Msg("closing listener, waiting for in-flight requests")
	if err := srv.Shutdown(ctx); err != nil {
		return errors.Join(err, srv.Close())
	}
	return nil
}
