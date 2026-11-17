package objects

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidMutationID(t *testing.T) {
	for _, valid := range []string{"a", "retry_1", "a.b~c-d", strings.Repeat("x", 128)} {
		if !ValidMutationID(valid) {
			t.Errorf("ValidMutationID(%q) = false", valid)
		}
	}
	for _, invalid := range []string{"", "contains space", "slash/not-allowed", strings.Repeat("x", 129)} {
		if ValidMutationID(invalid) {
			t.Errorf("ValidMutationID(%q) = true", invalid)
		}
	}
}

func TestMutationResponseReplayEncoding(t *testing.T) {
	fresh, err := json.Marshal(MutationResponse{Replayed: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fresh), `"replayed":false`) {
		t.Fatalf("fresh create omitted replayed: %s", fresh)
	}
	update, err := json.Marshal(MutationResponse{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(update), "replayed") {
		t.Fatalf("update included replayed: %s", update)
	}
}

func TestDecodeStoredResultAcceptsUnwrappedLegacyBody(t *testing.T) {
	stored, err := decodeStoredResult([]byte(`{"error":"version conflict","currentVersion":4,"currentBlobKey":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != 409 {
		t.Fatalf("status = %d, want 409", stored.Status)
	}
}

// The pending claim is persisted, so its bytes must stay what marshaling the
// wrapped result produced.
func TestPendingResultEncoding(t *testing.T) {
	want, err := json.Marshal(storedResult{Status: 102, Body: json.RawMessage(`{"error":"mutation still in progress"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if pendingResult != string(want) {
		t.Fatalf("pendingResult = %s, want %s", pendingResult, want)
	}
}

func TestDecodeStoredResultAcceptsLegacyPending(t *testing.T) {
	stored, err := decodeStoredResult([]byte(`{"status":"pending"}`))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != 102 {
		t.Fatalf("status = %d, want 102", stored.Status)
	}
}

// A stored create result carries the integer fields as JSON strings and a
// null size and blob key for legacy rows. It must decode, replay with
// Replayed set, and marshal back to the same object bytes.
func TestStoredMutationResponseDecodes(t *testing.T) {
	const object = `{"id":"018f4b2b-22bb-7e4f-8a1b-4d5e6f7a8b9c","collection_id":"018f4b28-11aa-7d3e-9f0a-3c4d5e6f7a8b",` +
		`"version":"3","change_seq":"12","deleted":false,"blob_key":null,"size_bytes":null,` +
		`"created_at":"2026-01-02T03:04:05.006Z","updated_at":"2026-01-02T03:04:05.006Z"}`
	stored, err := decodeStoredResult([]byte(`{"status":201,"body":{"object":` + object +
		`,"collectionVersion":12,"replayed":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := stored.mutationResponse(true)
	if err != nil {
		t.Fatal(err)
	}
	o := response.Object
	if o.Version != 3 || o.ChangeSeq != 12 || o.SizeBytes != nil || o.BlobKey != nil || response.CollectionVersion != 12 {
		t.Fatalf("unexpected object: %#v", response)
	}
	if response.Replayed == nil || !*response.Replayed {
		t.Fatalf("replay did not set replayed: %#v", response.Replayed)
	}
	encoded, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != object {
		t.Fatalf("re-encoded object\n got %s\nwant %s", encoded, object)
	}

	sized := strings.Replace(strings.Replace(object, `"size_bytes":null`, `"size_bytes":"42"`, 1),
		`"blob_key":null`, `"blob_key":"k"`, 1)
	var withSize Object
	if err := json.Unmarshal([]byte(sized), &withSize); err != nil {
		t.Fatal(err)
	}
	if withSize.SizeBytes == nil || *withSize.SizeBytes != 42 || withSize.BlobKey == nil || *withSize.BlobKey != "k" {
		t.Fatalf("unexpected sized object: %#v", withSize)
	}
	if encoded, err := json.Marshal(withSize); err != nil || string(encoded) != sized {
		t.Fatalf("re-encoded sized object = %s, %v", encoded, err)
	}
}

func TestStoredResultReplaysFailures(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want error
	}{
		{"not found", `{"status":404,"body":{"error":"not found"}}`, ErrNotFound},
		{"blob not staged", `{"status":409,"body":{"error":"blob is not staged"}}`, ErrBlobNotStaged},
		{"pending", `{"status":102,"body":{"error":"mutation still in progress"}}`, ErrMutationPending},
		{"legacy pending", `{"status":"pending"}`, ErrMutationPending},
		{"legacy not found", `{"error":"not found"}`, ErrNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stored, err := decodeStoredResult([]byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stored.mutationResponse(false); !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want %v", err, test.want)
			}
		})
	}

	stored, err := decodeStoredResult([]byte(`{"error":"version conflict","currentVersion":4,"currentBlobKey":null}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = stored.deleteResponse()
	conflict, ok := errors.AsType[*ConflictError](err)
	if !ok || conflict.Conflict.CurrentVersion != 4 || conflict.Conflict.CurrentBlobKey != nil {
		t.Fatalf("err = %v, want version conflict at 4", err)
	}
}
