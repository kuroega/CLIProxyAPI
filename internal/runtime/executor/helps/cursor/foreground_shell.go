package cursor

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
)

const foregroundOutputBuffer = 16

// ShellOutput is one bounded stdout or stderr chunk from a foreground shell.
type ShellOutput struct {
	Stdout bool
	Data   string
}

// ForegroundShell is a live stream shell which may later become a background shell.
type ForegroundShell struct {
	shell      *backgroundShell
	toolCallID string
	execID     uint32
	execKey    string
	output     chan ShellOutput
	promoted   chan struct{}

	mu         sync.Mutex
	isPromoted bool
}

// StartForeground starts a shell in the workspace and registers it by owner and tool call ID.
func (m *BackgroundShellManager) StartForeground(ctx context.Context, workspace, owner string, execID uint32, execKey string, args *cursorproto.ShellArgs) (*ForegroundShell, error) {
	if args == nil || args.GetCommand() == "" {
		return nil, fmt.Errorf("foreground shell command is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("foreground shell manager is closed")
	}
	if m.foregroundExec[foregroundKey(owner, execID)] != nil || (args.GetToolCallId() != "" && m.foreground[foregroundToolKey(owner, args.GetToolCallId())] != nil) {
		m.mu.Unlock()
		return nil, fmt.Errorf("foreground shell exec is already active")
	}
	m.starting.Add(1)
	defer m.starting.Done()
	m.mu.Unlock()
	root, errRoot := filepath.Abs(workspace)
	if errRoot != nil {
		return nil, fmt.Errorf("resolve workspace: %w", errRoot)
	}
	directory, errPath := workspacePath(root, args.GetWorkingDirectory())
	if errPath != nil {
		return nil, errPath
	}
	controller, errController := newShellProcessController()
	if errController != nil {
		return nil, errController
	}
	processCtx, cancel := context.WithCancel(context.Background())
	command := shellCommand(processCtx, args.GetCommand())
	controller.Configure(command)
	command.Dir = directory
	stdin, errStdin := command.StdinPipe()
	if errStdin != nil {
		_ = controller.Close()
		cancel()
		return nil, errStdin
	}
	stdout, errStdout := command.StdoutPipe()
	if errStdout != nil {
		_ = stdin.Close()
		_ = controller.Close()
		cancel()
		return nil, errStdout
	}
	stderr, errStderr := command.StderrPipe()
	if errStderr != nil {
		_ = stdin.Close()
		_ = controller.Close()
		cancel()
		return nil, errStderr
	}
	if errStart := command.Start(); errStart != nil {
		_ = stdin.Close()
		_ = controller.Close()
		cancel()
		return nil, errStart
	}
	if errAttach := controller.Attach(command); errAttach != nil {
		_ = stdin.Close()
		_ = controller.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		cancel()
		return nil, errAttach
	}
	foreground := &ForegroundShell{
		shell:      &backgroundShell{owner: owner, command: args.GetCommand(), directory: directory, stdinEnabled: true, cmd: command, stdin: stdin, controller: controller, cancel: cancel, done: make(chan struct{})},
		toolCallID: args.GetToolCallId(),
		execID:     execID,
		execKey:    execKey,
		output:     make(chan ShellOutput, foregroundOutputBuffer),
		promoted:   make(chan struct{}),
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		go m.waitForeground(foreground)
		_ = foreground.shell.Close()
		return nil, fmt.Errorf("foreground shell manager is closed")
	}
	var existing *ForegroundShell
	existing = m.foregroundExec[foregroundKey(owner, foreground.execID)]
	if existing != nil {
		m.mu.Unlock()
		go m.waitForeground(foreground)
		_ = foreground.shell.Close()
		return nil, fmt.Errorf("foreground shell exec is already active")
	}
	if foreground.toolCallID != "" {
		if _, exists := m.foreground[foregroundToolKey(foreground.shell.owner, foreground.toolCallID)]; exists {
			m.mu.Unlock()
			go m.waitForeground(foreground)
			_ = foreground.shell.Close()
			return nil, fmt.Errorf("foreground shell tool call is already active")
		}
		m.foreground[foregroundToolKey(foreground.shell.owner, foreground.toolCallID)] = foreground
	}
	m.foregroundExec[foregroundKey(owner, foreground.execID)] = foreground
	m.mu.Unlock()
	go m.drainForeground(foreground, stdout, true)
	go m.drainForeground(foreground, stderr, false)
	go m.waitForeground(foreground)
	_ = time.AfterFunc(maxShellTimeout, func() { _ = foreground.shell.Close() })
	return foreground, nil
}

// PromoteForeground transfers a running stream shell to the background shell map.
func (m *BackgroundShellManager) PromoteForeground(owner, toolCallID string) (*ForegroundShell, uint32, bool) {
	if m == nil || toolCallID == "" {
		return nil, 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	foreground := m.foreground[foregroundToolKey(owner, toolCallID)]
	if foreground == nil || foreground.shell.owner != owner {
		return nil, 0, false
	}
	select {
	case <-foreground.shell.done:
		delete(m.foreground, foregroundToolKey(owner, toolCallID))
		return nil, 0, false
	default:
	}
	foreground.mu.Lock()
	defer foreground.mu.Unlock()
	if foreground.isPromoted {
		return nil, 0, false
	}
	id := m.allocateIDLocked()
	foreground.shell.id = id
	foreground.isPromoted = true
	m.shells[id] = foreground.shell
	delete(m.foreground, foregroundToolKey(owner, toolCallID))
	delete(m.foregroundExec, foregroundKey(foreground.shell.owner, foreground.execID))
	close(foreground.promoted)
	return foreground, id, true
}

// ExecKey returns the Cursor attachable exec identity for this foreground shell.
func (s *ForegroundShell) ExecKey() string {
	if s == nil {
		return ""
	}
	return s.execKey
}

// ExecID returns the Cursor request correlation ID for this foreground shell.
func (s *ForegroundShell) ExecID() uint32 {
	if s == nil {
		return 0
	}
	return s.execID
}

// BackgroundDetails returns metadata needed for Cursor's backgrounded stream event.
func (s *ForegroundShell) BackgroundDetails() (command, directory string, pid uint32) {
	if s == nil || s.shell == nil || s.shell.cmd == nil || s.shell.cmd.Process == nil {
		return "", "", 0
	}
	return s.shell.command, s.shell.directory, uint32(s.shell.cmd.Process.Pid)
}

// AbortForeground stops one active shell stream by its Cursor exec request ID.
func (m *BackgroundShellManager) AbortForeground(owner string, execID uint32) bool {
	if m == nil || execID == 0 {
		return false
	}
	m.mu.Lock()
	foreground := m.foregroundExec[foregroundKey(owner, execID)]
	m.mu.Unlock()
	if foreground == nil || foreground.shell.owner != owner {
		return false
	}
	_ = foreground.shell.Close()
	return true
}

// StreamForeground emits shell events until the shell exits or is promoted.
func (s *ForegroundShell) StreamForeground(ctx context.Context, emit func(*cursorproto.ShellStream) error) (bool, error) {
	if s == nil || emit == nil {
		return false, fmt.Errorf("foreground shell stream is unavailable")
	}
	if errStart := emit(&cursorproto.ShellStream{Event: &cursorproto.ShellStream_Start{Start: &cursorproto.ShellStreamStart{}}}); errStart != nil {
		_ = s.shell.Close()
		return false, errStart
	}
	for {
		select {
		case <-ctx.Done():
			_ = s.shell.Close()
			return false, ctx.Err()
		case <-s.promoted:
			return true, nil
		case output := <-s.output:
			if output.Data == "" {
				continue
			}
			stream := &cursorproto.ShellStream{}
			if output.Stdout {
				stream.Event = &cursorproto.ShellStream_Stdout{Stdout: &cursorproto.ShellStreamStdout{Data: output.Data}}
			} else {
				stream.Event = &cursorproto.ShellStream_Stderr{Stderr: &cursorproto.ShellStreamStderr{Data: output.Data}}
			}
			if errEmit := emit(stream); errEmit != nil {
				_ = s.shell.Close()
				return false, errEmit
			}
		case <-s.shell.done:
			return false, emit(&cursorproto.ShellStream{Event: &cursorproto.ShellStream_Exit{Exit: &cursorproto.ShellStreamExit{Code: foregroundExitCode(s.shell), Cwd: s.shell.directory}}})
		}
	}
}

func (m *BackgroundShellManager) drainForeground(shell *ForegroundShell, reader io.Reader, stdout bool) {
	buffer := make([]byte, 8<<10)
	for {
		count, errRead := reader.Read(buffer)
		if count > 0 {
			shell.shell.output.Add(uint64(count))
			shell.mu.Lock()
			promoted := shell.isPromoted
			shell.mu.Unlock()
			if !promoted {
				select {
				case shell.output <- ShellOutput{Stdout: stdout, Data: string(buffer[:count])}:
				default:
				}
			}
		}
		if errRead != nil {
			return
		}
	}
}

func (m *BackgroundShellManager) waitForeground(foreground *ForegroundShell) {
	_ = foreground.shell.cmd.Wait()
	if foreground.shell.controller != nil {
		_ = foreground.shell.controller.Close()
	}
	_ = foreground.shell.stdin.Close()
	foreground.shell.cancel()
	m.mu.Lock()
	if foreground.toolCallID != "" && m.foreground[foregroundToolKey(foreground.shell.owner, foreground.toolCallID)] == foreground {
		delete(m.foreground, foregroundToolKey(foreground.shell.owner, foreground.toolCallID))
	}
	if m.foregroundExec[foregroundKey(foreground.shell.owner, foreground.execID)] == foreground {
		delete(m.foregroundExec, foregroundKey(foreground.shell.owner, foreground.execID))
	}
	if foreground.shell.id != 0 && m.shells[foreground.shell.id] == foreground.shell {
		delete(m.shells, foreground.shell.id)
	}
	m.mu.Unlock()
	close(foreground.shell.done)
}

func foregroundExitCode(shell *backgroundShell) uint32 {
	if shell == nil || shell.cmd == nil || shell.cmd.ProcessState == nil {
		return 1
	}
	code := shell.cmd.ProcessState.ExitCode()
	if code < 0 {
		return 1
	}
	return uint32(code)
}

func foregroundToolKey(owner, toolID string) string { return fmt.Sprintf("%q:%s", owner, toolID) }

func foregroundKey(owner string, execID uint32) string { return fmt.Sprintf("%s-%d", owner, execID) }
