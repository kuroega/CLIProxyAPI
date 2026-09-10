package executor

import (
	"runtime"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
	cursorconnect "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursor"
)

func TestCursorForeignAbortPreservesOwnerMapping(t *testing.T) {
	executor := &CursorExecutor{shells: cursorconnect.NewBackgroundShellManager()}
	executor.rememberShell("owner", 47, 99)
	if err := executor.abortCursorExec("foreign", 47); err != nil {
		t.Fatal(err)
	}
	if len(executor.execShells) != 1 {
		t.Fatal("foreign abort removed the owner's execution mapping")
	}
}

func TestCursorAbortExecStopsRememberedBackgroundShell(t *testing.T) {
	command := `while true; do :; done`
	if runtime.GOOS == "windows" {
		command = `while ($true) { Start-Sleep -Milliseconds 100 }`
	}
	executor := &CursorExecutor{shells: cursorconnect.NewBackgroundShellManager()}
	spawn := executor.shells.Spawn(t.Context(), t.TempDir(), "owner", &cursorproto.BackgroundShellSpawnArgs{Command: command, WorkingDirectory: "."}, nil)
	if spawn.GetSuccess() == nil {
		t.Fatalf("spawn result = %#v", spawn)
	}
	executor.rememberShell("owner", 47, spawn.GetSuccess().GetShellId())
	if errAbort := executor.abortCursorExec("owner", 47); errAbort != nil {
		t.Fatalf("abortCursorExec() error = %v", errAbort)
	}
	if shell := executor.shells; shell == nil {
		t.Fatal("background shell manager is nil")
	} else if got := shell.CloseShell("owner", spawn.GetSuccess().GetShellId()); got != nil {
		t.Fatalf("repeated close error = %v", got)
	}
}
