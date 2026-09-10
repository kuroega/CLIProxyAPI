package executor

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	cursorconnect "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursor"
)

func TestCursorDuplexCloseDoesNotRequireReader(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	duplex := &cursorDuplexWriter{writer: writer}
	done := make(chan struct{})
	go func() {
		duplex.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked waiting for upstream to read")
	}
}

func TestConsumeCursorStreamTerminalStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		terminal  string
		wantError bool
	}{
		{name: "success", terminal: `{}`},
		{name: "upstream error", terminal: `{"error":{"code":"resource_exhausted","message":"quota exceeded"}}`, wantError: true},
		{name: "malformed terminal", terminal: `{`, wantError: true},
		{name: "unexpected EOF", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			if tc.terminal != "" {
				if err := cursorconnect.WriteEndStream(&wire, []byte(tc.terminal)); err != nil {
					t.Fatal(err)
				}
			}
			ctx := t.Context()
			reader, writer := io.Pipe()
			defer reader.Close()
			go func() { _, _ = io.Copy(io.Discard, reader) }()
			events := make(chan cursorEvent, 1)
			(&CursorExecutor{}).consumeCursorStream(ctx, &http.Response{Body: io.NopCloser(&wire)}, &cursorDuplexWriter{writer: writer}, nil, "", nil, events, nil)
			var gotError bool
			for event := range events {
				gotError = gotError || event.err != nil
			}
			if gotError != tc.wantError {
				t.Fatalf("error received = %v, want %v", gotError, tc.wantError)
			}
		})
	}
}
