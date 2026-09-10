package cursor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
)

func TestHandleWorkspaceExecRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if errWrite := os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	request := &cursorproto.ExecServerMessage{Id: 7, Message: &cursorproto.ExecServerMessage_ReadArgs{ReadArgs: &cursorproto.ReadArgs{Path: "sample.txt", Offset: protoInt32(1), Limit: protoUint32(1)}}}
	response, errHandle := HandleWorkspaceExec(context.Background(), root, request)
	if errHandle != nil {
		t.Fatalf("HandleWorkspaceExec() error = %v", errHandle)
	}
	success := response.GetReadResult().GetSuccess()
	if success == nil || success.GetContent() != "second" || success.GetTotalLines() != 3 || !success.GetRangeApplied() {
		t.Fatalf("read result = %#v", response.GetReadResult())
	}
}

func TestHandleWorkspaceExecRejectsOutsidePath(t *testing.T) {
	root := t.TempDir()
	request := &cursorproto.ExecServerMessage{Id: 9, Message: &cursorproto.ExecServerMessage_ReadArgs{ReadArgs: &cursorproto.ReadArgs{Path: "..\\outside.txt"}}}
	response, errHandle := HandleWorkspaceExec(context.Background(), root, request)
	if errHandle != nil {
		t.Fatalf("HandleWorkspaceExec() error = %v", errHandle)
	}
	if response.GetReadResult().GetRejected() == nil {
		t.Fatalf("read result = %#v", response.GetReadResult())
	}
}

func TestHandleWorkspaceExecGrepAndList(t *testing.T) {
	root := t.TempDir()
	if errMkdir := os.Mkdir(filepath.Join(root, "nested"), 0o700); errMkdir != nil {
		t.Fatalf("create nested directory: %v", errMkdir)
	}
	if errWrite := os.WriteFile(filepath.Join(root, "nested", "match.go"), []byte("package sample\n// target\n"), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	grepRequest := &cursorproto.ExecServerMessage{Id: 11, Message: &cursorproto.ExecServerMessage_GrepArgs{GrepArgs: &cursorproto.GrepArgs{Pattern: "target", OutputMode: protoString("content")}}}
	grepResponse, errGrep := HandleWorkspaceExec(context.Background(), root, grepRequest)
	if errGrep != nil {
		t.Fatalf("grep error = %v", errGrep)
	}
	grep := grepResponse.GetGrepResult().GetSuccess()
	if grep == nil || len(grep.GetWorkspaceResults()[root].GetContent().GetMatches()) != 1 {
		t.Fatalf("grep result = %#v", grepResponse.GetGrepResult())
	}
	listRequest := &cursorproto.ExecServerMessage{Id: 12, Message: &cursorproto.ExecServerMessage_LsArgs{LsArgs: &cursorproto.LsArgs{Path: "."}}}
	listResponse, errList := HandleWorkspaceExec(context.Background(), root, listRequest)
	if errList != nil {
		t.Fatalf("list error = %v", errList)
	}
	tree := listResponse.GetLsResult().GetSuccess().GetDirectoryTreeRoot()
	if tree == nil || len(tree.GetChildrenDirs()) != 1 || tree.GetChildrenDirs()[0].GetAbsPath() != filepath.Join(root, "nested") {
		t.Fatalf("list result = %#v", listResponse.GetLsResult())
	}
}

func protoInt32(value int32) *int32    { return &value }
func protoUint32(value uint32) *uint32 { return &value }
func protoString(value string) *string { return &value }

func TestHandleWorkspaceExecRejectsSymlinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if errWrite := os.WriteFile(outside, []byte("outside"), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	link := filepath.Join(root, "outside-link.txt")
	if errLink := os.Symlink(outside, link); errLink != nil {
		t.Skipf("create symlink: %v", errLink)
	}
	request := &cursorproto.ExecServerMessage{Id: 13, Message: &cursorproto.ExecServerMessage_ReadArgs{ReadArgs: &cursorproto.ReadArgs{Path: "outside-link.txt"}}}
	response, errHandle := HandleWorkspaceExec(context.Background(), root, request)
	if errHandle != nil {
		t.Fatalf("HandleWorkspaceExec() error = %v", errHandle)
	}
	if response.GetReadResult().GetRejected() == nil {
		t.Fatalf("read result = %#v", response.GetReadResult())
	}
}

func TestHandleWorkspaceExecWriteAndDelete(t *testing.T) {
	root := t.TempDir()
	writeRequest := &cursorproto.ExecServerMessage{Id: 14, Message: &cursorproto.ExecServerMessage_WriteArgs{WriteArgs: &cursorproto.WriteArgs{Path: "nested/new.txt", FileText: "created\n", ReturnFileContentAfterWrite: true}}}
	writeResponse, errWrite := HandleWorkspaceExec(context.Background(), root, writeRequest)
	if errWrite != nil {
		t.Fatalf("write error = %v", errWrite)
	}
	if success := writeResponse.GetWriteResult().GetSuccess(); success == nil || success.GetFileContentAfterWrite() != "created\n" {
		t.Fatalf("write result = %#v", writeResponse.GetWriteResult())
	}
	path := filepath.Join(root, "nested", "new.txt")
	if data, errRead := os.ReadFile(path); errRead != nil || string(data) != "created\n" {
		t.Fatalf("written file = %q, %v", data, errRead)
	}
	deleteRequest := &cursorproto.ExecServerMessage{Id: 15, Message: &cursorproto.ExecServerMessage_DeleteArgs{DeleteArgs: &cursorproto.DeleteArgs{Path: "nested/new.txt"}}}
	deleteResponse, errDelete := HandleWorkspaceExec(context.Background(), root, deleteRequest)
	if errDelete != nil {
		t.Fatalf("delete error = %v", errDelete)
	}
	if success := deleteResponse.GetDeleteResult().GetSuccess(); success == nil || success.GetPrevContent() != "created\n" {
		t.Fatalf("delete result = %#v", deleteResponse.GetDeleteResult())
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("deleted file stat error = %v", errStat)
	}
}

func TestHandleWorkspaceExecShell(t *testing.T) {
	command := "printf shell-ok"
	if runtime.GOOS == "windows" {
		command = "Write-Output shell-ok"
	}
	request := &cursorproto.ExecServerMessage{Id: 16, Message: &cursorproto.ExecServerMessage_ShellArgs{ShellArgs: &cursorproto.ShellArgs{Command: command, WorkingDirectory: "."}}}
	response, errHandle := HandleWorkspaceExec(context.Background(), t.TempDir(), request)
	if errHandle != nil {
		t.Fatalf("shell error = %v", errHandle)
	}
	success := response.GetShellResult().GetSuccess()
	if success == nil || !strings.Contains(success.GetStdout(), "shell-ok") {
		t.Fatalf("shell result = %#v", response.GetShellResult())
	}
}

func TestHandleWorkspaceExecStream(t *testing.T) {
	command := "printf stream-ok"
	if runtime.GOOS == "windows" {
		command = "Write-Output stream-ok"
	}
	request := &cursorproto.ExecServerMessage{Id: 17, Message: &cursorproto.ExecServerMessage_ShellStreamArgs{ShellStreamArgs: &cursorproto.ShellArgs{Command: command, WorkingDirectory: "."}}}
	var events []*cursorproto.ExecClientMessage
	handled, errHandle := HandleWorkspaceExecStream(context.Background(), t.TempDir(), request, func(event *cursorproto.ExecClientMessage) error {
		events = append(events, event)
		return nil
	})
	if errHandle != nil || !handled {
		t.Fatalf("HandleWorkspaceExecStream() = %v, %v", handled, errHandle)
	}
	if len(events) < 3 || events[0].GetShellStream().GetStart() == nil || events[len(events)-1].GetShellStream().GetExit() == nil {
		t.Fatalf("stream events = %#v", events)
	}
	var output strings.Builder
	for _, event := range events {
		output.WriteString(event.GetShellStream().GetStdout().GetData())
		output.WriteString(event.GetShellStream().GetStderr().GetData())
	}
	if !strings.Contains(output.String(), "stream-ok") {
		t.Fatalf("stream output = %q", output.String())
	}
}
