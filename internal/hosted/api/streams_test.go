package api_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/events"
	"futo-notes-server/internal/hosted/hostedtest"
)

// streamEndBound is how soon a stream must end once its session does. It is
// far below the 25s ping, so only the events listener can meet it.
const streamEndBound = 5 * time.Second

// listen runs the events listener on h's hub until the test ends, and
// returns once it delivers.
func (h *harness) listen() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		events.Listen(ctx, hostedtest.URL(h.t), h.hub)
	}()
	h.t.Cleanup(func() {
		cancel()
		<-done
	})
	probe := "listener-probe-" + uuid.NewV7().String()
	deadline := time.Now().Add(10 * time.Second)
	for {
		sub := h.hub.Subscribe(probe, probe)
		h.exec(`SELECT pg_notify($1, $2)`, events.SessionsChannel, `{"userId":"`+probe+`"}`)
		select {
		case <-sub.Wake():
			// Closed, by the probe or by the listener connecting: either
			// way it is listening and has closed the streams opened before.
			return
		case <-time.After(200 * time.Millisecond):
			h.hub.Unsubscribe(sub)
		}
		if time.Now().After(deadline) {
			h.t.Fatal("events listener did not connect")
		}
	}
}

// openStream is an open sync event stream; ended closes when it ends.
type openStream struct {
	ended chan struct{}
}

// openStream opens a sync event stream on baseURL with token and waits for
// its ready event.
func (h *harness) openStream(baseURL, token string) openStream {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/sync/events", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// No client timeout: it would cut the stream.
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	reader := bufio.NewReader(res.Body)
	if line, err := reader.ReadString('\n'); res.StatusCode != http.StatusOK || err != nil || line != "event: ready\n" {
		cancel()
		res.Body.Close()
		h.t.Fatalf("sync events = %d, first line %q %v", res.StatusCode, line, err)
	}
	s := openStream{ended: make(chan struct{})}
	go func() {
		defer close(s.ended)
		for {
			if _, err := reader.ReadString('\n'); err != nil {
				return
			}
		}
	}()
	h.t.Cleanup(func() {
		cancel()
		res.Body.Close()
		<-s.ended
	})
	return s
}

func (s openStream) expectEnded(t *testing.T, within time.Duration, why string) {
	t.Helper()
	select {
	case <-s.ended:
	case <-time.After(within):
		t.Fatalf("stream still open %s after %s", within, why)
	}
}

func (s openStream) expectOpen(t *testing.T, why string) {
	t.Helper()
	select {
	case <-s.ended:
		t.Fatalf("stream ended after %s", why)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestEndingASessionEndsItsSyncStreams(t *testing.T) {
	h := newHarness(t)
	h.listen()
	sub := h.newSub()
	phone, phoneTokens := h.appLogin(sub)
	phone.expect(http.MethodPut, "/api/account/region", map[string]string{"region_id": "us-east"}, http.StatusOK)
	laptop, laptopTokens := h.appLogin(sub)
	tablet, tabletTokens := h.appLogin(sub)
	_, watchTokens := h.appLogin(sub)
	laptopSession, _ := laptop.sessionIDs()
	watchSession, _ := h.clientWith(watchTokens.Token).sessionIDs()

	phoneStream := h.openStream(h.api.URL, phoneTokens.Token)
	laptopStream := h.openStream(h.api.URL, laptopTokens.Token)
	laptopAgain := h.openStream(h.api.URL, laptopTokens.Token)
	tabletStream := h.openStream(h.api.URL, tabletTokens.Token)
	watchStream := h.openStream(h.api.URL, watchTokens.Token)

	// Revoking a device ends every stream it holds, and only those.
	phone.expect(http.MethodDelete, "/api/sessions/"+laptopSession, nil, http.StatusNoContent)
	laptopStream.expectEnded(t, streamEndBound, "revoking its session")
	laptopAgain.expectEnded(t, streamEndBound, "revoking its session")
	phoneStream.expectOpen(t, "revoking another device")
	tabletStream.expectOpen(t, "revoking another device")

	// So does signing out.
	tablet.expect(http.MethodPost, "/api/auth/logout", nil, http.StatusNoContent)
	tabletStream.expectEnded(t, streamEndBound, "signing out")

	// The session row going by any other path, such as another replica or a
	// job, ends it too: nothing here passes through this server.
	h.exec(`DELETE FROM sessions WHERE id = $1`, watchSession)
	watchStream.expectEnded(t, streamEndBound, "deleting its session row")
	phoneStream.expectOpen(t, "ending other sessions")

	// Deleting the account signs the other devices out and makes the
	// account pending deletion, which ends the deleting device's stream too.
	_, otherTokens := h.appLogin(sub)
	otherStream := h.openStream(h.api.URL, otherTokens.Token)
	phone.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
	otherStream.expectEnded(t, streamEndBound, "deleting the account from another device")
	phoneStream.expectEnded(t, streamEndBound, "deleting its account")

	// A stream cannot reopen while the deletion is pending.
	res := phone.do(http.MethodGet, "/api/sync/events", nil)
	if res.status != http.StatusForbidden {
		t.Fatalf("sync events while deletion is pending = %d %s", res.status, res.body)
	}
}

func TestSyncStreamsRecheckTheirSessionOnEveryPing(t *testing.T) {
	h := newHarness(t)
	// No listener: only the recheck can end these streams.
	pinging := httptest.NewServer(h.server.HandlerWithSyncPing(50 * time.Millisecond))
	t.Cleanup(pinging.Close)
	sub := h.newSub()
	app, tokens := h.appLogin(sub)
	_, otherTokens := h.appLogin(sub)
	revoked, revokedTokens := h.appLogin(sub)
	current, _ := app.sessionIDs()
	revokedSession, _ := revoked.sessionIDs()

	stream := h.openStream(pinging.URL, tokens.Token)
	other := h.openStream(pinging.URL, otherTokens.Token)
	missed := h.openStream(pinging.URL, revokedTokens.Token)
	stream.expectOpen(t, "a few pings of a live session")
	h.exec(`UPDATE sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, current)
	stream.expectEnded(t, time.Second, "its session expired")
	other.expectOpen(t, "another session expired")

	// A revocation whose notification this replica missed.
	h.exec(`DELETE FROM sessions WHERE id = $1`, revokedSession)
	missed.expectEnded(t, time.Second, "its session was deleted")

	// So does an account going pending deletion.
	h.exec(`UPDATE users SET deletion_requested_at = now(), purge_at = now() + interval '30 days' WHERE sub = $1`, sub)
	other.expectEnded(t, time.Second, "its account went pending deletion")
}

// clientWith is a client holding bearer token.
func (h *harness) clientWith(token string) *client {
	c := h.client()
	c.bearer = token
	return c
}
