package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/hosted/config"
)

func TestDevBillingEvents(t *testing.T) {
	h := newBillingHarness(t)
	app, tokens := h.appLogin(h.newSub())
	event := func(kind string) billingBody {
		t.Helper()
		var b billingBody
		app.expect(http.MethodPost, "/dev/billing/events/"+kind, nil, http.StatusOK).json(t, &b)
		return b
	}

	if b := event("grant"); !b.Entitled || str(b.Source) != "web" || b.WillRenew {
		t.Fatalf("grant = %+v", b)
	}
	if b := event("lapse"); b.Entitled || str(b.Status) != "expired" {
		t.Fatalf("lapse = %+v", b)
	}
	event("grant")
	if b := event("expire"); b.Entitled || str(b.Status) != "active" || b.ExpiresAt == nil || b.ExpiresAt.After(time.Now()) {
		t.Fatalf("expire = %+v, want active with its period over", b)
	}
	event("grant")
	if b := event("refund"); b.Entitled || str(b.Status) != "revoked" {
		t.Fatalf("refund = %+v", b)
	}
	// The mock agrees with the synthetic events, so reconciliation keeps them.
	if subs := h.mock.Subscriptions(tokens.User.ID); len(subs) != 3 {
		t.Fatalf("mock subscriptions = %+v, want the three grants", subs)
	}
	app.expect(http.MethodPost, "/dev/jobs/billing-reconciliation", nil, http.StatusOK)
	if app.billing().Entitled {
		t.Fatal("reconciliation undid the synthetic events")
	}

	if res := app.do(http.MethodPost, "/dev/billing/events/gift", nil); res.status != http.StatusNotFound {
		t.Fatalf("unknown event = %d %s", res.status, res.body)
	}
	if res := h.client().do(http.MethodPost, "/dev/billing/events/grant", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("signed-out event = %d %s", res.status, res.body)
	}
}

// TestDevLapseToPurge walks a lapse through the dev time controls to the
// purge: lapse, the lapse job, 83 days, 90 days.
func TestDevLapseToPurge(t *testing.T) {
	h := newBillingHarness(t)
	app, tokens := h.appLogin(h.newSub())
	app.expect(http.MethodPost, "/dev/billing/events/grant", nil, http.StatusOK)
	if res := app.do(http.MethodPost, "/dev/billing/lapsed-at?days=10", nil); res.status != http.StatusBadRequest {
		t.Fatalf("moving the lapse of an entitled account = %d %s", res.status, res.body)
	}
	app.expect(http.MethodPost, "/dev/billing/events/lapse", nil, http.StatusOK)
	var run struct {
		Lapsed   int `json:"lapsed"`
		Notified int `json:"notified"`
		Warned   int `json:"warned"`
		Purged   int `json:"purged"`
	}
	app.expect(http.MethodPost, "/dev/jobs/billing-lapses", nil, http.StatusOK).json(t, &run)
	if run.Lapsed < 1 || run.Notified < 1 {
		t.Fatalf("first lapse run = %+v", run)
	}
	for _, bad := range []string{"", "0", "366", "x"} {
		if res := app.do(http.MethodPost, "/dev/billing/lapsed-at?days="+bad, nil); res.status != http.StatusBadRequest {
			t.Fatalf("days=%q = %d %s", bad, res.status, res.body)
		}
	}
	var moved struct {
		LapsedAt time.Time `json:"lapsed_at"`
		PurgeAt  time.Time `json:"purge_at"`
	}
	app.expect(http.MethodPost, "/dev/billing/lapsed-at?days=83", nil, http.StatusOK).json(t, &moved)
	if until := time.Until(moved.PurgeAt); until > 7*24*time.Hour || until < 6*24*time.Hour {
		t.Fatalf("after moving 83 days: %+v", moved)
	}
	app.expect(http.MethodPost, "/dev/jobs/billing-lapses", nil, http.StatusOK).json(t, &run)
	if run.Warned != 1 || run.Purged != 0 {
		t.Fatalf("warning run = %+v", run)
	}
	app.expect(http.MethodPost, "/dev/billing/lapsed-at?days=7", nil, http.StatusOK)
	app.expect(http.MethodPost, "/dev/jobs/billing-lapses", nil, http.StatusOK).json(t, &run)
	if run.Purged != 1 {
		t.Fatalf("purge run = %+v", run)
	}
	var left int
	if err := h.database.QueryRowContext(t.Context(), `SELECT count(*) FROM users WHERE id = $1`, tokens.User.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("users left after the lapse purge: %d (%v)", left, err)
	}
	expectHostedError(t, app.do(http.MethodGet, "/api/billing", nil), http.StatusUnauthorized, "invalid_session")
}

func TestDevMockPagesNeedTheMock(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.DevUI = true })
	for _, path := range []string{"/dev/billing/checkout", "/dev/billing/portal"} {
		if res := h.client().do(http.MethodGet, path+"?customer=x&sig=y", nil); res.status != http.StatusNotFound ||
			!strings.Contains(res.body, "not_found") {
			t.Fatalf("GET %s without the mock = %d %s", path, res.status, res.body)
		}
	}
}
