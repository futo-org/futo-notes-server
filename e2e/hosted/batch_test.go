package hosted

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"uuid"

	"futo-notes-server/internal/blobframe"
)

// TestHostedBatchFirstSync is a first sync of a vault in batches: a laptop
// uploads every note to storage in one request and claims them all at the
// API in one more, retries the claim after a lost response, and a phone
// hears the changes, pulls and downloads the vault, then edits some of it
// in batches while the laptop races it on one note.
func TestHostedBatchFirstSync(t *testing.T) {
	s := startStack(t)
	const region = "us-east"
	const notes = 40
	sub := "e2e-batch-" + uuid.NewV7().String()

	laptop := s.login(t, sub, "e2e laptop")
	s.cleanupUser(t, laptop.userID)
	laptop.expectJSON(http.MethodPut, "/api/account/region", map[string]string{"region_id": region}, nil)
	collection := laptop.claimCollection()

	phone := s.login(t, sub, "e2e phone")
	stream := phone.subscribe()

	// One storage request for the whole vault, each frame named by the
	// Mutation-Id of the create that will claim it.
	vault := make([][]byte, notes)
	ids := make([]string, notes)
	for i := range vault {
		vault[i] = randomBytes(t, 512+i*97)
		ids[i] = fmt.Sprintf("first-sync-%03d", i)
	}
	grant := laptop.storageToken()
	uploads := laptop.uploadBatch(grant, ids, vault)

	// One API request claims them all, each entry its own create.
	entries := make([]map[string]any, notes)
	for i, u := range uploads {
		entries[i] = map[string]any{"mutation_id": ids[i], "receipt": u.Receipt}
	}
	results := laptop.claimBatch(collection, entries)
	created := make([]mutation, notes)
	for i, r := range results {
		if r.Status != http.StatusCreated {
			t.Fatalf("claim %d: %d %s", i, r.Status, r.Body)
		}
		if err := json.Unmarshal(r.Body, &created[i]); err != nil {
			t.Fatal(err)
		}
		c := created[i]
		if c.Replayed == nil || *c.Replayed || c.Object.BlobKey == nil || *c.Object.BlobKey != uploads[i].Key ||
			c.Object.Version != "1" || c.CollectionVersion != int64(i+1) {
			t.Fatalf("claim %d returned %s", i, r.Body)
		}
	}

	// The response was lost: the laptop sends the same batch again. Every
	// entry replays its create; nothing new is made.
	for i, r := range laptop.claimBatch(collection, entries) {
		want := strings.Replace(string(results[i].Body), `"replayed":false`, `"replayed":true`, 1)
		if r.Status != http.StatusCreated || string(r.Body) != want {
			t.Fatalf("retried claim %d: %d %s, want 201 %s", i, r.Status, r.Body, want)
		}
	}
	if n := s.count(t, `SELECT count(*) FROM objects WHERE collection_id = $1`, collection); n != notes {
		t.Fatalf("%d objects after the retried batch, want %d", n, notes)
	}

	// The phone hears the creates, each a change of its own, coalesced at
	// most, never repeated by the retry.
	seen := int64(0)
	for seen < notes {
		var change struct {
			CollectionID   string `json:"collectionId"`
			CurrentVersion int64  `json:"currentVersion"`
		}
		if err := json.Unmarshal([]byte(stream.next("change").data), &change); err != nil {
			t.Fatal(err)
		}
		if change.CollectionID != collection || change.CurrentVersion <= seen || change.CurrentVersion > notes {
			t.Fatalf("change event %+v after version %d", change, seen)
		}
		seen = change.CurrentVersion
	}

	// It pulls the vault and downloads it in storage batches.
	var pulled struct {
		Objects []object `json:"objects"`
	}
	phone.expectJSON(http.MethodGet, objectPath(collection, "")+"?sinceVersion=0", nil, &pulled)
	if len(pulled.Objects) != notes {
		t.Fatalf("pulled %d objects, want %d", len(pulled.Objects), notes)
	}
	keys := make([]string, notes)
	for i, o := range pulled.Objects {
		keys[i] = *o.BlobKey
	}
	phoneGrant := phone.storageToken()
	for i, f := range phone.downloadBatch(phoneGrant, keys...) {
		if f.Status != blobframe.OK || f.Key != uploads[i].Key || !bytes.Equal(f.Blob, vault[i]) {
			t.Fatalf("downloaded frame %d: %s status %d, %d bytes", i, f.Key, f.Status, len(f.Blob))
		}
	}

	// The laptop edits note 0 first; the phone then edits notes 0 to 2 in
	// one batch. Its edit of note 0 conflicts, the other two land.
	laptopEdit := laptop.uploadBatch(grant, []string{"laptop-edit"}, [][]byte{[]byte("laptop v2")})
	laptopResult := laptop.claimBatch(collection, []map[string]any{{"mutation_id": "laptop-edit",
		"object_id": created[0].Object.ID, "version": 2, "receipt": laptopEdit[0].Receipt}})
	if laptopResult[0].Status != http.StatusOK {
		t.Fatalf("laptop edit: %d %s", laptopResult[0].Status, laptopResult[0].Body)
	}
	edits := [][]byte{[]byte("phone v2 of 0"), []byte("phone v2 of 1"), []byte("phone v2 of 2")}
	editIDs := []string{"phone-edit-0", "phone-edit-1", "phone-edit-2"}
	editUploads := phone.uploadBatch(phoneGrant, editIDs, edits)
	editEntries := make([]map[string]any, len(edits))
	for i, u := range editUploads {
		editEntries[i] = map[string]any{"mutation_id": editIDs[i], "object_id": created[i].Object.ID, "version": 2, "receipt": u.Receipt}
	}
	editResults := phone.claimBatch(collection, editEntries)
	wantConflict := fmt.Sprintf(`{"error":"version conflict","currentVersion":2,"currentBlobKey":%q}`, laptopEdit[0].Key)
	if editResults[0].Status != http.StatusConflict || string(editResults[0].Body) != wantConflict {
		t.Fatalf("phone edit of the note the laptop edited: %d %s, want 409 %s", editResults[0].Status, editResults[0].Body, wantConflict)
	}
	for i := 1; i < len(edits); i++ {
		var updated mutation
		if err := json.Unmarshal(editResults[i].Body, &updated); err != nil || editResults[i].Status != http.StatusOK ||
			updated.Object.Version != "2" || *updated.Object.BlobKey != editUploads[i].Key {
			t.Fatalf("phone edit %d: %d %s", i, editResults[i].Status, editResults[i].Body)
		}
	}
	if n := s.count(t, `SELECT count(*) FROM objects WHERE collection_id = $1`, collection); n != notes {
		t.Fatalf("%d objects after the edits, want %d", n, notes)
	}
}
