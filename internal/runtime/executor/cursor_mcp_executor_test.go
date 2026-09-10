package executor

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
	cursorconnect "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursor"
)

func TestCursorMCPResourceDispatch(t *testing.T) {
	t.Setenv("CURSOR_MCP_EXECUTOR_FIXTURE", "1")
	executable, errExecutable := filepath.Abs(os.Args[0])
	if errExecutable != nil {
		t.Fatalf("resolve test executable: %v", errExecutable)
	}
	manager := cursorconnect.NewMCPStdioManager(map[string]config.CursorMCPServer{
		"fixture": {Enabled: true, Executable: executable, Args: []string{"-test.run=TestCursorMCPExecutorFixture"}, EnvAllowlist: []string{"CURSOR_MCP_EXECUTOR_FIXTURE"}},
	})
	defer func() { _ = manager.CloseAll() }()
	executor := &CursorExecutor{mcp: manager}
	state := executor.handleCursorMCPState(t.Context(), &cursorproto.McpStateExecArgs{})
	if state.GetSuccess() == nil || len(state.GetSuccess().GetServers()) != 1 || len(state.GetSuccess().GetServers()[0].GetTools()) != 1 {
		t.Fatalf("state = %#v", state)
	}
	server := "fixture"
	list := executor.handleCursorMCPResourceList(t.Context(), &cursorproto.ListMcpResourcesExecArgs{Server: &server})
	if list.GetSuccess() == nil || len(list.GetSuccess().GetResources()) != 1 || list.GetSuccess().GetResources()[0].GetUri() != "fixture://readme" {
		t.Fatalf("resource list = %#v", list)
	}
	read := executor.handleCursorMCPResourceRead(t.Context(), t.TempDir(), &cursorproto.ReadMcpResourceExecArgs{Server: "fixture", Uri: "fixture://readme"})
	if read.GetSuccess() == nil || read.GetSuccess().GetText() != "resource text" {
		t.Fatalf("resource read = %#v", read)
	}
	downloadPath := "downloaded/readme.txt"
	readDownload := executor.handleCursorMCPResourceRead(t.Context(), t.TempDir(), &cursorproto.ReadMcpResourceExecArgs{Server: "fixture", Uri: "fixture://readme", DownloadPath: &downloadPath})
	if readDownload.GetSuccess() == nil || readDownload.GetSuccess().GetDownloadPath() == "" {
		t.Fatalf("resource download = %#v", readDownload)
	}
	if data, errRead := os.ReadFile(readDownload.GetSuccess().GetDownloadPath()); errRead != nil || string(data) != "resource text" {
		t.Fatalf("downloaded resource = %q, %v", data, errRead)
	}
}

func TestCursorMCPExecutorFixture(t *testing.T) {
	if os.Getenv("CURSOR_MCP_EXECUTOR_FIXTURE") != "1" {
		return
	}
	input := bufio.NewScanner(os.Stdin)
	output := bufio.NewWriter(os.Stdout)
	defer output.Flush()
	for input.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      uint64          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(input.Bytes(), &request) != nil || request.ID == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "echo", "description": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		case "resources/list":
			result = map[string]any{"resources": []map[string]any{{"uri": "fixture://readme", "name": "Readme", "mimeType": "text/plain"}}}
		case "resources/read":
			result = map[string]any{"contents": []map[string]any{{"uri": "fixture://readme", "mimeType": "text/plain", "text": "resource text"}}}
		default:
			result = map[string]any{}
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		_, _ = output.Write(append(response, '\n'))
		_ = output.Flush()
	}
}
