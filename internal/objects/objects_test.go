package objects

import (
	"encoding/json"
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

func TestValidBlobKey(t *testing.T) {
	userID := "018f4b28-11aa-7d3e-9f0a-3c4d5e6f7a8b"
	blobID := "018f4b2b-22bb-7e4f-8a1b-4d5e6f7a8b9c"
	if !ValidBlobKey(userID, userID+"/"+blobID) {
		t.Fatal("expected scoped UUID blob key to be valid")
	}
	if ValidBlobKey("short", strings.Repeat("x", 73)) {
		t.Fatal("short user ID must not validate or panic")
	}
	if ValidBlobKey(userID, blobID+"/"+blobID) {
		t.Fatal("key scoped to another user must be invalid")
	}
}

func TestMutationResponseReplayEncoding(t *testing.T) {
	falseValue := false
	fresh, err := json.Marshal(MutationResponse{Replayed: &falseValue})
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

func TestDecodeStoredResultAcceptsLegacyPending(t *testing.T) {
	stored, err := decodeStoredResult([]byte(`{"status":"pending"}`))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != 102 {
		t.Fatalf("status = %d, want 102", stored.Status)
	}
}
