package api_test

import (
	"net/http"
	"testing"
)

func TestRestoreIsRefusedOnceThePurgeIsDue(t *testing.T) {
	h := newHarness(t)
	app, tokens := h.appLogin(h.newSub())
	app.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
	if _, err := h.database.ExecContext(t.Context(), `UPDATE users SET purge_at = now() - interval '1 second' WHERE id = $1`,
		tokens.User.ID); err != nil {
		t.Fatal(err)
	}
	expectHostedError(t, app.do(http.MethodPost, "/api/account/restore", nil), http.StatusConflict)
	var account accountBody
	app.expect(http.MethodGet, "/api/account", nil, http.StatusOK).json(t, &account)
	if account.Deletion == nil {
		t.Fatal("a refused restore cancelled the deletion")
	}
}

func TestDashboardRestoreIsRefusedOnceThePurgeIsDue(t *testing.T) {
	h := newHarness(t)
	browser := h.dashboardLogin(h.newSub())
	browser.expect(http.MethodDelete, "/api/account", nil, http.StatusOK)
	if _, err := h.database.ExecContext(t.Context(), `UPDATE users SET purge_at = now() - interval '1 second' WHERE id = $1`,
		browser.account().User.ID); err != nil {
		t.Fatal(err)
	}
	expectAlertFragment(t, browser.hxSubmit("/dashboard/restore", nil), http.StatusConflict, "can no longer be restored")
	if browser.account().Deletion == nil {
		t.Fatal("a refused dashboard restore cancelled the deletion")
	}
}
