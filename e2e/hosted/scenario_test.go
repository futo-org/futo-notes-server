package hosted

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"futo-notes-server/internal/blobframe"
	"futo-notes-server/internal/hosted/hostedjobs"
	"futo-notes-server/internal/hostedauth"
	"uuid"
)

// TestHostedScenario drives the hosted service through a user's whole life:
// sign-in on several devices, region choice, uploads, sync, conflicts,
// retries, token abuse, deletion and restore, and the jobs that clean up
// after them.
func TestHostedScenario(t *testing.T) {
	s := startStack(t)
	began := time.Now()
	const home, away = "eu-central", "us-east"
	sub := "e2e-" + uuid.NewV7().String()

	// Sign in on a phone.
	phone := s.login(t, sub, "e2e phone")
	s.cleanupUser(t, phone.userID)

	// Regions, and choosing one, once.
	var regions []struct {
		ID       string `json:"id"`
		Endpoint string `json:"endpoint"`
	}
	phone.expectJSON(http.MethodGet, "/api/regions", nil, &regions)
	if len(regions) != len(regionIDs) {
		t.Fatalf("GET /api/regions listed %d regions, want %d", len(regions), len(regionIDs))
	}
	for _, r := range regions {
		if s.storage[r.ID] != r.Endpoint {
			t.Fatalf("region %s endpoint %q, want %q", r.ID, r.Endpoint, s.storage[r.ID])
		}
	}
	if resp := phone.exchange(phone.refresh); resp.status != http.StatusConflict {
		t.Fatalf("storage token before choosing a region: status %d, want 409: %s", resp.status, resp.body)
	}
	phone.expectJSON(http.MethodPut, "/api/account/region", map[string]string{"region_id": home}, nil)
	phone.expect("choosing a second region", phone.api(http.MethodPut, "/api/account/region",
		map[string]string{"region_id": away}, ""), http.StatusConflict)

	// Upload to the home region and claim the blob with its receipt.
	grant := phone.storageToken()
	if grant.Region.ID != home || grant.Region.Endpoint != s.storage[home] {
		t.Fatalf("storage token for region %+v, want %s at %s", grant.Region, home, s.storage[home])
	}
	v1 := randomBytes(t, 96<<10)
	up1 := phone.upload(grant, v1)
	collection := phone.claimCollection()

	// A laptop signs in as the same user, with its own session, and listens.
	laptop := s.login(t, sub, "e2e laptop")
	if laptop.userID != phone.userID || laptop.token == phone.token {
		t.Fatalf("laptop session: user %s token reused %t, want user %s and a new session",
			laptop.userID, laptop.token == phone.token, phone.userID)
	}
	var sessions struct {
		Sessions []struct {
			ID        string `json:"id"`
			UserAgent string `json:"user_agent"`
			Current   bool   `json:"current"`
		} `json:"sessions"`
	}
	phone.expectJSON(http.MethodGet, "/api/sessions", nil, &sessions)
	if len(sessions.Sessions) != 2 {
		t.Fatalf("GET /api/sessions: %+v, want the phone and the laptop", sessions.Sessions)
	}
	stream := laptop.subscribe()

	createBody := map[string]string{"receipt": up1.Receipt}
	resp := phone.api(http.MethodPost, objectPath(collection, ""), createBody, "e2e-create-1")
	phone.expect("create object", resp, http.StatusCreated)
	var created mutation
	decode(t, resp, &created)
	if created.Object.BlobKey == nil || *created.Object.BlobKey != up1.Key || created.Object.Version != "1" ||
		created.Replayed == nil || *created.Replayed {
		t.Fatalf("create returned %s", resp.body)
	}
	if created.Object.SizeBytes == nil || *created.Object.SizeBytes != "98304" {
		t.Fatalf("create size_bytes %v, want the receipt's 98304", created.Object.SizeBytes)
	}

	// The laptop hears about it, pulls, and downloads identical bytes.
	var change struct {
		CollectionID   string `json:"collectionId"`
		CurrentVersion int64  `json:"currentVersion"`
	}
	if err := json.Unmarshal([]byte(stream.next("change").data), &change); err != nil {
		t.Fatal(err)
	}
	if change.CollectionID != collection || change.CurrentVersion != created.CollectionVersion {
		t.Fatalf("change event %+v, want collection %s at version %d", change, collection, created.CollectionVersion)
	}
	var pulled struct {
		Objects []object `json:"objects"`
	}
	laptop.expectJSON(http.MethodGet, objectPath(collection, "")+"?sinceVersion=0", nil, &pulled)
	if len(pulled.Objects) != 1 || pulled.Objects[0].ID != created.Object.ID || *pulled.Objects[0].BlobKey != up1.Key {
		t.Fatalf("pull returned %+v, want the created object", pulled.Objects)
	}
	laptopGrant := laptop.storageToken()
	if got := laptop.download(laptopGrant, up1.Key); !bytes.Equal(got, v1) {
		t.Fatalf("single download returned %d bytes that differ from the %d uploaded", len(got), len(v1))
	}
	missing := laptop.userID + "/" + uuid.NewV7().String()
	frames := laptop.downloadBatch(laptopGrant, up1.Key, missing)
	if len(frames) != 2 || frames[0].Key != up1.Key || frames[0].Status != blobframe.OK || !bytes.Equal(frames[0].Blob, v1) ||
		frames[1].Key != missing || frames[1].Status != blobframe.Missing {
		t.Fatalf("batch download frames %+v, want the blob then a missing frame", frameSummary(frames))
	}

	// The laptop updates the object with a new upload.
	v2 := randomBytes(t, 4<<10)
	up2 := laptop.upload(laptopGrant, v2)
	resp = laptop.api(http.MethodPut, objectPath(collection, created.Object.ID),
		map[string]any{"version": 2, "receipt": up2.Receipt}, "e2e-update-1")
	laptop.expect("update object", resp, http.StatusOK)
	var updated mutation
	decode(t, resp, &updated)
	if updated.Object.Version != "2" || *updated.Object.BlobKey != up2.Key {
		t.Fatalf("update returned %s", resp.body)
	}
	if ev := stream.next("change"); !strings.Contains(ev.data, collection) {
		t.Fatalf("change event %s, want collection %s", ev.data, collection)
	}

	// The phone, still at version 1, conflicts with it.
	up3 := phone.upload(grant, randomBytes(t, 1<<10))
	resp = phone.api(http.MethodPut, objectPath(collection, created.Object.ID),
		map[string]any{"version": 2, "receipt": up3.Receipt}, "e2e-update-2")
	phone.expect("conflicting update", resp, http.StatusConflict)
	var conflict struct {
		CurrentVersion int64   `json:"currentVersion"`
		CurrentBlobKey *string `json:"currentBlobKey"`
	}
	decode(t, resp, &conflict)
	if conflict.CurrentVersion != 2 || conflict.CurrentBlobKey == nil || *conflict.CurrentBlobKey != up2.Key {
		t.Fatalf("conflict body %s, want version 2 at %s", resp.body, up2.Key)
	}

	// A retry under the create's Mutation-Id replays the stored result; the
	// same receipt under a new id is refused.
	resp = phone.api(http.MethodPost, objectPath(collection, ""), createBody, "e2e-create-1")
	phone.expect("create retry", resp, http.StatusCreated)
	var replayed mutation
	decode(t, resp, &replayed)
	if replayed.Replayed == nil || !*replayed.Replayed || replayed.Object.ID != created.Object.ID || replayed.Object.Version != "1" {
		t.Fatalf("create retry returned %s, want the original create replayed", resp.body)
	}
	phone.expect("reusing a receipt", phone.api(http.MethodPost, objectPath(collection, ""), createBody, "e2e-create-2"),
		http.StatusConflict)

	// An access token for one region is refused by another's storage.
	foreign := grant
	foreign.Region.Endpoint = s.storage[away]
	resp = phone.do(request{method: http.MethodPost, url: foreign.Region.Endpoint + "/blobs", bearer: grant.AccessToken,
		body: bytes.NewReader([]byte("x")), length: 1})
	phone.expect("upload to another region", resp, http.StatusUnauthorized)
	resp = phone.do(request{method: http.MethodGet, url: foreign.Region.Endpoint + "/blobs/" + up1.Key, bearer: grant.AccessToken})
	phone.expect("download from another region", resp, http.StatusUnauthorized)

	// A revoked device loses its session, its refresh token and its open
	// event stream.
	tablet := s.login(t, sub, "e2e tablet")
	tabletSession := currentSession(tablet)
	tabletStream := tablet.subscribe()
	phone.expect("revoke tablet", phone.api(http.MethodDelete, "/api/sessions/"+tabletSession, nil, ""), http.StatusNoContent)
	tabletStream.ended("revoking the tablet")
	tablet.expect("revoked device's refresh token", tablet.exchange(tablet.refresh), http.StatusUnauthorized)
	tablet.expect("revoked device's session", tablet.api(http.MethodGet, "/api/auth", nil, ""), http.StatusUnauthorized)

	// Reusing a rotated refresh token signs the whole session out.
	watch := s.login(t, sub, "e2e watch")
	spent := watch.refresh
	watch.storageToken()
	// Until its successor is used, the spent token returns that successor
	// again, as after a lost response.
	var again storageGrant
	resp = watch.exchange(spent)
	watch.expect("refresh token presented again before its successor is used", resp, http.StatusOK)
	if err := json.Unmarshal(resp.body, &again); err != nil || again.RefreshToken != watch.refresh {
		t.Fatalf("re-presented refresh token: %v %s, want the successor", err, resp.body)
	}
	watch.expect("session after a lost response", watch.api(http.MethodGet, "/api/auth", nil, ""), http.StatusOK)
	// Once the successor is used, the spent token is reuse.
	watch.storageToken()
	watch.expect("refresh token reused", watch.exchange(spent), http.StatusUnauthorized)
	watch.expect("current refresh token after reuse", watch.exchange(watch.refresh), http.StatusUnauthorized)
	watch.expect("session after reuse", watch.api(http.MethodGet, "/api/auth", nil, ""), http.StatusUnauthorized)

	// Deleting the collection makes its blobs purgeable, but GC keeps them
	// while their receipts could still be presented.
	phone.expect("delete collection", phone.api(http.MethodDelete, "/api/collections/"+collection, nil, ""), http.StatusNoContent)
	var gc hostedjobs.GCResult
	s.runJob(t, "/dev/jobs/blob-gc", &gc)
	if gc.RowsPurged != 0 || !hasKeys(s.keys(t, home, phone.userID+"/"), up1.Key, up2.Key) {
		t.Fatalf("GC inside the receipt window: %+v, keys %v", gc, s.keys(t, home, phone.userID+"/"))
	}
	s.exec(t, `UPDATE blob_ledger SET created_at = created_at - $2::interval WHERE user_id = $1`,
		phone.userID, (hostedauth.ReceiptTTL + time.Hour).String())
	s.runJob(t, "/dev/jobs/blob-gc", &gc)
	if gc.RowsPurged != 2 || gc.KeysDeleted != 2 || gc.KeysFailed != 0 {
		t.Fatalf("GC after the receipt window: %+v, want 2 rows and 2 keys", gc)
	}
	keys := s.keys(t, home, phone.userID+"/")
	if hasAny(keys, up1.Key, up2.Key) || s.count(t, `SELECT count(*) FROM blob_ledger WHERE user_id = $1`, phone.userID) != 0 {
		t.Fatalf("after GC: keys %v, ledger rows %d", keys,
			s.count(t, `SELECT count(*) FROM blob_ledger WHERE user_id = $1`, phone.userID))
	}

	// The conflicting update's upload was never claimed. Reconciliation
	// leaves it while young, removes it once it is past OrphanMinAge, and
	// keeps a claimed blob beside it.
	collection = phone.claimCollection()
	up4 := phone.upload(grant, randomBytes(t, 2<<10))
	phone.expect("create kept object", phone.api(http.MethodPost, objectPath(collection, ""),
		map[string]string{"receipt": up4.Receipt}, "e2e-create-3"), http.StatusCreated)
	reconcile := "/dev/jobs/reconciliation?region=" + home + "&user=" + phone.userID
	var orphans hostedjobs.ReconcileResult
	s.runJob(t, reconcile, &orphans)
	if orphans.Deleted != 0 {
		t.Fatalf("reconciliation of young uploads: %+v, want nothing deleted", orphans)
	}
	s.runJob(t, reconcile+"&now="+time.Now().Add(hostedjobs.OrphanMinAge+time.Hour).UTC().Format(time.RFC3339), &orphans)
	if orphans.Listed != 2 || orphans.Deleted != 1 || orphans.Failed != 0 {
		t.Fatalf("reconciliation past OrphanMinAge: %+v, want 2 listed and 1 deleted", orphans)
	}
	if keys := s.keys(t, home, phone.userID+"/"); !slices.Equal(keys, []string{up4.Key}) {
		t.Fatalf("after reconciliation keys %v, want only the claimed %s", keys, up4.Key)
	}

	// Deleting the account blocks sync and storage tokens and signs out
	// every other device; restoring it brings them back.
	var account struct {
		Deletion *struct {
			PurgeAt time.Time `json:"purge_at"`
		} `json:"deletion"`
	}
	phone.expectJSON(http.MethodDelete, "/api/account", nil, &account)
	if account.Deletion == nil || time.Until(account.Deletion.PurgeAt) < 29*24*time.Hour {
		t.Fatalf("account deletion %+v, want a purge about 30 days out", account.Deletion)
	}
	laptop.expect("other device after account deletion", laptop.api(http.MethodGet, "/api/auth", nil, ""), http.StatusUnauthorized)
	stream.ended("deleting the account from another device")
	resp = phone.api(http.MethodGet, "/api/collections", nil, "")
	phone.expect("sync while deletion is pending", resp, http.StatusForbidden)
	if !strings.Contains(string(resp.body), "account pending deletion") {
		t.Fatalf("sync while deletion is pending: %s", resp.body)
	}
	phone.expect("storage token while deletion is pending", phone.exchange(phone.refresh), http.StatusForbidden)
	phone.expectJSON(http.MethodPost, "/api/account/restore", nil, &account)
	if account.Deletion != nil {
		t.Fatalf("restore left deletion %+v", account.Deletion)
	}
	phone.expect("sync after restore", phone.api(http.MethodGet, "/api/collections", nil, ""), http.StatusOK)
	grant = phone.storageToken()
	if got := phone.download(grant, up4.Key); len(got) != 2<<10 {
		t.Fatalf("download after restore returned %d bytes", len(got))
	}

	// Delete again and let the grace period lapse: purge empties the
	// storage prefix and removes every row.
	phone.expectJSON(http.MethodDelete, "/api/account", nil, nil)
	s.exec(t, `UPDATE users SET purge_at = now() - interval '1 minute' WHERE id = $1`, phone.userID)
	phone.expect("restore after the grace period", phone.api(http.MethodPost, "/api/account/restore", nil, ""), http.StatusConflict)
	var purge hostedjobs.PurgeResult
	s.runJob(t, "/dev/jobs/account-purge", &purge)
	if purge.Purged != 1 || purge.Failed != 0 || purge.BlobsDeleted != 1 {
		t.Fatalf("account purge: %+v, want 1 account and 1 blob", purge)
	}
	for _, region := range regionIDs {
		if keys := s.keys(t, region, phone.userID+"/"); len(keys) != 0 {
			t.Fatalf("after purge %s still holds %v", region, keys)
		}
	}
	for _, table := range []string{"users WHERE id", "sessions WHERE user_id", "collections WHERE user_id",
		"blob_ledger WHERE user_id", "mutation_results WHERE user_id"} {
		if n := s.count(t, "SELECT count(*) FROM "+table+" = $1", phone.userID); n != 0 {
			t.Fatalf("after purge %d rows remain in %s", n, table)
		}
	}
	phone.expect("session after purge", phone.api(http.MethodGet, "/api/auth", nil, ""), http.StatusUnauthorized)

	t.Logf("scenario ran in %s after the stack started", time.Since(began).Round(time.Millisecond))
}

// currentSession returns the id of d's own session.
func currentSession(d *device) string {
	d.t.Helper()
	var sessions struct {
		Sessions []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"sessions"`
	}
	d.expectJSON(http.MethodGet, "/api/sessions", nil, &sessions)
	for _, session := range sessions.Sessions {
		if session.Current {
			return session.ID
		}
	}
	d.t.Fatalf("GET /api/sessions marks no session current: %+v", sessions.Sessions)
	return ""
}

func hasKeys(keys []string, want ...string) bool {
	for _, key := range want {
		if !slices.Contains(keys, key) {
			return false
		}
	}
	return true
}

func hasAny(keys []string, want ...string) bool {
	return slices.ContainsFunc(want, func(key string) bool { return slices.Contains(keys, key) })
}

// frameSummary describes frames without their blob bytes.
func frameSummary(frames []blobframe.Frame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = fmt.Sprintf("%s status %d, %d bytes", f.Key, f.Status, len(f.Blob))
	}
	return out
}
