package handlers

import (
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/blobframe"
	"futo-notes-server/internal/hostedauth"
)

type endpoint struct {
	method, path, body string
	contentType        string
}

func blobEndpoints() []endpoint {
	return []endpoint{
		{http.MethodPost, "/blobs", "x", ""},
		{http.MethodPost, "/blobs/batch", `{"keys":["k"]}`, ""},
		{http.MethodPost, "/blobs/batch-upload", string(blobframe.AppendUpload(nil, "f", []byte("x"))), "application/octet-stream"},
		{http.MethodGet, "/blobs/" + userA + "/" + uuid.NewV7().String(), "", ""},
	}
}

func adminEndpoints() []endpoint {
	return []endpoint{
		{http.MethodDelete, "/internal/blobs", `{"keys":["` + userA + "/" + uuid.NewV7().String() + `"]}`, ""},
		{http.MethodGet, "/internal/blobs", "", ""},
		{http.MethodDelete, "/internal/users/" + userA, "", ""},
	}
}

func (e *env) call(ep endpoint, authorization string) response {
	e.t.Helper()
	header := map[string]string{}
	if authorization != "" {
		header["Authorization"] = authorization
	}
	if ep.contentType != "" {
		header["Content-Type"] = ep.contentType
	}
	return e.do(ep.method, ep.path, "", strings.NewReader(ep.body), header)
}

// otherTokens mints tokens that must never open this region: valid tokens of
// the wrong region, expired ones, ones signed by a key the region does not
// trust, and receipts.
type otherTokens struct {
	otherRegionAccess, otherRegionAdmin string
	expiredAccess, expiredAdmin         string
	untrustedAccess, untrustedAdmin     string
	receipt                             string
}

func (e *env) otherTokens() otherTokens {
	e.t.Helper()
	var o otherTokens
	var err error
	if o.otherRegionAccess, _, err = e.minter.MintAccess("eu-central", userA); err != nil {
		e.t.Fatal(err)
	}
	if o.otherRegionAdmin, err = e.minter.MintAdmin("eu-central"); err != nil {
		e.t.Fatal(err)
	}

	// The verifier allows 30s of clock skew.
	past := hostedauth.NewMinter(e.apiKey, hostedauth.WithClock(func() time.Time { return time.Now().Add(-25 * time.Hour) }))
	if o.expiredAccess, _, err = past.MintAccess(testRegion, userA); err != nil {
		e.t.Fatal(err)
	}
	if o.expiredAdmin, err = past.MintAdmin(testRegion); err != nil {
		e.t.Fatal(err)
	}

	untrusted := hostedauth.NewMinter(mustKey(e.t, "api-test"))
	if o.untrustedAccess, _, err = untrusted.MintAccess(testRegion, userA); err != nil {
		e.t.Fatal(err)
	}
	if o.untrustedAdmin, err = untrusted.MintAdmin(testRegion); err != nil {
		e.t.Fatal(err)
	}

	resp := e.upload(e.access(userA), []byte("x"))
	var body UploadResponse
	resp.json(e.t, &body)
	o.receipt = body.Receipt
	return o
}

func TestBlobEndpointsRejectEverythingButAnAccessToken(t *testing.T) {
	e := newEnv(t)
	o := e.otherTokens()
	rejected := map[string]string{
		"no header":                 "",
		"basic auth":                "Basic dXNlcjpwYXNz",
		"empty bearer":              "Bearer ",
		"garbage":                   "Bearer not-a-jwt",
		"admin token":               "Bearer " + e.admin(),
		"access for another region": "Bearer " + o.otherRegionAccess,
		"expired access":            "Bearer " + o.expiredAccess,
		"untrusted key":             "Bearer " + o.untrustedAccess,
		"receipt":                   "Bearer " + o.receipt,
	}
	for _, ep := range blobEndpoints() {
		for name, authorization := range rejected {
			resp := e.call(ep, authorization)
			if resp.status != http.StatusUnauthorized {
				t.Fatalf("%s %s with %s: status %d", ep.method, ep.path, name, resp.status)
			}
			want := "invalid_token"
			if authorization == "" || !strings.HasPrefix(authorization, "Bearer ") || authorization == "Bearer " {
				want = "unauthorized"
			}
			if code := resp.errorCode(t); code != want {
				t.Fatalf("%s %s with %s: code %q, want %q", ep.method, ep.path, name, code, want)
			}
		}
		if resp := e.call(ep, "Bearer "+e.access(userA)); resp.status == http.StatusUnauthorized {
			t.Fatalf("%s %s refused a valid access token", ep.method, ep.path)
		}
	}
}

func TestAdminEndpointsRejectEverythingButAnAdminToken(t *testing.T) {
	e := newEnv(t)
	o := e.otherTokens()
	rejected := map[string]string{
		"no header":                "",
		"garbage":                  "Bearer not-a-jwt",
		"access token":             "Bearer " + e.access(userA),
		"admin for another region": "Bearer " + o.otherRegionAdmin,
		"expired admin":            "Bearer " + o.expiredAdmin,
		"untrusted key":            "Bearer " + o.untrustedAdmin,
		"receipt":                  "Bearer " + o.receipt,
	}
	for _, ep := range adminEndpoints() {
		for name, authorization := range rejected {
			if resp := e.call(ep, authorization); resp.status != http.StatusUnauthorized {
				t.Fatalf("%s %s with %s: status %d", ep.method, ep.path, name, resp.status)
			}
		}
		if resp := e.call(ep, "Bearer "+e.admin()); resp.status != http.StatusOK {
			t.Fatalf("%s %s with an admin token: status %d: %s", ep.method, ep.path, resp.status, resp.body)
		}
	}
}

// TestReadOnlyAccessReadsButCannotUpload checks the scope an account without
// a subscription gets: uploads answer 403 write_not_allowed and store
// nothing, while reads of what the account already has keep working.
func TestReadOnlyAccessReadsButCannotUpload(t *testing.T) {
	e := newEnv(t)
	var stored UploadResponse
	e.upload(e.access(userA), []byte("kept")).json(t, &stored)
	readOnly := "Bearer " + e.readAccess(userA)

	for _, ep := range blobEndpoints() {
		if ep.path != "/blobs" && ep.path != "/blobs/batch-upload" {
			continue
		}
		resp := e.call(ep, readOnly)
		if resp.status != http.StatusForbidden {
			t.Fatalf("%s %s with a read-only token: status %d: %s", ep.method, ep.path, resp.status, resp.body)
		}
		if code := resp.errorCode(t); code != "write_not_allowed" {
			t.Fatalf("%s %s with a read-only token: code %q, want write_not_allowed", ep.method, ep.path, code)
		}
	}
	if keys := e.store.keys(); len(keys) != 1 || keys[0] != stored.Key {
		t.Fatalf("stored keys after refused uploads = %v, want only %s", keys, stored.Key)
	}

	get := e.call(endpoint{http.MethodGet, "/blobs/" + stored.Key, "", ""}, readOnly)
	if get.status != http.StatusOK || string(get.body) != "kept" {
		t.Fatalf("GET with a read-only token = %d %q", get.status, get.body)
	}
	batch := e.call(endpoint{http.MethodPost, "/blobs/batch", `{"keys":["` + stored.Key + `"]}`, ""}, readOnly)
	if batch.status != http.StatusOK {
		t.Fatalf("POST /blobs/batch with a read-only token = %d %s", batch.status, batch.body)
	}
}
