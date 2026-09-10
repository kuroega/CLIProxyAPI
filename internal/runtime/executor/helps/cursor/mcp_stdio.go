package cursor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const maxMCPMessageBytes = 1 << 20

// MCPStdioManager owns configured local stdio MCP sessions.
type MCPStdioManager struct {
	mu       sync.Mutex
	servers  map[string]config.CursorMCPServer
	sessions map[string]*mcpStdioSession
	closed   bool
}

type mcpStdioSession struct {
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	server     config.CursorMCPServer
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	controller shellProcessController
	nextID     uint64
	closed     bool
}

// NewMCPStdioManager creates a lazy session manager for configured servers.
func NewMCPStdioManager(servers map[string]config.CursorMCPServer) *MCPStdioManager {
	copyServers := make(map[string]config.CursorMCPServer, len(servers))
	for id, server := range servers {
		copyServers[id] = config.CursorMCPServer{Enabled: server.Enabled, Executable: server.Executable, WorkingDir: server.WorkingDir, Args: append([]string(nil), server.Args...), EnvAllowlist: append([]string(nil), server.EnvAllowlist...)}
	}
	return &MCPStdioManager{servers: copyServers, sessions: make(map[string]*mcpStdioSession)}
}

// Call sends one JSON-RPC request to a configured local MCP server.
func (m *MCPStdioManager) Call(ctx context.Context, serverID, method string, params any) (json.RawMessage, error) {
	if m == nil {
		return nil, fmt.Errorf("cursor MCP manager is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("cursor MCP manager is closed")
	}
	session := m.sessions[serverID]
	if session == nil {
		server, exists := m.servers[serverID]
		if !exists || !server.Enabled {
			m.mu.Unlock()
			return nil, fmt.Errorf("cursor MCP server is not configured")
		}
		processCtx, cancel := context.WithCancel(context.Background())
		session = &mcpStdioSession{server: server, ctx: processCtx, cancel: cancel}
		m.sessions[serverID] = session
	}
	m.mu.Unlock()
	return session.call(ctx, method, params)
}

// ServerIDs returns enabled configured servers in deterministic order.
func (m *MCPStdioManager) ServerIDs() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.servers))
	for id, server := range m.servers {
		if server.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// CloseAll terminates every active configured MCP child process.
func (m *MCPStdioManager) CloseAll() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closed = true
	sessions := make([]*mcpStdioSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	var result error
	for _, session := range sessions {
		result = errorsJoin(result, session.close())
	}
	return result
}

func (s *mcpStdioSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	stopSession := context.AfterFunc(s.ctx, cancel)
	stopCall := context.AfterFunc(ctx, s.cancel)
	defer func() { stopCall(); stopSession(); cancel() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errStart := s.startLocked(ctx); errStart != nil {
		return nil, errStart
	}
	return s.requestLocked(ctx, method, params)
}

func (s *mcpStdioSession) startLocked(ctx context.Context) error {
	if s.closed {
		return fmt.Errorf("cursor MCP server session is closed")
	}
	if s.cmd != nil {
		return nil
	}
	if s.server.Executable == "" || !filepath.IsAbs(s.server.Executable) {
		return fmt.Errorf("cursor MCP executable must be absolute")
	}
	controller, errController := newShellProcessController()
	if errController != nil {
		return errController
	}
	cmd := exec.CommandContext(s.ctx, s.server.Executable, s.server.Args...)
	controller.Configure(cmd)
	cmd.Dir = s.server.WorkingDir
	cmd.Env = mcpEnvironment(s.server.EnvAllowlist)
	stdin, errStdin := cmd.StdinPipe()
	if errStdin != nil {
		_ = controller.Close()
		return errStdin
	}
	stdout, errStdout := cmd.StdoutPipe()
	if errStdout != nil {
		_ = stdin.Close()
		_ = controller.Close()
		return errStdout
	}
	if errStart := cmd.Start(); errStart != nil {
		_ = stdin.Close()
		_ = controller.Close()
		return errStart
	}
	if errAttach := controller.Attach(cmd); errAttach != nil {
		_ = stdin.Close()
		_ = controller.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return errAttach
	}
	s.cmd = cmd
	s.stdin = stdin
	s.stdout = bufio.NewReaderSize(stdout, maxMCPMessageBytes+1)
	s.controller = controller
	if _, errInitialize := s.requestLocked(ctx, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "cli-proxy-api", "version": "1"}}); errInitialize != nil {
		_ = s.closeLocked()
		return fmt.Errorf("initialize Cursor MCP server: %w", errInitialize)
	}
	return s.notifyLocked("notifications/initialized", map[string]any{})
}

func (s *mcpStdioSession) requestLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}
	s.nextID++
	id := s.nextID
	if errWrite := writeMCPMessage(s.stdin, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); errWrite != nil {
		_ = s.closeLocked()
		return nil, errWrite
	}
	for {
		line, errRead := readMCPLine(ctx, s.stdout)
		if errRead != nil {
			if ctx.Err() != nil {
				_ = s.closeLocked()
				return nil, ctx.Err()
			}
			_ = s.closeLocked()
			return nil, errRead
		}
		var response struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      uint64          `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if errDecode := json.Unmarshal(line, &response); errDecode != nil {
			_ = s.closeLocked()
			return nil, fmt.Errorf("decode Cursor MCP response: %w", errDecode)
		}
		if response.ID == 0 {
			continue
		}
		if response.JSONRPC != "2.0" || response.ID != id {
			_ = s.closeLocked()
			return nil, fmt.Errorf("invalid Cursor MCP response ID")
		}
		if response.Error != nil {
			return nil, fmt.Errorf("Cursor MCP server returned an error: %s", response.Error.Message)
		}
		return response.Result, nil
	}
}

func readMCPLine(ctx context.Context, reader *bufio.Reader) ([]byte, error) {
	result := make(chan struct {
		line []byte
		err  error
	}, 1)
	go func() {
		line, errRead := readMCPLineBlocking(reader)
		result <- struct {
			line []byte
			err  error
		}{line: line, err: errRead}
	}()
	select {
	case output := <-result:
		return output.line, output.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func readMCPLineBlocking(reader *bufio.Reader) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("cursor MCP stdout is unavailable")
	}
	lines := bytes.NewBuffer(make([]byte, 0, 4096))
	for {
		part, errRead := reader.ReadSlice('\n')
		if len(part) > 0 {
			if lines.Len()+len(part) > maxMCPMessageBytes {
				return nil, fmt.Errorf("cursor MCP response exceeds byte limit")
			}
			_, _ = lines.Write(part)
		}
		if errRead == nil {
			return lines.Bytes(), nil
		}
		if errRead == bufio.ErrBufferFull {
			continue
		}
		return nil, errRead
	}
}

func (s *mcpStdioSession) notifyLocked(method string, params any) error {
	return writeMCPMessage(s.stdin, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *mcpStdioSession) close() error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *mcpStdioSession) closeLocked() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel()
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.controller != nil {
		_ = s.controller.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	return nil
}

func writeMCPMessage(writer io.Writer, message any) error {
	data, errMarshal := json.Marshal(message)
	if errMarshal != nil {
		return errMarshal
	}
	if len(data) > maxMCPMessageBytes {
		return fmt.Errorf("cursor MCP request exceeds byte limit")
	}
	_, errWrite := writer.Write(append(data, '\n'))
	return errWrite
}

func mcpEnvironment(names []string) []string {
	environment := make([]string, 0, len(names))
	for _, name := range names {
		if value, exists := os.LookupEnv(name); exists {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func errorsJoin(left, right error) error {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	return fmt.Errorf("%v; %w", left, right)
}
