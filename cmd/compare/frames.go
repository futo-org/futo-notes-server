package main

import (
	"encoding/binary"
	"fmt"
)

type downloadFrame struct {
	Key    string
	Status byte
	Blob   []byte
}

func decodeDownloadFrames(body []byte) ([]downloadFrame, error) {
	frames := []downloadFrame{}
	for offset := 0; offset < len(body); {
		if len(body)-offset < 2 {
			return nil, fmt.Errorf("truncated key length at %d", offset)
		}
		keyLen := int(binary.BigEndian.Uint16(body[offset : offset+2]))
		offset += 2
		if len(body)-offset < keyLen+5 {
			return nil, fmt.Errorf("truncated frame at %d", offset)
		}
		key := string(body[offset : offset+keyLen])
		offset += keyLen
		status := body[offset]
		offset++
		blobLen := int(binary.BigEndian.Uint32(body[offset : offset+4]))
		offset += 4
		if len(body)-offset < blobLen {
			return nil, fmt.Errorf("truncated blob at %d", offset)
		}
		blob := append([]byte(nil), body[offset:offset+blobLen]...)
		offset += blobLen
		frames = append(frames, downloadFrame{Key: key, Status: status, Blob: blob})
	}
	return frames, nil
}

func normalizeFrames(frames []downloadFrame, target *target) []downloadFrame {
	result := make([]downloadFrame, len(frames))
	for index, frame := range frames {
		result[index] = frame
		result[index].Key = normalizeIdentityString(frame.Key, target)
	}
	return result
}

func summarizeFrames(frames []downloadFrame) []string {
	result := make([]string, len(frames))
	for index, frame := range frames {
		result[index] = fmt.Sprintf("%s(status=%d,bytes=%d)", frame.Key, frame.Status, len(frame.Blob))
	}
	return result
}

func batchEntryFrame(operation byte, identifier string, version uint32, blob []byte) []byte {
	frame := []byte{operation}
	frame = binary.BigEndian.AppendUint16(frame, uint16(len(identifier)))
	frame = append(frame, identifier...)
	frame = binary.BigEndian.AppendUint32(frame, version)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(blob)))
	return append(frame, blob...)
}
