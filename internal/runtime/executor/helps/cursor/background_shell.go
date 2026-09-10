package cursor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
)

const maxBackgroundShells = 32

// ShellResourceBinder owns resources that must be closed with an execution lifecycle.
type ShellResourceBinder interface {
	Bind(func() error) error
}

// BackgroundShellManager owns detached Cursor shell processes.
type BackgroundShellManager struct {
	mu             sync.Mutex
	starting       sync.WaitGroup
	nextID         uint32
	shells         map[uint32]*backgroundShell
	foreground     map[string]*ForegroundShell
	foregroundExec map[string]*ForegroundShell
	closed         bool
}

type shellProcessController interface {
	Configure(*exec.Cmd)
	Attach(*exec.Cmd) error
	Close() error
}

type backgroundShell struct {
	id           uint32
	owner        string
	command      string
	directory    string
	stdinEnabled bool

	cmd        *exec.Cmd
	stdin      io.WriteCloser
	controller shellProcessController
	cancel     context.CancelFunc
	done       chan struct{}

	writeMu sync.Mutex
	output  atomic.Uint64
	close   sync.Once
}

// NewBackgroundShellManager returns an empty, bounded detached-shell manager.
func NewBackgroundShellManager() *BackgroundShellManager {
	return &BackgroundShellManager{
		shells:         make(map[uint32]*backgroundShell),
		foreground:     make(map[string]*ForegroundShell),
		foregroundExec: make(map[string]*ForegroundShell),
	}
}

// Spawn starts a shell with a workspace working directory, not a filesystem sandbox.
func (m *BackgroundShellManager) Spawn(ctx context.Context, workspace, owner string, args *cursorproto.BackgroundShellSpawnArgs, binder ShellResourceBinder) *cursorproto.BackgroundShellSpawnResult {
	if args == nil {
		return backgroundShellError("", "", "background shell arguments are missing")
	}
	if errContext := ctx.Err(); errContext != nil {
		return backgroundShellError(args.GetCommand(), args.GetWorkingDirectory(), errContext.Error())
	}
	if args.GetCommand() == "" {
		return backgroundShellError("", args.GetWorkingDirectory(), "background shell command is empty")
	}
	root, errRoot := filepath.Abs(workspace)
	if errRoot != nil {
		return backgroundShellError(args.GetCommand(), args.GetWorkingDirectory(), fmt.Sprintf("resolve workspace: %v", errRoot))
	}
	directory, errPath := workspacePath(root, args.GetWorkingDirectory())
	if errPath != nil {
		return &cursorproto.BackgroundShellSpawnResult{Result: &cursorproto.BackgroundShellSpawnResult_Rejected{Rejected: &cursorproto.ShellRejected{Command: args.GetCommand(), WorkingDirectory: args.GetWorkingDirectory(), Reason: errPath.Error()}}}
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return backgroundShellError(args.GetCommand(), directory, "background shell manager is closed")
	}
	if len(m.shells) >= maxBackgroundShells {
		m.mu.Unlock()
		return backgroundShellError(args.GetCommand(), directory, "background shell limit reached")
	}
	id := m.allocateIDLocked()
	m.starting.Add(1)
	defer m.starting.Done()
	m.mu.Unlock()

	processCtx, cancel := context.WithCancel(context.Background())
	controller, errController := newShellProcessController()
	if errController != nil {
		cancel()
		return backgroundShellError(args.GetCommand(), directory, errController.Error())
	}
	cmd := shellCommand(processCtx, args.GetCommand())
	controller.Configure(cmd)
	cmd.Dir = directory
	stdin, errStdin := cmd.StdinPipe()
	if errStdin != nil {
		_ = controller.Close()
		cancel()
		return backgroundShellError(args.GetCommand(), directory, errStdin.Error())
	}
	stdout, errStdout := cmd.StdoutPipe()
	if errStdout != nil {
		_ = stdin.Close()
		_ = controller.Close()
		cancel()
		return backgroundShellError(args.GetCommand(), directory, errStdout.Error())
	}
	stderr, errStderr := cmd.StderrPipe()
	if errStderr != nil {
		_ = stdin.Close()
		_ = controller.Close()
		cancel()
		return backgroundShellError(args.GetCommand(), directory, errStderr.Error())
	}
	if errStart := cmd.Start(); errStart != nil {
		_ = stdin.Close()
		_ = controller.Close()
		cancel()
		return backgroundShellError(args.GetCommand(), directory, errStart.Error())
	}
	if errAttach := controller.Attach(cmd); errAttach != nil {
		_ = stdin.Close()
		_ = controller.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cancel()
		return backgroundShellError(args.GetCommand(), directory, errAttach.Error())
	}

	shell := &backgroundShell{id: id, owner: owner, command: args.GetCommand(), directory: directory, stdinEnabled: args.GetEnableWriteShellStdinTool(), cmd: cmd, stdin: stdin, controller: controller, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		go m.wait(shell)
		_ = shell.Close()
		return backgroundShellError(args.GetCommand(), directory, "background shell manager is closed")
	}
	m.shells[id] = shell
	m.mu.Unlock()
	go drainBackgroundShellOutput(stdout, &shell.output)
	go drainBackgroundShellOutput(stderr, &shell.output)
	go m.wait(shell)

	if binder != nil {
		if errBind := binder.Bind(shell.Close); errBind != nil {
			_ = shell.Close()
			return backgroundShellError(args.GetCommand(), directory, errBind.Error())
		}
	}
	_ = time.AfterFunc(maxShellTimeout, func() { _ = shell.Close() })
	pid := uint32(cmd.Process.Pid)
	return &cursorproto.BackgroundShellSpawnResult{Result: &cursorproto.BackgroundShellSpawnResult_Success{Success: &cursorproto.BackgroundShellSpawnSuccess{ShellId: id, Command: args.GetCommand(), WorkingDirectory: directory, Pid: &pid}}}
}

// WriteStdin appends chars to one attached shell's standard input.
func (m *BackgroundShellManager) WriteStdin(ctx context.Context, owner string, args *cursorproto.WriteShellStdinArgs) *cursorproto.WriteShellStdinResult {
	if args == nil {
		return backgroundStdinError("background shell stdin arguments are missing")
	}
	if len(args.GetChars()) > maxShellOutputBytes {
		return backgroundStdinError("background shell stdin exceeds byte limit")
	}
	shell := m.shell(args.GetShellId())
	if shell == nil || shell.owner != owner {
		return backgroundStdinError("background shell is not available")
	}
	if !shell.stdinEnabled {
		return backgroundStdinError("background shell stdin is disabled")
	}
	select {
	case <-shell.done:
		return backgroundStdinError("background shell has exited")
	default:
	}
	shell.writeMu.Lock()
	defer shell.writeMu.Unlock()
	before := saturatingUint32(shell.output.Load())
	select {
	case <-ctx.Done():
		return backgroundStdinError(ctx.Err().Error())
	default:
	}
	if _, errWrite := io.WriteString(shell.stdin, args.GetChars()); errWrite != nil {
		return backgroundStdinError("background shell stdin write failed")
	}
	return &cursorproto.WriteShellStdinResult{Result: &cursorproto.WriteShellStdinResult_Success{Success: &cursorproto.WriteShellStdinSuccess{ShellId: shell.id, TerminalFileLengthBeforeInputWritten: before}}}
}

// CloseShell stops and reaps one owned background shell.
func (m *BackgroundShellManager) CloseShell(owner string, id uint32) error {
	shell := m.shell(id)
	if shell == nil || shell.owner != owner {
		return nil
	}
	return shell.Close()
}

// CloseOwner stops and reaps all shells belonging to one request owner.
func (m *BackgroundShellManager) CloseOwner(owner string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	var shells []*backgroundShell
	for _, shell := range m.shells {
		if shell.owner == owner {
			shells = append(shells, shell)
		}
	}
	for _, foreground := range m.foregroundExec {
		if foreground.shell.owner == owner {
			shells = append(shells, foreground.shell)
		}
	}
	m.mu.Unlock()
	var result error
	for _, shell := range shells {
		result = errors.Join(result, shell.Close())
	}
	return result
}

// CloseAll stops and reaps every tracked background shell.
func (m *BackgroundShellManager) CloseAll() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.starting.Wait()
	m.mu.Lock()
	shells := make([]*backgroundShell, 0, len(m.shells)+len(m.foregroundExec))
	for _, shell := range m.shells {
		shells = append(shells, shell)
	}
	for _, foreground := range m.foregroundExec {
		shells = append(shells, foreground.shell)
	}
	m.mu.Unlock()
	var result error
	for _, shell := range shells {
		result = errors.Join(result, shell.Close())
	}
	return result
}

func (m *BackgroundShellManager) allocateIDLocked() uint32 {
	for {
		m.nextID++
		if m.nextID == 0 {
			m.nextID++
		}
		if m.shells[m.nextID] == nil {
			return m.nextID
		}
	}
}

func (m *BackgroundShellManager) shell(id uint32) *backgroundShell {
	if m == nil || id == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shells[id]
}

func (m *BackgroundShellManager) wait(shell *backgroundShell) {
	_ = shell.cmd.Wait()
	if shell.controller != nil {
		_ = shell.controller.Close()
	}
	_ = shell.stdin.Close()
	shell.cancel()
	m.mu.Lock()
	if m.shells[shell.id] == shell {
		delete(m.shells, shell.id)
	}
	m.mu.Unlock()
	close(shell.done)
}

func (s *backgroundShell) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	s.close.Do(func() {
		s.cancel()
		if s.stdin != nil {
			closeErr = errors.Join(closeErr, s.stdin.Close())
		}
		if s.controller != nil {
			closeErr = errors.Join(closeErr, s.controller.Close())
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-s.done
	})
	return closeErr
}

func drainBackgroundShellOutput(reader io.Reader, counter *atomic.Uint64) {
	buffer := make([]byte, 8<<10)
	for {
		count, errRead := reader.Read(buffer)
		if count > 0 {
			counter.Add(uint64(count))
		}
		if errRead != nil {
			return
		}
	}
}

func backgroundShellError(command, directory, message string) *cursorproto.BackgroundShellSpawnResult {
	return &cursorproto.BackgroundShellSpawnResult{Result: &cursorproto.BackgroundShellSpawnResult_Error{Error: &cursorproto.BackgroundShellSpawnError{Command: command, WorkingDirectory: directory, Error: message}}}
}

func backgroundStdinError(message string) *cursorproto.WriteShellStdinResult {
	return &cursorproto.WriteShellStdinResult{Result: &cursorproto.WriteShellStdinResult_Error{Error: &cursorproto.WriteShellStdinError{Error: message}}}
}

func saturatingUint32(value uint64) uint32 {
	if value > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(value)
}
