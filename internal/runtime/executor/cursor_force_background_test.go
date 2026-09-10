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

func TestCursorForceBackgroundPromotesStreamShell(t *testing.T) {
	command := `while true; do :; done`
	if runtime.GOOS == "windows" {
		command = `while ($true) { Start-Sleep -Milliseconds 100 }`
	}
	executor := NewCursorExecutor(&config.Config{CursorTools: config.CursorToolsConfig{Workspace: t.TempDir(), Shell: true}})
	defer func() { _ = executor.shells.CloseAll() }()
	reader, writer := io.Pipe()
	duplex := &cursorDuplexWriter{writer: writer}
	streamRequest := &cursorproto.ExecServerMessage{Id: 71, ExecId: "exec-stream", Message: &cursorproto.ExecServerMessage_ShellStreamArgs{ShellStreamArgs: &cursorproto.ShellArgs{Command: command, WorkingDirectory: ".", ToolCallId: "tool-stream"}}}
	if errStart := executor.handleCursorExec(context.Background(), duplex, streamRequest, "owner", nil); errStart != nil {
		t.Fatalf("start stream: %v", errStart)
	}
	start := readCursorExecReply(t, reader)
	if start.GetId() != streamRequest.GetId() || start.GetShellStream().GetStart() == nil {
		t.Fatalf("start reply = %#v", start)
	}

	forceRequest := &cursorproto.ExecServerMessage{Id: 72, ExecId: "exec-force", Message: &cursorproto.ExecServerMessage_ForceBackgroundShellArgs{ForceBackgroundShellArgs: &cursorproto.ForceBackgroundShellArgs{ToolCallId: "tool-stream"}}}
	errs := make(chan error, 1)
	go func() { errs <- executor.handleCursorExec(context.Background(), duplex, forceRequest, "owner", nil) }()
	force := readCursorExecReply(t, reader)
	if force.GetId() != forceRequest.GetId() || force.GetExecId() != forceRequest.GetExecId() || force.GetForceBackgroundShellResult().GetStatus() != cursorproto.ForceBackgroundShellStatus_FORCE_BACKGROUND_SHELL_STATUS_ACCEPTED {
		t.Fatalf("force reply = %#v", force)
	}
	backgrounded := readCursorExecReply(t, reader)
	stream := backgrounded.GetShellStream()
	if backgrounded.GetId() != streamRequest.GetId() || backgrounded.GetExecId() != streamRequest.GetExecId() || stream.GetBackgrounded() == nil || stream.GetBackgrounded().GetShellId() == 0 {
		t.Fatalf("backgrounded reply = %#v", backgrounded)
	}
	control := readCursorControl(t, reader)
	if control.GetStreamClose() == nil || control.GetStreamClose().GetId() != streamRequest.GetId() {
		t.Fatalf("close control = %#v", control)
	}
	if errForce := <-errs; errForce != nil {
		t.Fatalf("force handler: %v", errForce)
	}
	if errAbort := executor.abortCursorExec("owner", streamRequest.GetId()); errAbort != nil {
		t.Fatalf("abort promoted shell: %v", errAbort)
	}
	_ = reader.Close()
}

func readCursorControl(t *testing.T, reader io.Reader) *cursorproto.ExecClientControlMessage {
	t.Helper()
	frame, errFrame := cursorconnect.ReadFrame(reader)
	if errFrame != nil {
		t.Fatalf("read control frame: %v", errFrame)
	}
	var message cursorproto.AgentClientMessage
	if errDecode := proto.Unmarshal(frame.Payload, &message); errDecode != nil {
		t.Fatalf("decode control frame: %v", errDecode)
	}
	if message.GetExecClientControlMessage() == nil {
		t.Fatalf("client message = %#v", message)
	}
	return message.GetExecClientControlMessage()
}
