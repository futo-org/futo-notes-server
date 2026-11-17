package blobframe

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestAppendUploadBytes(t *testing.T) {
	got := AppendUpload(AppendUpload(nil, "a", []byte("hi")), "m-2", nil)
	want := []byte{
		0x00, 0x01, 'a', 0x00, 0x00, 0x00, 0x02, 'h', 'i',
		0x00, 0x03, 'm', '-', '2', 0x00, 0x00, 0x00, 0x00,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x\nwant % x", got, want)
	}
}

func TestParseUpload(t *testing.T) {
	body := AppendUpload(AppendUpload(AppendUpload(nil, "one", []byte("first")), "two", nil), "one", []byte("again"))
	frames, err := ParseUpload(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []UploadFrame{{"one", []byte("first")}, {"two", []byte{}}, {"one", []byte("again")}}
	if !slices.EqualFunc(frames, want, func(a, b UploadFrame) bool { return a.ID == b.ID && bytes.Equal(a.Blob, b.Blob) }) {
		t.Fatalf("frames = %q, want %q", frames, want)
	}
}

func TestParseUploadRejectsTheWholeRequest(t *testing.T) {
	valid := AppendUpload(nil, "ok", []byte("x"))
	var full []byte
	for range MaxUploadFrames {
		full = AppendUpload(full, "f", []byte("x"))
	}
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"empty body", nil, "batch must contain at least one frame"},
		{"one byte of id length", []byte{0}, "truncated id length"},
		{"id shorter than its length", []byte{0, 3, 'a'}, "truncated id"},
		{"empty id", AppendUpload(nil, "", []byte("x")), "invalid id"},
		{"id outside the Mutation-Id syntax", AppendUpload(nil, "a b", []byte("x")), "invalid id"},
		{"id over 128 characters", AppendUpload(nil, strings.Repeat("a", 129), []byte("x")), "invalid id"},
		{"blob length cut short", slices.Concat(valid, []byte{0, 1, 'b', 0, 0}), "truncated blob length"},
		{"blob shorter than its length", slices.Concat(valid, []byte{0, 1, 'b', 0, 0, 0, 3, 'x'}), "truncated blob"},
		{"too many frames", AppendUpload(full, "f", []byte("x")), "too many frames (max 200)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseUpload(tc.body); err == nil || err.Error() != tc.want {
				t.Fatalf("ParseUpload = %v, want %q", err, tc.want)
			}
		})
	}
	if frames, err := ParseUpload(full); err != nil || len(frames) != MaxUploadFrames {
		t.Fatalf("ParseUpload of %d frames = %d frames, %v", MaxUploadFrames, len(frames), err)
	}
	if frames, err := ParseUpload(AppendUpload(nil, strings.Repeat("a", 128), []byte("x"))); err != nil || len(frames) != 1 {
		t.Fatalf("ParseUpload with a 128-character id = %v, %v", frames, err)
	}
}
