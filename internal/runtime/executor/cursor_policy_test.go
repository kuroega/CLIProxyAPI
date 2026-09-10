package executor

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
	cursorconnect "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursor"
	"google.golang.org/protobuf/proto"
)

func TestCursorToolPolicy(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "sample.txt")
	if err := os.WriteFile(path, []byte("allowed content"), 0600); err != nil {
		t.Fatal(err)
	}
	read := &cursorproto.ExecServerMessage{Id: 1, Message: &cursorproto.ExecServerMessage_ReadArgs{ReadArgs: &cursorproto.ReadArgs{Path: path}}}
	shell := &cursorproto.ExecServerMessage{Id: 2, Message: &cursorproto.ExecServerMessage_ShellArgs{ShellArgs: &cursorproto.ShellArgs{Command: "echo allowed"}}}
	mcp := &cursorproto.ExecServerMessage{Id: 3, Message: &cursorproto.ExecServerMessage_McpStateExecArgs{McpStateExecArgs: &cursorproto.McpStateExecArgs{}}}
	for _, tc := range []struct {
		name    string
		tools   map[string]any
		request *cursorproto.ExecServerMessage
		denied  bool
	}{
		{"default denies files", nil, read, true},
		{"default denies shell", nil, shell, true},
		{"default denies MCP", nil, mcp, true},
		{"files need workspace", map[string]any{"files": true}, read, true},
		{"workspace alone grants nothing", map[string]any{"workspace": workspace}, read, true},
		{"relative workspace denied", map[string]any{"workspace": ".", "files": true}, read, true},
		{"file workspace denied", map[string]any{"workspace": path, "files": true}, read, true},
		{"explicit files allowed", map[string]any{"workspace": workspace, "files": true}, read, false},
		{"files do not enable shell", map[string]any{"workspace": workspace, "files": true}, shell, true},
		{"files do not enable MCP", map[string]any{"workspace": workspace, "files": true}, mcp, true},
		{"explicit shell allowed", map[string]any{"workspace": workspace, "shell": true}, shell, false},
		{"shell does not enable files", map[string]any{"workspace": workspace, "shell": true}, read, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"cursor-tools": tc.tools})
			if err != nil {
				t.Fatal(err)
			}
			var cfg config.Config
			if err := json.Unmarshal(payload, &cfg); err != nil {
				t.Fatal(err)
			}
			executor := NewCursorExecutor(&cfg)
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			errs := make(chan error, 1)
			go func() {
				errs <- executor.handleCursorExec(t.Context(), &cursorDuplexWriter{writer: writer}, tc.request, "owner", nil)
			}()
			frame, err := cursorconnect.ReadFrame(reader)
			if err != nil {
				t.Fatal(err)
			}
			var reply cursorproto.AgentClientMessage
			if err := proto.Unmarshal(frame.Payload, &reply); err != nil {
				t.Fatal(err)
			}
			denied := reply.GetExecClientControlMessage().GetThrow() != nil
			if denied != tc.denied {
				t.Fatalf("denied = %v, want %v; reply=%v", denied, tc.denied, &reply)
			}
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		})
	}
}
