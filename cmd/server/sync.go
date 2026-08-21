package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"futo-notes-server/internal/events"
)

const syncPingInterval = 25 * time.Second

func handleSyncEvents(hub *events.Hub) http.HandlerFunc {
	return handleSyncEventsWithInterval(hub, syncPingInterval)
}

func handleSyncEventsWithInterval(hub *events.Hub, pingInterval time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")

		// Subscribe before sending ready so a mutation cannot land between the
		// client's initial pull and registration with the hub.
		sub := hub.Subscribe(sessionFrom(r).User.ID)
		defer hub.Unsubscribe(sub)

		controller := http.NewResponseController(w)
		if err := writeEmptyEvent(w, "ready"); err != nil {
			return
		}
		if err := controller.Flush(); err != nil {
			log.Printf("sync events: response does not support flushing: %v", err)
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
						log.Printf("sync events: encoding change: %v", err)
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
