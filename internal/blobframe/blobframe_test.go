package blobframe

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWriteHeaderBytes(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		status  Status
		blobLen uint32
		want    []byte
	}{
		{"ok", "ab", OK, 3, []byte{0x00, 0x02, 'a', 'b', 0x00, 0x00, 0x00, 0x00, 0x03}},
		{"missing", "k", Missing, 0, []byte{0x00, 0x01, 'k', 0x01, 0x00, 0x00, 0x00, 0x00}},
		{"omitted", "k", Omitted, 0, []byte{0x00, 0x01, 'k', 0x02, 0x00, 0x00, 0x00, 0x00}},
		{"big-endian lengths", strings.Repeat("x", 0x0102), OK, 0x01020304,
			append(append([]byte{0x01, 0x02}, strings.Repeat("x", 0x0102)...), 0x00, 0x01, 0x02, 0x03, 0x04)},
		{"utf-8 key counts bytes", "é", Missing, 0, []byte{0x00, 0x02, 0xc3, 0xa9, 0x01, 0x00, 0x00, 0x00, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteHeader(&buf, tc.key, tc.status, tc.blobLen); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), tc.want) {
				t.Fatalf("got % x\nwant % x", buf.Bytes(), tc.want)
			}
		})
	}
}

func TestEncoderStreamBytes(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf, MaxBatchBytes)
	if err := enc.Missing("m"); err != nil {
		t.Fatal(err)
	}
	if sent, err := enc.Blob("b", 2, strings.NewReader("hi")); err != nil || !sent {
		t.Fatalf("Blob = %v, %v", sent, err)
	}
	want := []byte{
		0x00, 0x01, 'm', 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 'b', 0x00, 0x00, 0x00, 0x00, 0x02, 'h', 'i',
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got % x\nwant % x", buf.Bytes(), want)
	}
}

func TestEncoderFirstBlobShipsOverTheCap(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf, 10)
	if err := enc.Missing("gone"); err != nil {
		t.Fatal(err)
	}
	if sent, err := enc.Blob("a", 20, bytes.NewReader(make([]byte, 20))); err != nil || !sent {
		t.Fatalf("first blob: sent=%v err=%v", sent, err)
	}
	if sent, err := enc.Blob("b", 1, strings.NewReader("x")); err != nil || sent {
		t.Fatalf("second blob: sent=%v err=%v, want omitted", sent, err)
	}
	frames := readAll(t, &buf)
	if len(frames) != 3 || frames[0].Status != Missing || frames[1].Status != OK || frames[2].Status != Omitted {
		t.Fatalf("frames = %+v", frames)
	}
	if len(frames[1].Blob) != 20 {
		t.Fatalf("first blob carried %d bytes", len(frames[1].Blob))
	}
}

// The cap counts every frame's fixed bytes and key bytes, not only blobs.
func TestEncoderCapArithmetic(t *testing.T) {
	// a: 7+1+4 = 12 bytes; b: 7+1+3 = 11 bytes fills 23 exactly; c needs 9.
	var buf bytes.Buffer
	enc := NewEncoder(&buf, 23)
	for _, step := range []struct {
		key  string
		blob string
		sent bool
	}{
		{"a", "1234", true},
		{"b", "123", true},
		{"c", "1", false},
		{"d", "", false},
	} {
		sent, err := enc.Blob(step.key, int64(len(step.blob)), strings.NewReader(step.blob))
		if err != nil || sent != step.sent {
			t.Fatalf("%s: sent=%v err=%v, want sent=%v", step.key, sent, err, step.sent)
		}
	}
	if got := buf.Len(); got != 12+11+8+8 {
		t.Fatalf("payload %d bytes", got)
	}
}

func TestEncoderLaterSmallBlobFillsRemainingRoom(t *testing.T) {
	var buf bytes.Buffer
	// a takes 13 bytes and omitted b 8, leaving 19 for c's 13.
	enc := NewEncoder(&buf, 40)
	for _, step := range []struct {
		key  string
		size int
		sent bool
	}{
		{"a", 5, true},
		{"b", 50, false},
		{"c", 5, true},
	} {
		sent, err := enc.Blob(step.key, int64(step.size), bytes.NewReader(make([]byte, step.size)))
		if err != nil || sent != step.sent {
			t.Fatalf("%s: sent=%v err=%v, want %v", step.key, sent, err, step.sent)
		}
	}
}

func TestEncoderReportsOversizeBlobMissing(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf, MaxBatchBytes)
	sent, err := enc.Blob("huge", MaxBlobBytes+1, failingReader{})
	if err != nil || sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	frames := readAll(t, &buf)
	if len(frames) != 1 || frames[0].Status != Missing {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestEncoderFailsOnShortBody(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf, MaxBatchBytes)
	if _, err := enc.Blob("k", 10, strings.NewReader("short")); err == nil {
		t.Fatal("a body shorter than its declared size must fail")
	}
}

func TestReadFrameRejectsMalformedStreams(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0x00})); !errors.Is(err, ErrTruncated) {
		t.Fatalf("half a key length: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0x00, 0x01, 'k', 0x00, 0x00, 0x00, 0x00, 0x02, 'x'})); !errors.Is(err, ErrTruncated) {
		t.Fatalf("short blob: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0x00, 0x01, 'k', 0x01, 0x00, 0x00, 0x00, 0x01})); err == nil {
		t.Fatal("a missing frame with a blob length must fail")
	}
}

func readAll(t *testing.T, r io.Reader) []Frame {
	t.Helper()
	var frames []Frame
	for {
		f, err := ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, f)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("must not be read") }
