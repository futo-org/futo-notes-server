package syncapi

import (
	"encoding/json"
	"testing"

	"futo-notes-server/internal/objects"
)

// The typed responses replaced map[string]any values; their bytes must match
// what the sorted-key maps produced.
func TestResponseStructsMatchMapEncoding(t *testing.T) {
	o := objects.Object{ID: "o", CollectionID: "c", Version: 2, ChangeSeq: 7}
	tests := []struct {
		name   string
		typed  any
		mapped map[string]any
	}{
		{"list", listObjectsResponse{Objects: []objects.Object{o}}, map[string]any{"objects": []objects.Object{o}}},
		{"limited list", listObjectsResponse{HasMore: new(false), NextCursor: new(int64(7)), Objects: []objects.Object{}},
			map[string]any{"objects": []objects.Object{}, "hasMore": false, "nextCursor": int64(7)}},
		{"get", objectResponse{Object: o}, map[string]any{"object": o}},
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
