package syncapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/hlog"

	"futo-notes-server/internal/events"
)

// SyncPingInterval is how often a sync event stream pings.
const SyncPingInterval = 25 * time.Second

// HandleSyncEvents serves GET /api/sync/events, the server-sent event stream
// of the caller's collection changes.
func HandleSyncEvents(hub *events.Hub) http.HandlerFunc {
	return handleSyncEvents(hub, SyncPingInterval, nil)
}

// HandleSyncEventsWhile is HandleSyncEvents pinging every pingInterval, for
// streams that must end with their session: alive is asked before every
// ping, and false ends the stream instead.
func HandleSyncEventsWhile(hub *events.Hub, pingInterval time.Duration, alive func(*http.Request) bool) http.HandlerFunc {
	return handleSyncEvents(hub, pingInterval, alive)
}

func handleSyncEvents(hub *events.Hub, pingInterval time.Duration, alive func(*http.Request) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")

		// Subscribe before sending ready so a mutation cannot land between the
		// client's initial pull and registration with the hub.
		session := SessionFrom(r)
		sub := hub.Subscribe(session.User.ID, session.ID)
		defer hub.Unsubscribe(sub)

		controller := http.NewResponseController(w)
		if err := writeEmptyEvent(w, "ready"); err != nil {
			return
		}
		if err := controller.Flush(); err != nil {
			hlog.FromRequest(r).Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("flushing sync events response")
			return
		}

		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case _, open := <-sub.Wake():
				if !open {
					return
				}
				changes, open := sub.Drain()
				if !open {
					return
				}
				for _, change := range changes {
					data, err := json.Marshal(change)
					if err != nil {
						hlog.FromRequest(r).Error().Err(err).Str("method", r.Method).Str("path", r.URL.Path).Msg("encoding sync event change")
						return
					}
					if _, err := fmt.Fprintf(w, "event: change\ndata: %s\n\n", data); err != nil {
						return
					}
				}
				if len(changes) > 0 {
					if err := controller.Flush(); err != nil {
						return
					}
				}
			case <-ticker.C:
				if alive != nil && !alive(r) {
					return
				}
				if err := writeEmptyEvent(w, "ping"); err != nil {
					return
				}
				if err := controller.Flush(); err != nil {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	}
}

func writeEmptyEvent(w http.ResponseWriter, event string) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata:\n\n", event)
	return err
}
