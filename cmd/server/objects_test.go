package main

import (
	"encoding/binary"
	"testing"
)

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
	entries, message := parseBatch(append(create, update...))
	if message != "" {
		t.Fatal(message)
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
			_, got := parseBatch(test.body)
			if got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
