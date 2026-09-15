package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"futo-notes-server/internal/blobs"
	"futo-notes-server/internal/config"
	databasepkg "futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/pairing"
	"uuid"
)

// The values a client would put in a QR code. None of them mean anything to
// the server: the public key and the ciphertext are opaque, and the name and
// platform are whatever the new device calls itself.
const (
	pairingPublicKey = "MCowBQYDK2VuAyEAnew1device0public0key0000000"
	pairingSealedKey = "c2VhbGVkIHZhdWx0IGtleSBjaXBoZXJ0ZXh0"
	pairingOffer     = `{"public_key":"` + pairingPublicKey + `","device_name":"Kitchen laptop","platform":"desktop"}`
)

// notFoundBody is the one answer an unknown, expired, spent, or someone
// else's pairing gets, byte for byte.
const notFoundBody = "{\"error\":\"not found\"}\n"

// pairingHarness is a dev-mode server with two accounts signed in: the one
// that owns the pairings, and a stranger used to prove nothing leaks across
// accounts.
type pairingHarness struct {
	url      string
	client   *http.Client
	database *databasepkg.DB
	token    string
	stranger string
}

func newPairingHarness(t *testing.T, database *databasepkg.DB, emailPrefix string) pairingHarness {
	t.Helper()
	cfg := config.Config{AuthMode: "dev", CookieSecure: false, BlobDir: t.TempDir()}
	hub := events.NewHub()
	server := httptest.NewServer(routes(cfg, database, &blobs.Store{Dir: cfg.BlobDir}, hub,
		events.NewPublisher(database.Dialect(), hub), nil))
	t.Cleanup(server.Close)
	client := serverClient(server)

	login := func(email string) string {
		response := doRequest(t, client, http.MethodPost, server.URL+"/api/auth/dev/login",
			`{"email":"`+email+`","name":"Pairing Routes"}`, "", nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("login %s = %d %q", email, response.StatusCode, response.Body)
		}
		var loggedIn struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(response.Body), &loggedIn); err != nil {
			t.Fatal(err)
		}
		return loggedIn.Token
	}

	return pairingHarness{
		url:      server.URL,
		client:   client,
		database: database,
		token:    login(emailPrefix + "-owner@example.invalid"),
		stranger: login(emailPrefix + "-stranger@example.invalid"),
	}
}

func (h pairingHarness) create(t *testing.T, token, body string) routeTestResponse {
	t.Helper()
	return doRequest(t, h.client, http.MethodPost, h.url+"/api/pairings", body, token, nil)
}

func (h pairingHarness) sendKey(t *testing.T, token, id, body string) routeTestResponse {
	t.Helper()
	return doRequest(t, h.client, http.MethodPost, h.url+"/api/pairings/"+id+"/key", body, token, nil)
}

func (h pairingHarness) poll(t *testing.T, token, id string) routeTestResponse {
	t.Helper()
	return doRequest(t, h.client, http.MethodGet, h.url+"/api/pairings/"+id, "", token, nil)
}

// newPairing creates one and returns its id, failing the test if the create
// did not answer 201.
func (h pairingHarness) newPairing(t *testing.T, token string) string {
	t.Helper()
	response := h.create(t, token, pairingOffer)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create pairing = %d %q, want 201", response.StatusCode, response.Body)
	}
	var created struct {
		ID        string `json:"id"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(response.Body), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatalf("create pairing returned no id: %q", response.Body)
	}
	return created.ID
}

// expire backdates a pairing the way the clock would, without waiting out
// the whole window.
func (h pairingHarness) expire(t *testing.T, id string) {
	t.Helper()
	if _, err := h.database.Exec(`UPDATE device_pairings SET expires_at = $1 WHERE id = $2`,
		databasepkg.Timestamp(time.Now().Add(-time.Second)), id); err != nil {
		t.Fatal(err)
	}
}

func runPairingRelayScenario(t *testing.T, h pairingHarness) {
	t.Helper()

	created := h.create(t, h.token, pairingOffer)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create pairing = %d %q, want 201", created.StatusCode, created.Body)
	}
	var offer struct {
		ID        string    `json:"id"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(created.Body), &offer); err != nil {
		t.Fatal(err)
	}
	if len(offer.ID) < 32 {
		t.Fatalf("pairing id = %q, want an unguessable value", offer.ID)
	}
	if got, want := offer.ExpiresAt, time.Now().Add(pairing.TTL); got.Sub(want).Abs() > time.Minute {
		t.Fatalf("expires_at = %v, want about %v (%v out)", got, want, pairing.TTL)
	}

	if pending := h.poll(t, h.token, offer.ID); pending.StatusCode != http.StatusAccepted || pending.Body != "" {
		t.Fatalf("poll before the key was posted = %d %q, want 202 with an empty body",
			pending.StatusCode, pending.Body)
	}

	posted := h.sendKey(t, h.token, offer.ID, `{"ciphertext":"`+pairingSealedKey+`"}`)
	if posted.StatusCode != http.StatusNoContent || posted.Body != "" {
		t.Fatalf("post ciphertext = %d %q, want 204 with an empty body", posted.StatusCode, posted.Body)
	}

	collected := h.poll(t, h.token, offer.ID)
	if collected.StatusCode != http.StatusOK {
		t.Fatalf("poll after the key was posted = %d %q, want 200", collected.StatusCode, collected.Body)
	}
	var relayed struct {
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal([]byte(collected.Body), &relayed); err != nil {
		t.Fatal(err)
	}
	if relayed.Ciphertext != pairingSealedKey {
		t.Fatalf("relayed ciphertext = %q, want the bytes the unlocked device posted", relayed.Ciphertext)
	}
	// The ciphertext and nothing else: the public key, device name, and
	// platform are stored and never handed back, because the device that
	// needs them read them out of the QR payload it scanned.
	if want := "{\"ciphertext\":\"" + pairingSealedKey + "\"}\n"; collected.Body != want {
		t.Fatalf("collected body = %q, want exactly %q", collected.Body, want)
	}

	// Collected once means gone: a second poll is a 404, not a replay.
	again := h.poll(t, h.token, offer.ID)
	if again.StatusCode != http.StatusNotFound || again.Body != notFoundBody {
		t.Fatalf("second poll = %d %q, want 404 %q", again.StatusCode, again.Body, notFoundBody)
	}
	// And the spent pairing cannot be refilled.
	refill := h.sendKey(t, h.token, offer.ID, `{"ciphertext":"`+pairingSealedKey+`"}`)
	if refill.StatusCode != http.StatusNotFound || refill.Body != notFoundBody {
		t.Fatalf("post to a spent pairing = %d %q, want 404 %q", refill.StatusCode, refill.Body, notFoundBody)
	}
}

func runPairingSecondPostScenario(t *testing.T, h pairingHarness) {
	t.Helper()
	id := h.newPairing(t, h.token)

	if first := h.sendKey(t, h.token, id, `{"ciphertext":"first"}`); first.StatusCode != http.StatusNoContent {
		t.Fatalf("first post = %d %q, want 204", first.StatusCode, first.Body)
	}
	second := h.sendKey(t, h.token, id, `{"ciphertext":"second"}`)
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second post = %d %q, want 409", second.StatusCode, second.Body)
	}
	if !strings.Contains(second.Body, "already") {
		t.Fatalf("second post body = %q, want it to say the pairing is already filled", second.Body)
	}

	// The refusal changed nothing: the first ciphertext is still what the new
	// device collects.
	collected := h.poll(t, h.token, id)
	if collected.StatusCode != http.StatusOK || !strings.Contains(collected.Body, `"first"`) {
		t.Fatalf("poll = %d %q, want the first ciphertext", collected.StatusCode, collected.Body)
	}
}

func runPairingCrossAccountScenario(t *testing.T, h pairingHarness) {
	t.Helper()
	id := h.newPairing(t, h.token)
	// A pairing id that was never minted, for comparison. Same shape, so the
	// only difference between the two is whether the row exists.
	unknown := strings.Repeat("A", len(id))

	for _, attempt := range []struct {
		what string
		got  routeTestResponse
	}{
		{"stranger posting to a real pairing", h.sendKey(t, h.stranger, id, `{"ciphertext":"stolen"}`)},
		{"stranger posting to a pairing that does not exist", h.sendKey(t, h.stranger, unknown, `{"ciphertext":"stolen"}`)},
		{"stranger polling a real pairing", h.poll(t, h.stranger, id)},
		{"stranger polling a pairing that does not exist", h.poll(t, h.stranger, unknown)},
		{"the owner polling a pairing that does not exist", h.poll(t, h.token, unknown)},
	} {
		if attempt.got.StatusCode != http.StatusNotFound || attempt.got.Body != notFoundBody {
			t.Errorf("%s = %d %q, want 404 %q — a pairing another account cannot see must not be "+
				"distinguishable from one that was never created",
				attempt.what, attempt.got.StatusCode, attempt.got.Body, notFoundBody)
		}
	}

	// None of that touched the pairing: it is still empty and still the
	// owner's to fill.
	if pending := h.poll(t, h.token, id); pending.StatusCode != http.StatusAccepted {
		t.Fatalf("owner's poll after the stranger's attempts = %d %q, want 202", pending.StatusCode, pending.Body)
	}
	if posted := h.sendKey(t, h.token, id, `{"ciphertext":"`+pairingSealedKey+`"}`); posted.StatusCode != http.StatusNoContent {
		t.Fatalf("owner's post after the stranger's attempts = %d %q, want 204", posted.StatusCode, posted.Body)
	}
}

func runPairingExpiryScenario(t *testing.T, h pairingHarness) {
	t.Helper()

	empty := h.newPairing(t, h.token)
	h.expire(t, empty)
	if response := h.poll(t, h.token, empty); response.StatusCode != http.StatusNotFound || response.Body != notFoundBody {
		t.Errorf("poll for an expired empty pairing = %d %q, want 404 %q",
			response.StatusCode, response.Body, notFoundBody)
	}
	if response := h.sendKey(t, h.token, empty, `{"ciphertext":"late"}`); response.StatusCode != http.StatusNotFound {
		t.Errorf("post to an expired pairing = %d %q, want 404", response.StatusCode, response.Body)
	}

	// A filled pairing expires too, so an uncollected vault key does not sit
	// on the server waiting to be picked up later.
	filled := h.newPairing(t, h.token)
	if posted := h.sendKey(t, h.token, filled, `{"ciphertext":"`+pairingSealedKey+`"}`); posted.StatusCode != http.StatusNoContent {
		t.Fatalf("post = %d %q", posted.StatusCode, posted.Body)
	}
	h.expire(t, filled)
	if response := h.poll(t, h.token, filled); response.StatusCode != http.StatusNotFound || response.Body != notFoundBody {
		t.Errorf("poll for an expired filled pairing = %d %q, want 404 %q",
			response.StatusCode, response.Body, notFoundBody)
	}
}

// Two devices polling the same pairing at the same time: the key goes to
// exactly one of them, never to both.
func runPairingConcurrentPollScenario(t *testing.T, h pairingHarness) {
	t.Helper()
	id := h.newPairing(t, h.token)
	if posted := h.sendKey(t, h.token, id, `{"ciphertext":"`+pairingSealedKey+`"}`); posted.StatusCode != http.StatusNoContent {
		t.Fatalf("post = %d %q", posted.StatusCode, posted.Body)
	}

	const pollers = 8
	statuses := make([]int, pollers)
	bodies := make([]string, pollers)
	errs := make([]error, pollers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range pollers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, err := http.NewRequest(http.MethodGet, h.url+"/api/pairings/"+id, nil)
			if err != nil {
				errs[i] = err
				return
			}
			request.Header.Set("Authorization", "Bearer "+h.token)
			<-start
			response, err := h.client.Do(request)
			if err != nil {
				errs[i] = err
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			statuses[i], bodies[i], errs[i] = response.StatusCode, string(body), err
		}()
	}
	close(start)
	wg.Wait()

	winners := 0
	for i := range pollers {
		if errs[i] != nil {
			t.Fatalf("poll %d: %v", i, errs[i])
		}
		switch statuses[i] {
		case http.StatusOK:
			winners++
			if !strings.Contains(bodies[i], pairingSealedKey) {
				t.Errorf("poll %d won with %q, want the ciphertext", i, bodies[i])
			}
		case http.StatusNotFound:
		default:
			t.Errorf("poll %d = %d %q, want 200 or 404", i, statuses[i], bodies[i])
		}
	}
	if winners != 1 {
		t.Fatalf("%d of %d concurrent polls got the vault key, want exactly 1", winners, pollers)
	}
}

func runPairingValidationScenario(t *testing.T, h pairingHarness) {
	t.Helper()

	for _, refused := range []struct{ what, body string }{
		{"no body at all", ""},
		{"not JSON", "{"},
		{"no public key", `{"device_name":"Kitchen laptop","platform":"desktop"}`},
		{"an empty public key", `{"public_key":"","device_name":"Kitchen laptop","platform":"desktop"}`},
		{"no device name", `{"public_key":"` + pairingPublicKey + `","platform":"desktop"}`},
		{"no platform", `{"public_key":"` + pairingPublicKey + `","device_name":"Kitchen laptop"}`},
		{"an over-long public key", `{"public_key":"` + strings.Repeat("k", maxPairingField+1) +
			`","device_name":"Kitchen laptop","platform":"desktop"}`},
		{"an over-long device name", `{"public_key":"` + pairingPublicKey +
			`","device_name":"` + strings.Repeat("n", maxPairingField+1) + `","platform":"desktop"}`},
	} {
		if response := h.create(t, h.token, refused.body); response.StatusCode != http.StatusBadRequest {
			t.Errorf("create with %s = %d %q, want 400", refused.what, response.StatusCode, response.Body)
		}
	}

	id := h.newPairing(t, h.token)
	for _, refused := range []struct{ what, body string }{
		{"no body at all", ""},
		{"not JSON", "{"},
		{"no ciphertext", `{}`},
		{"an empty ciphertext", `{"ciphertext":""}`},
		{"an over-long ciphertext", `{"ciphertext":"` + strings.Repeat("c", maxPairingField+1) + `"}`},
	} {
		if response := h.sendKey(t, h.token, id, refused.body); response.StatusCode != http.StatusBadRequest {
			t.Errorf("post with %s = %d %q, want 400", refused.what, response.StatusCode, response.Body)
		}
	}
	// None of those refusals filled it.
	if pending := h.poll(t, h.token, id); pending.StatusCode != http.StatusAccepted {
		t.Fatalf("poll after refused posts = %d %q, want 202", pending.StatusCode, pending.Body)
	}
}

// An over-long body is refused before it can be stored, so an authenticated
// account cannot use the relay as an unbounded write into the database.
func runPairingOversizedBodyScenario(t *testing.T, h pairingHarness) {
	t.Helper()
	id := h.newPairing(t, h.token)
	huge := `{"ciphertext":"` + strings.Repeat("x", 2*maxPairingBody) + `"}`
	if response := h.sendKey(t, h.token, id, huge); response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("post of a %d-byte body = %d %q, want 413", len(huge), response.StatusCode, response.Body)
	}
	if pending := h.poll(t, h.token, id); pending.StatusCode != http.StatusAccepted {
		t.Fatalf("poll after the oversized post = %d %q, want 202 — nothing was stored",
			pending.StatusCode, pending.Body)
	}
}

// Each Postgres harness needs its own accounts, because the scratch database
// is shared across runs and across the tests in this file.
func newPostgresPairingHarness(t *testing.T) pairingHarness {
	t.Helper()
	return newPairingHarness(t, postgresKeyDatabase(t), "pairing-pg-"+uuid.NewV7().String())
}

func TestPairingRelayOnSQLite(t *testing.T) {
	runPairingRelayScenario(t, newPairingHarness(t, sqliteKeyDatabase(t), "pairing-sqlite"))
}

func TestPairingRelayOnPostgres(t *testing.T) {
	runPairingRelayScenario(t, newPostgresPairingHarness(t))
}

func TestPairingRefusesASecondPostOnSQLite(t *testing.T) {
	runPairingSecondPostScenario(t, newPairingHarness(t, sqliteKeyDatabase(t), "pairing-second-sqlite"))
}

func TestPairingRefusesASecondPostOnPostgres(t *testing.T) {
	runPairingSecondPostScenario(t, newPostgresPairingHarness(t))
}

func TestPairingIsInvisibleToAnotherAccountOnSQLite(t *testing.T) {
	runPairingCrossAccountScenario(t, newPairingHarness(t, sqliteKeyDatabase(t), "pairing-cross-sqlite"))
}

func TestPairingIsInvisibleToAnotherAccountOnPostgres(t *testing.T) {
	runPairingCrossAccountScenario(t, newPostgresPairingHarness(t))
}

func TestPairingExpiresOnSQLite(t *testing.T) {
	runPairingExpiryScenario(t, newPairingHarness(t, sqliteKeyDatabase(t), "pairing-expiry-sqlite"))
}

func TestPairingExpiresOnPostgres(t *testing.T) {
	runPairingExpiryScenario(t, newPostgresPairingHarness(t))
}

func TestPairingHandsTheKeyToOneOfTwoConcurrentPollsOnSQLite(t *testing.T) {
	runPairingConcurrentPollScenario(t, newPairingHarness(t, sqliteKeyDatabase(t), "pairing-race-sqlite"))
}

func TestPairingHandsTheKeyToOneOfTwoConcurrentPollsOnPostgres(t *testing.T) {
	runPairingConcurrentPollScenario(t, newPostgresPairingHarness(t))
}

func TestPairingValidatesItsBodiesOnSQLite(t *testing.T) {
	runPairingValidationScenario(t, newPairingHarness(t, sqliteKeyDatabase(t), "pairing-valid-sqlite"))
}

func TestPairingValidatesItsBodiesOnPostgres(t *testing.T) {
	runPairingValidationScenario(t, newPostgresPairingHarness(t))
}

func TestPairingRefusesAnOversizedBodyOnSQLite(t *testing.T) {
	runPairingOversizedBodyScenario(t, newPairingHarness(t, sqliteKeyDatabase(t), "pairing-big-sqlite"))
}

func TestPairingRefusesAnOversizedBodyOnPostgres(t *testing.T) {
	runPairingOversizedBodyScenario(t, newPostgresPairingHarness(t))
}

// A lapsed subscription pauses writes, but not this one: someone who stopped
// paying must still be able to set up a device and read what they already
// stored.
func TestALapsedAccountCanStillPairANewDevice(t *testing.T) {
	h := newQuotaHarness(t)
	h.deliver(t, "msg_pairing_revoked", "subscription.revoked", "canceled")
	if h.entitlement(t).Entitled {
		t.Fatal("entitled = true after the subscription was revoked")
	}

	created := h.do(t, http.MethodPost, "/api/pairings", pairingOffer)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create pairing on a lapsed account = %d %q, want 201", created.StatusCode, created.Body)
	}
	var offer struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created.Body), &offer); err != nil {
		t.Fatal(err)
	}

	posted := h.do(t, http.MethodPost, "/api/pairings/"+offer.ID+"/key",
		`{"ciphertext":"`+pairingSealedKey+`"}`)
	if posted.StatusCode != http.StatusNoContent {
		t.Fatalf("post ciphertext on a lapsed account = %d %q, want 204", posted.StatusCode, posted.Body)
	}
	collected := h.do(t, http.MethodGet, "/api/pairings/"+offer.ID, "")
	if collected.StatusCode != http.StatusOK || !strings.Contains(collected.Body, pairingSealedKey) {
		t.Fatalf("poll on a lapsed account = %d %q, want the ciphertext", collected.StatusCode, collected.Body)
	}
}
