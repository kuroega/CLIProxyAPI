// Package cursor contains Cursor's native Connect protocol helpers.
package cursor

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// MaxFrameSize bounds untrusted Connect frame payload allocation at 64 MiB.
	MaxFrameSize = 64 << 20

	compressedFlag = 0x01
	endStreamFlag  = 0x02
)

// Frame is a single Connect protocol frame. EndStream carries the terminal JSON
// metadata rather than a protobuf payload.
type Frame struct {
	Payload   []byte
	EndStream bool
}

// WriteFrame writes a protobuf Connect frame. Callers write end-stream metadata
// explicitly with WriteEndStream.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("cursor connect frame exceeds %d byte limit", MaxFrameSize)
	}
	return writeFrame(w, 0, payload)
}

// WriteEndStream writes a terminal Connect JSON frame.
func WriteEndStream(w io.Writer, metadata []byte) error {
	if len(metadata) > MaxFrameSize {
		return fmt.Errorf("cursor connect end-stream frame exceeds %d byte limit", MaxFrameSize)
	}
	return writeFrame(w, endStreamFlag, metadata)
}

func writeFrame(w io.Writer, flags byte, payload []byte) error {
	var header [5]byte
	header[0] = flags
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, errWrite := w.Write(header[:]); errWrite != nil {
		return fmt.Errorf("write cursor connect frame header: %w", errWrite)
	}
	if _, errWrite := w.Write(payload); errWrite != nil {
		return fmt.Errorf("write cursor connect frame payload: %w", errWrite)
	}
	return nil
}

// EndStreamError decodes terminal metadata without treating upstream failures as success.
func EndStreamError(payload []byte) error {
	var terminal struct {
		Error *struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Details []json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &terminal); err != nil {
		return fmt.Errorf("cursor connect: decode end-stream metadata: %w", err)
	}
	if terminal.Error != nil {
		message := fmt.Sprintf("cursor connect: %s: %s", terminal.Error.Code, terminal.Error.Message)
		if len(terminal.Error.Details) > 0 {
			details, errMarshal := json.Marshal(terminal.Error.Details)
			if errMarshal == nil {
				const maxDetails = 400
				if len(details) > maxDetails {
					details = append(details[:maxDetails], '.', '.', '.')
				}
				message += " [details: " + string(details) + "]"
			}
		}
		return errors.New(message)
	}
	return nil
}

// ReadFrame reads one complete frame, tolerating arbitrary transport
// fragmentation. Compression is intentionally rejected because Cursor's native
// transport does not negotiate it and decoding compressed untrusted payloads
// here would undermine the frame limit.
func ReadFrame(r io.Reader) (Frame, error) {
	var header [5]byte
	if _, errRead := io.ReadFull(r, header[:]); errRead != nil {
		return Frame{}, errRead
	}
	flags := header[0]
	if flags&^(compressedFlag|endStreamFlag) != 0 {
		return Frame{}, fmt.Errorf("cursor connect frame has unknown flags 0x%02x", flags)
	}
	if flags&compressedFlag != 0 {
		return Frame{}, fmt.Errorf("cursor connect compressed frames are unsupported")
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > MaxFrameSize {
		return Frame{}, fmt.Errorf("cursor connect frame length %d exceeds %d byte limit", length, MaxFrameSize)
	}
	payload := make([]byte, int(length))
	if _, errRead := io.ReadFull(r, payload); errRead != nil {
		return Frame{}, fmt.Errorf("read cursor connect frame payload: %w", errRead)
	}
	return Frame{Payload: payload, EndStream: flags&endStreamFlag != 0}, nil
}
