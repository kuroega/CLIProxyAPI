package cursor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type mcpReadSignal struct {
	io.Reader
	started chan struct{}
	once    sync.Once
}

func (r *mcpReadSignal) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.Reader.Read(p)
}

func TestMCPShutdownInterruptsBlockedCall(t *testing.T) {
	t.Setenv("CURSOR_MCP_FIXTURE", "1")
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	manager := NewMCPStdioManager(map[string]config.CursorMCPServer{"fixture": {Enabled: true, Executable: executable, Args: []string{"-test.run=TestCursorMCPFixtureProcess"}, EnvAllowlist: []string{"CURSOR_MCP_FIXTURE"}}})
	defer manager.CloseAll()
	if _, err := manager.Call(t.Context(), "fixture", "tools/list", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	session := manager.sessions["fixture"]
	started := make(chan struct{})
	session.stdout = bufio.NewReader(&mcpReadSignal{Reader: session.stdout, started: started})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := make(chan error, 1)
	go func() { _, err := manager.Call(ctx, "fixture", "blocked", nil); called <- err }()
	<-started
	closed := make(chan error, 1)
	go func() { closed <- manager.CloseAll() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-called
		<-closed
		t.Fatal("shutdown waited for a blocked MCP call")
	}
	if err := <-called; err == nil {
		t.Fatal("blocked call succeeded during shutdown")
	}
	if _, err := manager.Call(t.Context(), "fixture", "tools/list", nil); err == nil {
		t.Fatal("closed manager accepted a new call")
	}
}

func TestMCPStdioManagerCallsConfiguredServer(t *testing.T) {
	t.Setenv("CURSOR_MCP_FIXTURE", "1")
	executable, errExecutable := filepath.Abs(os.Args[0])
	if errExecutable != nil {
		t.Fatalf("resolve test executable: %v", errExecutable)
	}
	manager := NewMCPStdioManager(map[string]config.CursorMCPServer{
		"fixture": {Enabled: true, Executable: executable, Args: []string{"-test.run=TestCursorMCPFixtureProcess"}, EnvAllowlist: []string{"CURSOR_MCP_FIXTURE"}},
	})
	defer func() {
		if errClose := manager.CloseAll(); errClose != nil {
			t.Errorf("CloseAll() error = %v", errClose)
		}
	}()

	tools, errTools := manager.Call(t.Context(), "fixture", "tools/list", map[string]any{})
	if errTools != nil {
		t.Fatalf("tools/list error = %v", errTools)
	}
	if string(tools) != `{"tools":[{"name":"echo","description":"echo text","inputSchema":{"type":"object"}}]}` {
		t.Fatalf("tools/list = %s", tools)
	}
	call, errCall := manager.Call(t.Context(), "fixture", "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "hello"}})
	if errCall != nil {
		t.Fatalf("tools/call error = %v", errCall)
	}
	if string(call) != `{"content":[{"text":"hello","type":"text"}]}` {
		t.Fatalf("tools/call = %s", call)
	}
	resources, errResources := manager.Call(t.Context(), "fixture", "resources/list", map[string]any{})
	if errResources != nil {
		t.Fatalf("resources/list error = %v", errResources)
	}
	if string(resources) != `{"resources":[{"uri":"fixture://readme","name":"Readme","mimeType":"text/plain"}]}` {
		t.Fatalf("resources/list = %s", resources)
	}
	resource, errResource := manager.Call(t.Context(), "fixture", "resources/read", map[string]any{"uri": "fixture://readme"})
	if errResource != nil {
		t.Fatalf("resources/read error = %v", errResource)
	}
	if string(resource) != `{"contents":[{"uri":"fixture://readme","mimeType":"text/plain","text":"resource text"}]}` {
		t.Fatalf("resources/read = %s", resource)
	}
}

func TestMCPStdioManagerCancellationClosesSession(t *testing.T) {
	t.Setenv("CURSOR_MCP_FIXTURE", "1")
	executable, errExecutable := filepath.Abs(os.Args[0])
	if errExecutable != nil {
		t.Fatalf("resolve test executable: %v", errExecutable)
	}
	manager := NewMCPStdioManager(map[string]config.CursorMCPServer{
		"fixture": {Enabled: true, Executable: executable, Args: []string{"-test.run=TestCursorMCPFixtureProcess"}, EnvAllowlist: []string{"CURSOR_MCP_FIXTURE"}},
	})
	defer manager.CloseAll()
	if _, errCall := manager.Call(t.Context(), "fixture", "tools/list", map[string]any{}); errCall != nil {
		t.Fatalf("initial tools/list error = %v", errCall)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, errCall := manager.Call(ctx, "fixture", "blocked", map[string]any{})
		result <- errCall
	}()
	cancel()
	select {
	case errCall := <-result:
		if !errors.Is(errCall, context.Canceled) {
			t.Fatalf("canceled call error = %v", errCall)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled MCP call did not return")
	}
}

func TestMCPStdioManagerListsEnabledServers(t *testing.T) {
	manager := NewMCPStdioManager(map[string]config.CursorMCPServer{
		"zeta":  {Enabled: true},
		"alpha": {Enabled: true},
		"off":   {Enabled: false},
	})
	ids := manager.ServerIDs()
	if fmt.Sprint(ids) != "[alpha zeta]" {
		t.Fatalf("ServerIDs() = %v", ids)
	}
}

func TestCursorMCPFixtureProcess(t *testing.T) {
	if os.Getenv("CURSOR_MCP_FIXTURE") != "1" {
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
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = json.RawMessage(`{"tools":[{"name":"echo","description":"echo text","inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			var params struct {
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(request.Params, &params)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": params.Arguments["text"]}}}
		case "resources/list":
			result = json.RawMessage(`{"resources":[{"uri":"fixture://readme","name":"Readme","mimeType":"text/plain"}]}`)
		case "resources/read":
			result = json.RawMessage(`{"contents":[{"uri":"fixture://readme","mimeType":"text/plain","text":"resource text"}]}`)
		case "blocked":
			continue
		default:
			result = map[string]any{}
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		_, _ = output.Write(append(response, '\n'))
		_ = output.Flush()
	}
}
