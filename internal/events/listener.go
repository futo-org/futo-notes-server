package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	initialReconnectDelay = time.Second
	maxReconnectDelay     = 30 * time.Second
	listenerAppName       = "futo-notes-events"
)

// Listen holds a dedicated native pgx connection and reconnects until ctx is
// canceled. Every connection loss closes current streams before retrying.
func Listen(ctx context.Context, databaseURL string, hub *Hub) {
	listen(ctx, databaseURL, hub, listenerAppName)
}

// listen is Listen with the connection's application_name injectable, so
// tests can find and terminate the listener's backend.
func listen(ctx context.Context, databaseURL string, hub *Hub, appName string) {
	delay := initialReconnectDelay
	for {
		connected, err := listenOnce(ctx, databaseURL, hub, appName)
		if ctx.Err() != nil {
			hub.CloseAll()
			return
		}
		hub.CloseAll()
		if connected {
			delay = initialReconnectDelay
		}
		slog.Warn("sync events listener lost", "err", err, "reconnect_in", delay)

		select {
		case <-ctx.Done():
			hub.CloseAll()
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, maxReconnectDelay)
	}
}

// listenOnce runs one LISTEN connection until it fails or ctx is canceled.
// connected reports whether LISTEN was established, which resets the
// reconnect backoff.
func listenOnce(ctx context.Context, databaseURL string, hub *Hub, appName string) (connected bool, err error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return false, err
	}
	config.RuntimeParams["application_name"] = appName
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return false, err
	}
	// Subscribers can be created while the listener is in its reconnect
	// backoff. They may have missed committed notifications, so close them as
	// soon as LISTEN is restored and force their clients to pull again.
	hub.CloseAll()
	slog.Info("sync events listener connected", "channel", Channel)

	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return true, ctx.Err()
			}
			return true, err
		}
		var payload Notification
		if err := json.Unmarshal([]byte(notification.Payload), &payload); err != nil {
			slog.Error("sync events listener: dropping invalid payload", "err", err)
			continue
		}
		hub.Publish(payload)
	}
}
