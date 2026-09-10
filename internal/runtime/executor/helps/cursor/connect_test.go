package cursor

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

type fragmentedReader struct {
	data []byte
}

func (r *fragmentedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestCursorConnectFrameRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	if errWrite := WriteFrame(&wire, []byte("protobuf")); errWrite != nil {
		t.Fatalf("WriteFrame() error = %v", errWrite)
	}
	if errWrite := WriteEndStream(&wire, []byte(`{"code":"ok"}`)); errWrite != nil {
		t.Fatalf("WriteEndStream() error = %v", errWrite)
	}

	reader := &fragmentedReader{data: wire.Bytes()}
	frame, errRead := ReadFrame(reader)
	if errRead != nil {
		t.Fatalf("ReadFrame() error = %v", errRead)
	}
	if frame.EndStream || string(frame.Payload) != "protobuf" {
		t.Fatalf("first frame = %#v", frame)
	}
	frame, errRead = ReadFrame(reader)
	if errRead != nil {
		t.Fatalf("ReadFrame() error = %v", errRead)
	}
	if !frame.EndStream || string(frame.Payload) != `{"code":"ok"}` {
		t.Fatalf("end-stream frame = %#v", frame)
	}
}

func TestCursorConnectFrameRejectsCompressedAndOversized(t *testing.T) {
	for name, header := range map[string][5]byte{
		"compressed": {compressedFlag, 0, 0, 0, 0},
		"unknown":    {0x80, 0, 0, 0, 0},
		"oversized":  oversizedHeader(),
	} {
		t.Run(name, func(t *testing.T) {
			_, errRead := ReadFrame(bytes.NewReader(header[:]))
			if errRead == nil {
				t.Fatal("ReadFrame() error = nil")
			}
		})
	}
	if errWrite := WriteFrame(io.Discard, make([]byte, MaxFrameSize+1)); errWrite == nil || !strings.Contains(errWrite.Error(), "exceeds") {
		t.Fatalf("WriteFrame() error = %v, want limit error", errWrite)
	}
}

func oversizedHeader() [5]byte {
	var header [5]byte
	binary.BigEndian.PutUint32(header[1:], MaxFrameSize+1)
	return header
}
