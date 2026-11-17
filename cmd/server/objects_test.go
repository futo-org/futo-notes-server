package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"

	"futo-notes-server/internal/objects"
)

// The typed responses replaced map[string]any values; their bytes must match
// what the sorted-key maps produced.
func TestResponseStructsMatchMapEncoding(t *testing.T) {
	o := objects.Object{ID: "o", CollectionID: "c", Version: 2, ChangeSeq: 7}
	failure := func(err error) any {
		result, _ := batchFailure(err)
		return result
	}
	tests := []struct {
		name   string
		typed  any
		mapped map[string]any
	}{
		{"list", listObjectsResponse{Objects: []objects.Object{o}}, map[string]any{"objects": []objects.Object{o}}},
		{"limited list", listObjectsResponse{HasMore: new(false), NextCursor: new(int64(7)), Objects: []objects.Object{}},
			map[string]any{"objects": []objects.Object{}, "hasMore": false, "nextCursor": int64(7)}},
		{"get", objectResponse{Object: o}, map[string]any{"object": o}},
		{"upload", uploadBlobResponse{Key: "k"}, map[string]any{"key": "k"}},
		{"batch object", batchResult(opCreate, objects.MutationResponse{Object: o, CollectionVersion: 7, Replayed: new(true)}),
			map[string]any{"status": "replayed", "object": o, "collectionVersion": int64(7)}},
		{"batch conflict", failure(&objects.ConflictError{Conflict: objects.Conflict{Error: "version conflict", CurrentVersion: 2}}),
			map[string]any{"status": "conflict", "currentVersion": int64(2), "currentBlobKey": (*string)(nil)}},
		{"batch not found", failure(objects.ErrNotFound), map[string]any{"status": "not_found"}},
		{"batch mismatch", failure(objects.ErrMutationMismatch),
			map[string]any{"status": "error", "error": "mutation id reused for different intent"}},
		{"batch pending", failure(objects.ErrMutationPending),
			map[string]any{"status": "error", "error": "mutation still in progress"}},
		{"batch internal", failure(errors.New("boom")), map[string]any{"status": "error", "error": "internal server error"}},
		{"batch too large", batchStatusResult{Status: "too_large"}, map[string]any{"status": "too_large"}},
		{"batch", batchResponse{Results: []any{batchStatusResult{Status: "too_large"}}},
			map[string]any{"results": []any{map[string]any{"status": "too_large"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := json.Marshal(test.typed)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(test.mapped)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("got  %s\nwant %s", got, want)
			}
		})
	}
}

func batchFrame(operation byte, identifier string, version uint32, blob []byte) []byte {
	frame := make([]byte, 1+2+len(identifier)+4+4+len(blob))
	frame[0] = operation
	binary.BigEndian.PutUint16(frame[1:3], uint16(len(identifier)))
	offset := 3
	copy(frame[offset:], identifier)
	offset += len(identifier)
	binary.BigEndian.PutUint32(frame[offset:offset+4], version)
	offset += 4
	binary.BigEndian.PutUint32(frame[offset:offset+4], uint32(len(blob)))
	offset += 4
	copy(frame[offset:], blob)
	return frame
}

func TestParseBatch(t *testing.T) {
	create := batchFrame(0, "mutation-1", 0, []byte("create"))
	update := batchFrame(1, "018f4b2b-22bb-7e4f-8a1b-4d5e6f7a8b9c", 2, []byte("update"))
	entries, err := parseBatch(append(create, update...))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Identifier != "mutation-1" || entries[1].Version != 2 {
		t.Fatalf("unexpected entries: %#v", entries)
	}
}

func TestParseBatchErrors(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{"empty", nil, "batch must contain at least one entry"},
		{"operation", []byte{2}, "invalid operation"},
		{"identifier length", []byte{0, 0}, "truncated object id length"},
		{"create version", batchFrame(0, "mutation-1", 1, []byte("x")), "create version must be zero"},
		{"empty blob", batchFrame(0, "mutation-1", 0, nil), "blob must not be empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseBatch(test.body)
			if err == nil || err.Error() != test.want {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}
