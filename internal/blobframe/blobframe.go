// Package blobframe encodes the binary response of a batch blob download
// (self-hosted POST /api/blobs/batch, hosted storage POST /blobs/batch).
//
// One frame per requested key, in request order, integers big-endian:
//
//	[u16 keyLen][key utf8][u8 status][u32 blobLen][blob bytes]
//
// Missing and omitted frames carry blobLen 0 and no blob bytes.
package blobframe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Request limits and the response payload cap shared by every batch endpoint.
const (
	// MaxRequestBytes caps the JSON request body.
	MaxRequestBytes = 64 << 10
	// MaxKeys caps the number of keys in one request.
	MaxKeys = 200
	// MaxKeyChars caps each key's length in characters (runes).
	MaxKeyChars = 128
	// MaxBatchBytes caps the encoded response, counting every frame's fixed
	// bytes, key bytes and blob bytes.
	MaxBatchBytes = 32 << 20
	// MaxBlobBytes is the largest blob a frame may carry: the largest blob a
	// single upload can create.
	MaxBlobBytes = 100 << 20
)

// Overhead is the fixed size of a frame: u16 key length, u8 status and u32
// blob length.
const Overhead = 7

// Status is the status byte of a frame.
type Status byte

const (
	// OK frames carry blobLen bytes of blob.
	OK Status = 0
	// Missing frames report a key that is absent, malformed, not the caller's,
	// or too large to describe.
	Missing Status = 1
	// Omitted frames report a blob left out to keep the response under
	// MaxBatchBytes; the client re-requests it in a fresh batch.
	Omitted Status = 2
)

// WriteHeader writes a frame's header: everything but the blob bytes.
func WriteHeader(w io.Writer, key string, status Status, blobLen uint32) error {
	header := make([]byte, 0, Overhead+len(key))
	header = binary.BigEndian.AppendUint16(header, uint16(len(key)))
	header = append(header, key...)
	header = append(header, byte(status))
	header = binary.BigEndian.AppendUint32(header, blobLen)
	_, err := w.Write(header)
	return err
}

// Encoder writes a batch response and keeps it under its payload cap. The
// first blob available always ships, so a client whose next blob is larger
// than the whole cap still makes progress.
type Encoder struct {
	w       io.Writer
	limit   int
	written int
	shipped bool
}

// NewEncoder returns an Encoder writing to w with a payload cap of limit
// bytes (MaxBatchBytes for the real endpoints).
func NewEncoder(w io.Writer, limit int) *Encoder {
	return &Encoder{w: w, limit: limit}
}

// Missing writes a Missing frame for key.
func (e *Encoder) Missing(key string) error {
	return e.header(key, Missing, 0)
}

// Blob writes key's frame for a blob of size bytes read from body. A blob over
// MaxBlobBytes is reported Missing, and one whose frame would push the payload
// past the cap is reported Omitted unless nothing has shipped yet. It reports
// whether the blob itself was sent. A body that ends before size bytes is an
// error: the declared length is already on the wire, so the caller must abort
// the response rather than continue a desynchronized stream.
func (e *Encoder) Blob(key string, size int64, body io.Reader) (bool, error) {
	switch {
	case size < 0 || size > MaxBlobBytes:
		return false, e.Missing(key)
	case e.shipped && Overhead+len(key)+int(size) > e.limit-e.written:
		return false, e.header(key, Omitted, 0)
	}
	if err := e.header(key, OK, uint32(size)); err != nil {
		return false, err
	}
	copied, err := io.CopyN(e.w, body, size)
	if err != nil {
		return false, fmt.Errorf("blob %s: sent %d of %d bytes: %w", key, copied, size, err)
	}
	e.written += int(size)
	e.shipped = true
	return true, nil
}

func (e *Encoder) header(key string, status Status, blobLen uint32) error {
	if err := WriteHeader(e.w, key, status, blobLen); err != nil {
		return err
	}
	e.written += Overhead + len(key)
	return nil
}

// Frame is one decoded frame.
type Frame struct {
	Key    string
	Status Status
	Blob   []byte
}

// ErrTruncated reports a stream that ends inside a frame.
var ErrTruncated = errors.New("truncated frame")

// ReadFrame decodes the next frame from r. It returns io.EOF, unwrapped, when
// r ends cleanly between frames.
func ReadFrame(r io.Reader) (Frame, error) {
	var keyLen [2]byte
	if _, err := io.ReadFull(r, keyLen[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, io.EOF
		}
		return Frame{}, ErrTruncated
	}
	rest := make([]byte, int(binary.BigEndian.Uint16(keyLen[:]))+5)
	if _, err := io.ReadFull(r, rest); err != nil {
		return Frame{}, ErrTruncated
	}
	n := len(rest) - 5
	f := Frame{Key: string(rest[:n]), Status: Status(rest[n])}
	blobLen := binary.BigEndian.Uint32(rest[n+1:])
	if f.Status != OK {
		if blobLen != 0 {
			return Frame{}, fmt.Errorf("status %d frame with blob length %d", f.Status, blobLen)
		}
		return f, nil
	}
	if blobLen > MaxBlobBytes {
		return Frame{}, fmt.Errorf("blob length %d over %d", blobLen, MaxBlobBytes)
	}
	f.Blob = make([]byte, blobLen)
	if _, err := io.ReadFull(r, f.Blob); err != nil {
		return Frame{}, ErrTruncated
	}
	return f, nil
}
