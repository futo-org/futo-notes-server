package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/storagesvc/adminapi"
)

func blobKeyFor(userID string) string { return userID + "/" + uuid.NewV7().String() }

func TestDeleteBlobs(t *testing.T) {
	e := newEnv(t)
	a, b, gone, kept := blobKeyFor(userA), blobKeyFor(userB), blobKeyFor(userA), blobKeyFor(userA)
	for _, k := range []string{a, b, kept} {
		e.store.put(k, []byte("x"))
	}
	resp := e.deleteBlobs(e.admin(), map[string][]string{"keys": {a, b, gone}})
	if resp.status != http.StatusOK {
		t.Fatalf("status %d: %s", resp.status, resp.body)
	}
	var body adminapi.DeleteBlobsResponse
	resp.json(t, &body)
	if body.Deleted != 3 || len(body.Failed) != 0 {
		t.Fatalf("response %+v", body)
	}
	if keys := e.store.keys(); !slices.Equal(keys, []string{kept}) {
		t.Fatalf("left %v", keys)
	}
}

func TestDeleteBlobsReportsPerKeyFailures(t *testing.T) {
	e := newEnv(t)
	ok, stuck := blobKeyFor(userA), blobKeyFor(userA)
	e.store.put(ok, []byte("x"))
	e.store.put(stuck, []byte("x"))
	e.store.deleteFail = map[string]bool{stuck: true}
	resp := e.deleteBlobs(e.admin(), map[string][]string{"keys": {ok, stuck}})
	var body adminapi.DeleteBlobsResponse
	resp.json(t, &body)
	if resp.status != http.StatusOK || body.Deleted != 1 || len(body.Failed) != 1 || body.Failed[0].Key != stuck ||
		body.Failed[0].Message == "" {
		t.Fatalf("status %d, %+v", resp.status, body)
	}
}

func TestDeleteBlobsValidation(t *testing.T) {
	e := newEnv(t)
	tooMany := make([]string, adminapi.MaxDeleteKeys+1)
	for i := range tooMany {
		tooMany[i] = blobKeyFor(userA)
	}
	cases := map[string]string{
		"invalid json":        `{"keys":`,
		"empty":               `{"keys":[]}`,
		"null key":            `{"keys":[null]}`,
		"no slash":            `{"keys":["` + userA + `"]}`,
		"bad blob id":         `{"keys":["` + userA + `/x"]}`,
		"uppercase user":      `{"keys":["` + strings.ToUpper(userA) + "/" + uuid.NewV7().String() + `"]}`,
		"nested path":         `{"keys":["` + blobKeyFor(userA) + `/x"]}`,
		"too many keys":       `{"keys":["` + strings.Join(tooMany, `","`) + `"]}`,
		"path traversal user": `{"keys":["../` + uuid.NewV7().String() + `"]}`,
	}
	for name, body := range cases {
		resp := e.do(http.MethodDelete, "/internal/blobs", e.admin(), strings.NewReader(body), nil)
		if resp.status != http.StatusBadRequest {
			t.Fatalf("%s: status %d: %s", name, resp.status, resp.body)
		}
		if code := resp.errorCode(t); code != "invalid_request" {
			t.Fatalf("%s: code %q", name, code)
		}
	}
	huge := `{"keys":["` + strings.Repeat("x", maxDeleteBody) + `"]}`
	if resp := e.do(http.MethodDelete, "/internal/blobs", e.admin(), strings.NewReader(huge), nil); resp.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge body: status %d", resp.status)
	}
	exactly := tooMany[:adminapi.MaxDeleteKeys]
	if resp := e.deleteBlobs(e.admin(), map[string][]string{"keys": exactly}); resp.status != http.StatusOK {
		t.Fatalf("1000 keys: status %d: %s", resp.status, resp.body)
	}
}

func (e *env) list(query url.Values) (adminapi.ListBlobsResponse, response) {
	e.t.Helper()
	resp := e.do(http.MethodGet, "/internal/blobs?"+query.Encode(), e.admin(), nil, nil)
	var body adminapi.ListBlobsResponse
	if resp.status == http.StatusOK {
		resp.json(e.t, &body)
	}
	return body, resp
}

func TestListBlobsPagesThroughAPrefix(t *testing.T) {
	e := newEnv(t)
	var want []string
	for range 5 {
		k := blobKeyFor(userA)
		e.store.put(k, []byte("abc"))
		want = append(want, k)
	}
	other := blobKeyFor(userB)
	e.store.put(other, []byte("x"))
	slices.Sort(want)

	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("listing does not end")
		}
		q := url.Values{"prefix": {userA + "/"}, "limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		body, resp := e.list(q)
		if resp.status != http.StatusOK || len(body.Blobs) > 2 {
			t.Fatalf("status %d: %s", resp.status, resp.body)
		}
		for _, b := range body.Blobs {
			if b.Size != 3 || b.LastModified.IsZero() {
				t.Fatalf("blob %+v", b)
			}
			got = append(got, b.Key)
		}
		if body.NextCursor == nil {
			break
		}
		cursor = *body.NextCursor
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}

	all, raw := e.list(url.Values{})
	if len(all.Blobs) != 6 || all.NextCursor != nil {
		t.Fatalf("whole bucket: %d blobs, cursor %v", len(all.Blobs), all.NextCursor)
	}
	if !strings.Contains(string(raw.body), `"next_cursor":null`) {
		t.Fatal("a finished listing must say next_cursor: null")
	}
}

func TestListBlobsOlderThan(t *testing.T) {
	e := newEnv(t)
	cutoff := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	old, fresh, boundary := blobKeyFor(userA), blobKeyFor(userA), blobKeyFor(userA)
	e.store.putAt(old, []byte("x"), cutoff.Add(-time.Hour))
	e.store.putAt(fresh, []byte("x"), cutoff.Add(time.Hour))
	e.store.putAt(boundary, []byte("x"), cutoff)

	body, resp := e.list(url.Values{"older_than": {cutoff.Format(time.RFC3339)}})
	if resp.status != http.StatusOK || len(body.Blobs) != 1 || body.Blobs[0].Key != old {
		t.Fatalf("status %d: %s", resp.status, resp.body)
	}
	if !body.Blobs[0].LastModified.Equal(cutoff.Add(-time.Hour)) {
		t.Fatalf("last_modified %v", body.Blobs[0].LastModified)
	}

	// Filtering after S3 pages means a page can be empty mid-listing.
	body, _ = e.list(url.Values{"older_than": {cutoff.Add(-24 * time.Hour).Format(time.RFC3339)}, "limit": {"1"}})
	if len(body.Blobs) != 0 || body.NextCursor == nil {
		t.Fatalf("got %+v", body)
	}
}

func TestListBlobsValidation(t *testing.T) {
	e := newEnv(t)
	for name, q := range map[string]url.Values{
		"prefix without slash": {"prefix": {userA}},
		"uppercase prefix":     {"prefix": {strings.ToUpper(userA) + "/"}},
		"key as prefix":        {"prefix": {blobKeyFor(userA)}},
		"partial prefix":       {"prefix": {userA[:8]}},
		"bad older_than":       {"older_than": {"yesterday"}},
		"zero limit":           {"limit": {"0"}},
		"limit over 1000":      {"limit": {"1001"}},
		"limit not a number":   {"limit": {"ten"}},
	} {
		_, resp := e.list(q)
		if resp.status != http.StatusBadRequest {
			t.Fatalf("%s: status %d", name, resp.status)
		}
		if code := resp.errorCode(t); code != "invalid_request" {
			t.Fatalf("%s: code %q", name, code)
		}
	}
	e.store.listErr = errors.New("s3 down")
	if _, resp := e.list(url.Values{}); resp.status != http.StatusInternalServerError || resp.errorCode(t) != "internal_error" {
		t.Fatalf("storage failure: status %d: %s", resp.status, resp.body)
	}
}

func TestDeleteUserLoopsUntilDone(t *testing.T) {
	e := newEnv(t)
	for range adminapi.MaxDeleteKeys + 1 {
		e.store.put(blobKeyFor(userA), []byte("x"))
	}
	survivor := blobKeyFor(userB)
	e.store.put(survivor, []byte("x"))

	for i, want := range []adminapi.DeleteUserResponse{
		{Deleted: adminapi.MaxDeleteKeys, Done: false},
		{Deleted: 1, Done: true},
		{Deleted: 0, Done: true},
	} {
		resp := e.do(http.MethodDelete, "/internal/users/"+userA, e.admin(), nil, nil)
		var got adminapi.DeleteUserResponse
		resp.json(t, &got)
		if resp.status != http.StatusOK || got != want {
			t.Fatalf("call %d: status %d, %+v, want %+v", i, resp.status, got, want)
		}
	}
	if keys := e.store.keys(); !slices.Equal(keys, []string{survivor}) {
		t.Fatalf("left %v", keys)
	}
}

func TestDeleteUserValidationAndFailures(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"nobody", strings.ToUpper(userA), userA[:35]} {
		resp := e.do(http.MethodDelete, "/internal/users/"+id, e.admin(), nil, nil)
		if resp.status != http.StatusBadRequest || resp.errorCode(t) != "invalid_request" {
			t.Fatalf("%q: status %d: %s", id, resp.status, resp.body)
		}
	}

	stuck := blobKeyFor(userA)
	e.store.put(stuck, []byte("x"))
	e.store.deleteFail = map[string]bool{stuck: true}
	resp := e.do(http.MethodDelete, "/internal/users/"+userA, e.admin(), nil, nil)
	if resp.status != http.StatusInternalServerError || resp.errorCode(t) != "internal_error" ||
		resp.errorBody(t).Message != fmt.Sprintf("%d blobs could not be deleted", 1) {
		t.Fatalf("status %d: %s", resp.status, resp.body)
	}

	e.store.listErr = errors.New("s3 down")
	resp = e.do(http.MethodDelete, "/internal/users/"+userA, e.admin(), nil, nil)
	if resp.status != http.StatusInternalServerError || resp.errorCode(t) != "internal_error" {
		t.Fatalf("status %d: %s", resp.status, resp.body)
	}
}
