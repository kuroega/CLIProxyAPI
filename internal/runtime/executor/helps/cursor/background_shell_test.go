package cursor

import (
	"context"
	"runtime"
	"testing"
	"time"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
)

func TestBackgroundShellSpawnAndWriteStdin(t *testing.T) {
	command := `read line; printf "%s" "$line"`
	if runtime.GOOS == "windows" {
		command = `$line = [Console]::In.ReadLine(); Write-Output $line`
	}
	manager := NewBackgroundShellManager()
	result := manager.Spawn(t.Context(), t.TempDir(), "owner", &cursorproto.BackgroundShellSpawnArgs{Command: command, WorkingDirectory: ".", EnableWriteShellStdinTool: true}, nil)
	success := result.GetSuccess()
	if success == nil || success.GetShellId() == 0 {
		t.Fatalf("spawn result = %#v", result)
	}
	shell := manager.shell(success.GetShellId())
	if shell == nil {
		t.Fatal("spawned shell was not registered")
	}
	stdinResult := manager.WriteStdin(t.Context(), "owner", &cursorproto.WriteShellStdinArgs{ShellId: success.GetShellId(), Chars: "hello\n"})
	if stdinResult.GetSuccess() == nil || stdinResult.GetSuccess().GetShellId() != success.GetShellId() {
		t.Fatalf("stdin result = %#v", stdinResult)
	}
	select {
	case <-shell.done:
	case <-time.After(5 * time.Second):
		t.Fatal("background shell did not exit after stdin")
	}
	if got := manager.shell(success.GetShellId()); got != nil {
		t.Fatal("exited shell was retained")
	}
}

func TestBackgroundShellRejectsUnknownOwnerAndDisabledStdin(t *testing.T) {
	manager := NewBackgroundShellManager()
	if result := manager.WriteStdin(context.Background(), "owner", &cursorproto.WriteShellStdinArgs{ShellId: 99, Chars: "ignored"}); result.GetError() == nil {
		t.Fatalf("unknown shell result = %#v", result)
	}
	command := `read line`
	if runtime.GOOS == "windows" {
		command = `$line = [Console]::In.ReadLine()`
	}
	spawn := manager.Spawn(t.Context(), t.TempDir(), "owner-a", &cursorproto.BackgroundShellSpawnArgs{Command: command, WorkingDirectory: "."}, nil)
	if spawn.GetSuccess() == nil {
		t.Fatalf("spawn result = %#v", spawn)
	}
	if result := manager.WriteStdin(t.Context(), "owner-b", &cursorproto.WriteShellStdinArgs{ShellId: spawn.GetSuccess().GetShellId(), Chars: "ignored"}); result.GetError() == nil {
		t.Fatalf("foreign owner result = %#v", result)
	}
	if result := manager.WriteStdin(t.Context(), "owner-a", &cursorproto.WriteShellStdinArgs{ShellId: spawn.GetSuccess().GetShellId(), Chars: "ignored"}); result.GetError() == nil {
		t.Fatalf("disabled stdin result = %#v", result)
	}
	if errClose := manager.CloseAll(); errClose != nil {
		t.Fatalf("CloseAll() error = %v", errClose)
	}
}

func TestBackgroundShellBinderClosesProcess(t *testing.T) {
	manager := NewBackgroundShellManager()
	binder := &testShellBinder{}
	command := `while true; do :; done`
	if runtime.GOOS == "windows" {
		command = `while ($true) { Start-Sleep -Milliseconds 100 }`
	}
	spawn := manager.Spawn(t.Context(), t.TempDir(), "owner", &cursorproto.BackgroundShellSpawnArgs{Command: command, WorkingDirectory: "."}, binder)
	if spawn.GetSuccess() == nil || binder.close == nil {
		t.Fatalf("spawn result = %#v, binder = %#v", spawn, binder)
	}
	if errClose := binder.close(); errClose != nil {
		t.Fatalf("bound close error = %v", errClose)
	}
	if got := manager.shell(spawn.GetSuccess().GetShellId()); got != nil {
		t.Fatal("lifecycle close retained shell")
	}
}

func TestForegroundShellAbortUsesExecID(t *testing.T) {
	command := `while true; do :; done`
	if runtime.GOOS == "windows" {
		command = `while ($true) { Start-Sleep -Milliseconds 100 }`
	}
	manager := NewBackgroundShellManager()
	foreground, errStart := manager.StartForeground(t.Context(), t.TempDir(), "owner", 62, "exec-abort", &cursorproto.ShellArgs{Command: command, WorkingDirectory: "."})
	if errStart != nil {
		t.Fatalf("StartForeground() error = %v", errStart)
	}
	if manager.AbortForeground("other-owner", foreground.ExecID()) {
		t.Fatal("foreign owner aborted foreground shell")
	}
	if !manager.AbortForeground("owner", foreground.ExecID()) {
		t.Fatal("owner could not abort foreground shell")
	}
	select {
	case <-foreground.shell.done:
	case <-time.After(5 * time.Second):
		t.Fatal("foreground shell did not exit after abort")
	}
}

func TestForegroundExecIDsAreOwnerScoped(t *testing.T) {
	command := `while true; do :; done`
	if runtime.GOOS == "windows" {
		command = `while ($true) { Start-Sleep -Milliseconds 100 }`
	}
	manager := NewBackgroundShellManager()
	first, err := manager.StartForeground(t.Context(), t.TempDir(), "owner-a", 7, "a", &cursorproto.ShellArgs{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.StartForeground(t.Context(), t.TempDir(), "owner-b", 7, "b", &cursorproto.ShellArgs{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("distinct owners shared foreground shell")
	}
	if _, err = manager.StartForeground(t.Context(), t.TempDir(), "owner-a", 7, "a2", &cursorproto.ShellArgs{Command: command}); err == nil {
		t.Fatal("same owner reused exec ID")
	}
	if err := manager.CloseOwner("owner-a"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.shell.done:
	case <-time.After(5 * time.Second):
		t.Fatal("owner shell did not close")
	}
	if err := manager.CloseAll(); err != nil {
		t.Fatal(err)
	}
}

func TestForegroundShellPromotionReusesProcess(t *testing.T) {
	command := `while true; do :; done`
	if runtime.GOOS == "windows" {
		command = `while ($true) { Start-Sleep -Milliseconds 100 }`
	}
	manager := NewBackgroundShellManager()
	foreground, errStart := manager.StartForeground(t.Context(), t.TempDir(), "owner", 61, "exec-foreground", &cursorproto.ShellArgs{Command: command, WorkingDirectory: ".", ToolCallId: "tool-call"})
	if errStart != nil {
		t.Fatalf("StartForeground() error = %v", errStart)
	}
	promoted, shellID, ok := manager.PromoteForeground("owner", "tool-call")
	if !ok || promoted != foreground || shellID == 0 || foreground.ExecID() != 61 || foreground.ExecKey() != "exec-foreground" {
		t.Fatalf("PromoteForeground() = %#v, %d, %t", promoted, shellID, ok)
	}
	select {
	case <-foreground.promoted:
	default:
		t.Fatal("promotion did not notify foreground stream")
	}
	if errClose := manager.CloseShell("owner", shellID); errClose != nil {
		t.Fatalf("CloseShell() error = %v", errClose)
	}
}

type testShellBinder struct {
	close func() error
}

func (b *testShellBinder) Bind(close func() error) error {
	b.close = close
	return nil
}
