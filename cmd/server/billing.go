package main

import (
	"net/http"

	"futo-notes-server/internal/db"
	"futo-notes-server/internal/usage"
)

// handleBillingStatus answers what an app needs to show an account's limits:
// the Plan it is on and how much of its Storage Quota it has used.
// Entitlement fields join this body once a subscription record exists.
func handleBillingStatus(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, err := usage.Of(r.Context(), database, sessionFrom(r).User.ID)
		if err != nil {
			serverError(w, r, "reading billing status", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"plan":  account.Plan,
			"usage": map[string]int64{"bytes_used": account.BytesUsed},
		})
	}
}

// storageLimits is what a route accepting ciphertext needs before it reads a
// body: how large one blob may be, and whether the account has room for it.
//
// The per-blob cap comes from the Plan in every deployment, and the one plan
// carries the value the server used as a constant, so nothing changes for a
// self-hoster. The Storage Quota is a hosted limit, so quotaEnforced is off
// outside hosted mode and a self-hosted server keeps storing whatever fits on
// its disk.
type storageLimits struct {
	account       usage.Account
	quotaEnforced bool
}

// readBlobBodyWithinLimits is the whole admission check a route accepting one
// blob of ciphertext performs: the body is read under the Plan's per-blob cap
// and refused if storing it would cross the account's Storage Quota. It writes
// the 413 or 507 itself, so a false second return means the response is done.
func readBlobBodyWithinLimits(w http.ResponseWriter, r *http.Request, database *db.DB, quotaEnforced bool) ([]byte, bool) {
	limits, ok := loadStorageLimits(w, r, database, quotaEnforced)
	if !ok {
		return nil, false
	}
	body, ok := readBlobBody(w, r, limits.account.Plan.BlobMaxBytes)
	if !ok {
		return nil, false
	}
	if limits.exceeded(int64(len(body))) {
		writeQuotaExceeded(w, limits)
		return nil, false
	}
	return body, true
}

func loadStorageLimits(w http.ResponseWriter, r *http.Request, database *db.DB, quotaEnforced bool) (storageLimits, bool) {
	account, err := usage.Of(r.Context(), database, sessionFrom(r).User.ID)
	if err != nil {
		serverError(w, r, "reading plan limits", err)
		return storageLimits{}, false
	}
	return storageLimits{account: account, quotaEnforced: quotaEnforced}, true
}

// exceeded reports whether storing incoming more bytes would put the account
// past its Storage Quota.
func (l storageLimits) exceeded(incoming int64) bool {
	return l.quotaEnforced && l.account.Exceeds(incoming)
}

// recordClaim adds bytes this request has just had claimed, so a batch weighs
// each entry against what the entries before it already took.
func (l *storageLimits) recordClaim(bytes int64) {
	l.account.BytesUsed += bytes
}

// writeQuotaExceeded names the quota and the usage behind the refusal, so a
// client can tell the person how much room is left rather than only that
// the write failed.
func writeQuotaExceeded(w http.ResponseWriter, l storageLimits) {
	writeJSON(w, http.StatusInsufficientStorage, map[string]any{
		"error":       "storage quota exceeded",
		"code":        "quota_exceeded",
		"quota_bytes": l.account.Plan.StorageQuotaBytes,
		"bytes_used":  l.account.BytesUsed,
	})
}

// batchQuotaExceeded is the same refusal as one entry of a batch response,
// which reports per entry rather than failing the whole request.
func (l storageLimits) batchQuotaExceeded() map[string]any {
	return map[string]any{
		"status":      "quota_exceeded",
		"quota_bytes": l.account.Plan.StorageQuotaBytes,
		"bytes_used":  l.account.BytesUsed,
	}
}
