package blobframe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
)

// MaxUploadFrames caps the frames of one batch upload.
const MaxUploadFrames = 200

// uploadIDPattern is the batch-safe Mutation-Id syntax, so a client can name
// a frame with the Mutation-Id of the create that will claim its receipt.
var uploadIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// UploadFrame is one frame of a batch upload request: the client's id for
// it, echoed in its result, and the blob to store.
type UploadFrame struct {
	ID   string
	Blob []byte
}

// ParseUpload decodes a batch upload request body (hosted storage
// POST /blobs/batch-upload): 1 to
// MaxUploadFrames frames, repeating to EOF, integers big-endian:
//
//	[u16 idLen][id utf8][u32 blobLen][blob bytes]
//
// An empty blob parses; the caller answers it as that frame's failure. Every
// error is a 400 for the whole request, and its message is the one the
// response carries.
func ParseUpload(body []byte) ([]UploadFrame, error) {
	var frames []UploadFrame
	for offset := 0; offset < len(body); {
		if len(frames) == MaxUploadFrames {
			return nil, fmt.Errorf("too many frames (max %d)", MaxUploadFrames)
		}
		if len(body)-offset < 2 {
			return nil, errors.New("truncated id length")
		}
		idLen := int(binary.BigEndian.Uint16(body[offset:]))
		offset += 2
		if len(body)-offset < idLen {
			return nil, errors.New("truncated id")
		}
		id := string(body[offset : offset+idLen])
		offset += idLen
		if !uploadIDPattern.MatchString(id) {
			return nil, errors.New("invalid id")
		}
		if len(body)-offset < 4 {
			return nil, errors.New("truncated blob length")
		}
		blobLen := int(binary.BigEndian.Uint32(body[offset:]))
		offset += 4
		if blobLen > len(body)-offset {
			return nil, errors.New("truncated blob")
		}
		frames = append(frames, UploadFrame{ID: id, Blob: body[offset : offset+blobLen]})
		offset += blobLen
	}
	if len(frames) == 0 {
		return nil, errors.New("batch must contain at least one frame")
	}
	return frames, nil
}

// AppendUpload appends one batch upload frame to buf, as a client encodes
// it.
func AppendUpload(buf []byte, id string, blob []byte) []byte {
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(id)))
	buf = append(buf, id...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(blob)))
	return append(buf, blob...)
}
