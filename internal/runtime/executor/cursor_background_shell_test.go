package executor

import (
	"context"
	"io"
	"runtime"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
	cursorconnect "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursor"
	"google.golang.org/protobuf/proto"
)

func TestCursorBackgroundShellExecRepliesWithSpawnAndStdinResults(t *testing.T) {
	command := `read line; printf "%s" "$line"`
	if runtime.GOOS == "windows" {
		command = `$line = [Console]::In.ReadLine(); Write-Output $line`
	}
	executor := NewCursorExecutor(&config.Config{CursorTools: config.CursorToolsConfig{Workspace: t.TempDir(), Shell: true}})
	defer func() {
		if errClose := executor.shells.CloseAll(); errClose != nil {
			t.Errorf("close shells: %v", errClose)
		}
	}()
	reader, writer := io.Pipe()
	duplex := &cursorDuplexWriter{writer: writer}
	spawnRequest := &cursorproto.ExecServerMessage{Id: 31, ExecId: "exec-spawn", Message: &cursorproto.ExecServerMessage_BackgroundShellSpawnArgs{BackgroundShellSpawnArgs: &cursorproto.BackgroundShellSpawnArgs{Command: command, WorkingDirectory: ".", EnableWriteShellStdinTool: true}}}
	errs := make(chan error, 1)
	go func() { errs <- executor.handleCursorExec(context.Background(), duplex, spawnRequest, "owner", nil) }()
	spawnReply := readCursorExecReply(t, reader)
	if errHandle := <-errs; errHandle != nil {
		t.Fatalf("spawn handler: %v", errHandle)
	}
	spawn := spawnReply.GetBackgroundShellSpawnResult().GetSuccess()
	if spawnReply.GetId() != spawnRequest.GetId() || spawnReply.GetExecId() != spawnRequest.GetExecId() || spawn == nil || spawn.GetShellId() == 0 {
		t.Fatalf("spawn reply = %#v", spawnReply)
	}

	stdinRequest := &cursorproto.ExecServerMessage{Id: 32, ExecId: "exec-stdin", Message: &cursorproto.ExecServerMessage_WriteShellStdinArgs{WriteShellStdinArgs: &cursorproto.WriteShellStdinArgs{ShellId: spawn.GetShellId(), Chars: "hello\n"}}}
	go func() { errs <- executor.handleCursorExec(context.Background(), duplex, stdinRequest, "owner", nil) }()
	stdinReply := readCursorExecReply(t, reader)
	if errHandle := <-errs; errHandle != nil {
		t.Fatalf("stdin handler: %v", errHandle)
	}
	if stdinReply.GetId() != stdinRequest.GetId() || stdinReply.GetExecId() != stdinRequest.GetExecId() || stdinReply.GetWriteShellStdinResult().GetSuccess().GetShellId() != spawn.GetShellId() {
		t.Fatalf("stdin reply = %#v", stdinReply)
	}
	forceRequest := &cursorproto.ExecServerMessage{Id: 33, ExecId: "exec-force", Message: &cursorproto.ExecServerMessage_ForceBackgroundShellArgs{ForceBackgroundShellArgs: &cursorproto.ForceBackgroundShellArgs{ToolCallId: "missing"}}}
	go func() { errs <- executor.handleCursorExec(context.Background(), duplex, forceRequest, "owner", nil) }()
	forceReply := readCursorExecReply(t, reader)
	if errHandle := <-errs; errHandle != nil {
		t.Fatalf("force-background handler: %v", errHandle)
	}
	if forceReply.GetId() != forceRequest.GetId() || forceReply.GetExecId() != forceRequest.GetExecId() || forceReply.GetForceBackgroundShellResult().GetStatus() != cursorproto.ForceBackgroundShellStatus_FORCE_BACKGROUND_SHELL_STATUS_NOT_FOUND {
		t.Fatalf("force-background reply = %#v", forceReply)
	}
	_ = reader.Close()
}

func readCursorExecReply(t *testing.T, reader io.Reader) *cursorproto.ExecClientMessage {
	t.Helper()
	frame, errFrame := cursorconnect.ReadFrame(reader)
	if errFrame != nil {
		t.Fatalf("read client frame: %v", errFrame)
	}
	var message cursorproto.AgentClientMessage
	if errDecode := proto.Unmarshal(frame.Payload, &message); errDecode != nil {
		t.Fatalf("decode client frame: %v", errDecode)
	}
	if message.GetExecClientMessage() == nil {
		t.Fatalf("client message = %#v", message)
	}
	return message.GetExecClientMessage()
}
