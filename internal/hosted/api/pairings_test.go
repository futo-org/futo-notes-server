package api_test

import (
	"bytes"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/hosted/config"
)

type openedPairing struct {
	ID        string `json:"id"`
	ExpiresAt string `json:"expires_at"`
}

func pairingOffer() map[string]string {
	return map[string]string{"public_key": "pk-" + strings.Repeat("a", 43), "device_name": "Test phone", "platform": "android"}
}

func (c *client) openPairing() openedPairing {
	c.h.t.Helper()
	var opened openedPairing
	c.expect(http.MethodPost, "/api/pairings", pairingOffer(), http.StatusCreated).json(c.h.t, &opened)
	return opened
}

func TestPairingRelay(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	newDevice, _ := h.appLogin(sub)
	unlocked, _ := h.appLogin(sub)

	opened := newDevice.openPairing()
	expiresAt, err := time.Parse(time.RFC3339Nano, opened.ExpiresAt)
	if err != nil || opened.ID == "" {
		t.Fatalf("opened pairing = %+v (%v)", opened, err)
	}
	if until := time.Until(expiresAt); until < 4*time.Minute+50*time.Second || until > 5*time.Minute+10*time.Second {
		t.Fatalf("pairing expires in %s, want 5 minutes", until)
	}
	path := "/api/pairings/" + opened.ID

	if res := newDevice.expect(http.MethodGet, path, nil, http.StatusAccepted); res.body != "" {
		t.Fatalf("poll before the key = %q, want no body", res.body)
	}
	const sealed = "c2VhbGVkLXZhdWx0LWtleQ"
	unlocked.expect(http.MethodPost, path+"/key", map[string]string{"ciphertext": sealed}, http.StatusNoContent)
	expectHostedError(t, unlocked.do(http.MethodPost, path+"/key", map[string]string{"ciphertext": "another"}),
		http.StatusConflict, "pairing_already_keyed")

	res := newDevice.expect(http.MethodGet, path, nil, http.StatusOK)
	if res.body != `{"ciphertext":"`+sealed+`"}`+"\n" || res.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("collect = %v %s, want the first ciphertext, not stored", res.header, res.body)
	}
	expectHostedError(t, newDevice.do(http.MethodGet, path, nil), http.StatusNotFound, "not_found")
	var rows int
	if err := h.database.QueryRowContext(t.Context(), `SELECT count(*) FROM pairings WHERE id = $1`, opened.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("pairing rows after collection = %d, %v, want 0", rows, err)
	}
}

var requestID = regexp.MustCompile(`"requestId":"[^"]*"`)

// Unknown, malformed, expired, collected and another account's pairings get
// one answer, on the poll and the post alike, and it is the answer of an
// unknown route.
func TestPairingNotFoundIsOneAnswer(t *testing.T) {
	h := newHarness(t)
	sub := h.newSub()
	owner, _ := h.appLogin(sub)
	stranger, _ := h.appLogin(h.newSub())

	expired := owner.openPairing()
	if _, err := h.database.ExecContext(t.Context(),
		`UPDATE pairings SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	collected := owner.openPairing()
	owner.expect(http.MethodPost, "/api/pairings/"+collected.ID+"/key", map[string]string{"ciphertext": "x"}, http.StatusNoContent)
	owner.expect(http.MethodGet, "/api/pairings/"+collected.ID, nil, http.StatusOK)
	foreign := owner.openPairing()

	want := owner.do(http.MethodGet, "/api/no-such-route", nil)
	normalize := func(res response) string {
		return res.header.Get("Content-Type") + " " + requestID.ReplaceAllString(res.body, `"requestId":""`)
	}
	cases := map[string]struct {
		c  *client
		id string
	}{
		"unknown":           {owner, absentID},
		"malformed":         {owner, "not-a-pairing"},
		"expired":           {owner, expired.ID},
		"collected":         {owner, collected.ID},
		"another account's": {stranger, foreign.ID},
	}
	for name, tc := range cases {
		for _, res := range []response{
			tc.c.do(http.MethodGet, "/api/pairings/"+tc.id, nil),
			tc.c.do(http.MethodPost, "/api/pairings/"+tc.id+"/key", map[string]string{"ciphertext": "probe"}),
		} {
			if res.status != http.StatusNotFound || normalize(res) != normalize(want) {
				t.Errorf("%s pairing = %d %s, want %d %s", name, res.status, normalize(res), want.status, normalize(want))
			}
		}
	}
	// The stranger's post did not key the owner's pairing.
	owner.expect(http.MethodGet, "/api/pairings/"+foreign.ID, nil, http.StatusAccepted)
	owner.expect(http.MethodPost, "/api/pairings/"+foreign.ID+"/key", map[string]string{"ciphertext": "x"}, http.StatusNoContent)
}

func TestPairingBounds(t *testing.T) {
	h := newHarness(t)
	app, _ := h.appLogin(h.newSub())
	opened := app.openPairing()
	keyPath := "/api/pairings/" + opened.ID + "/key"

	huge := pairingOffer()
	huge["padding"] = strings.Repeat("p", 8<<10)
	expectHostedError(t, app.do(http.MethodPost, "/api/pairings", huge), http.StatusRequestEntityTooLarge, "payload_too_large")
	expectHostedError(t, app.do(http.MethodPost, keyPath, map[string]string{"ciphertext": strings.Repeat("c", 8<<10)}),
		http.StatusRequestEntityTooLarge, "payload_too_large")

	for _, field := range []string{"public_key", "device_name", "platform"} {
		long := pairingOffer()
		long[field] = strings.Repeat("é", 1025)
		expectHostedError(t, app.do(http.MethodPost, "/api/pairings", long), http.StatusBadRequest, "invalid_request")
		missing := pairingOffer()
		delete(missing, field)
		expectHostedError(t, app.do(http.MethodPost, "/api/pairings", missing), http.StatusBadRequest, "invalid_request")
		empty := pairingOffer()
		empty[field] = ""
		expectHostedError(t, app.do(http.MethodPost, "/api/pairings", empty), http.StatusBadRequest, "invalid_request")
	}
	expectHostedError(t, app.do(http.MethodPost, "/api/pairings", map[string]any{"public_key": 7, "device_name": "d", "platform": "p"}),
		http.StatusBadRequest, "invalid_request")
	expectHostedError(t, app.do(http.MethodPost, keyPath, []string{"ciphertext"}), http.StatusBadRequest, "invalid_request")
	expectHostedError(t, app.do(http.MethodPost, keyPath, map[string]string{"ciphertext": strings.Repeat("c", 1025)}),
		http.StatusBadRequest, "invalid_request")

	// 1024 characters is allowed, even at two bytes each.
	exact := pairingOffer()
	exact["device_name"] = strings.Repeat("é", 1024)
	app.expect(http.MethodPost, "/api/pairings", exact, http.StatusCreated)
	app.expect(http.MethodPost, keyPath, map[string]string{"ciphertext": strings.Repeat("c", 1024)}, http.StatusNoContent)
}

// Of two devices collecting at once, exactly one gets the key.
func TestPairingCollectedOnce(t *testing.T) {
	h := newHarness(t)
	app, _ := h.appLogin(h.newSub())
	for range 10 {
		opened := app.openPairing()
		path := "/api/pairings/" + opened.ID
		app.expect(http.MethodPost, path+"/key", map[string]string{"ciphertext": "sealed"}, http.StatusNoContent)
		statuses := make([]int, 2)
		var wg sync.WaitGroup
		for i := range statuses {
			wg.Go(func() { statuses[i] = app.do(http.MethodGet, path, nil).status })
		}
		wg.Wait()
		slices.Sort(statuses)
		if statuses[0] != http.StatusOK || statuses[1] != http.StatusNotFound {
			t.Fatalf("concurrent collections = %v, want one 200 and one 404", statuses)
		}
	}
}

func TestPairingSweep(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.DevUI = true })
	app, _ := h.appLogin(h.newSub())
	live := app.openPairing()
	expired := app.openPairing()
	if _, err := h.database.ExecContext(t.Context(),
		`UPDATE pairings SET ciphertext = 'sealed', expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	var swept struct{ Deleted int64 }
	app.expect(http.MethodPost, "/dev/jobs/pairing-sweep", nil, http.StatusOK).json(t, &swept)
	if swept.Deleted < 1 {
		t.Fatalf("sweep = %+v, want the expired pairing deleted", swept)
	}
	var ids []string
	rows, err := h.database.QueryContext(t.Context(), `SELECT id FROM pairings WHERE id IN ($1, $2)`, live.ID, expired.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 || ids[0] != live.ID {
		t.Fatalf("pairings after the sweep = %v, want only %s", ids, live.ID)
	}
}

// The sealed key appears in no log line, on success or failure.
func TestPairingCiphertextIsNotLogged(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&logs))
	t.Cleanup(func() { log.Logger = previous })

	h := newHarness(t)
	app, _ := h.appLogin(h.newSub())
	const sealed = "SEALED-VAULT-KEY-must-not-be-logged"
	opened := app.openPairing()
	path := "/api/pairings/" + opened.ID
	app.do(http.MethodPost, path+"/key", map[string]string{"ciphertext": sealed + strings.Repeat("x", 1025)})
	app.expect(http.MethodPost, path+"/key", map[string]string{"ciphertext": sealed}, http.StatusNoContent)
	app.expect(http.MethodPost, path+"/key", map[string]string{"ciphertext": sealed}, http.StatusConflict)
	app.expect(http.MethodGet, path, nil, http.StatusOK)

	if !strings.Contains(logs.String(), "/api/pairings/") {
		t.Fatalf("captured no access log lines: %s", logs.String())
	}
	if strings.Contains(logs.String(), "SEALED-VAULT-KEY") {
		t.Fatalf("the ciphertext was logged:\n%s", logs.String())
	}
}
