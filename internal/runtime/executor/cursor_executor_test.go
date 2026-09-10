package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
	cursorconnect "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursor"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"google.golang.org/protobuf/proto"
)

func TestBuildCursorRunRequestUsesResponsesInput(t *testing.T) {
	request := buildCursorRunRequest([]byte(`{"instructions":"be concise","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],"reasoning":{"effort":"xhigh"}}`), "cursor-model", "conversation")
	if got := request.GetConversationId(); got != "conversation" {
		t.Fatalf("conversation ID = %q", got)
	}
	if request.GetModelDetails().GetModelId() != "cursor-model" || !request.GetRequestedModel().GetMaxMode() {
		t.Fatalf("model request = %#v", request)
	}
	if got := request.GetAction().GetUserMessageAction().GetUserMessage().GetText(); got != "hello" {
		t.Fatalf("user message = %q", got)
	}
	wire, err := proto.Marshal(request)
	if err != nil {
		t.Fatalf("marshal run request: %v", err)
	}
	var decoded cursorproto.AgentRunRequest
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal run request: %v", err)
	}
	if len(decoded.GetConversationState().GetRootPromptMessagesJson()) != 1 {
		t.Fatalf("root prompt messages = %d", len(decoded.GetConversationState().GetRootPromptMessagesJson()))
	}
}

func TestStoreCursorRootPromptBlobsReplacesInlineData(t *testing.T) {
	request := buildCursorRunRequest([]byte(`{"input":"hello"}`), "default", "conversation")
	original := append([]byte(nil), request.GetConversationState().GetRootPromptMessagesJson()[0]...)
	store := cursorconnect.NewBlobStorePool(1).ForSession("conversation")
	if err := storeCursorRootPromptBlobs(request, store); err != nil {
		t.Fatal(err)
	}
	id := request.GetConversationState().GetRootPromptMessagesJson()[0]
	if len(id) != sha256.Size || bytes.Equal(id, original) {
		t.Fatalf("root prompt blob id = %x", id)
	}
	if got := store.Get(id); !bytes.Equal(got, original) {
		t.Fatalf("stored blob = %q, want %q", got, original)
	}
}

func TestCursorResponseIDUsesStableSessionIdentity(t *testing.T) {
	req := cliproxyexecutor.Request{Model: "cursor-model", Payload: []byte(`{"input":"hello"}`)}
	a := cursorResponseID(req, cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "session-a"}})
	b := cursorResponseID(req, cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "session-a"}})
	c := cursorResponseID(req, cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "session-b"}})
	if a != b || a == c {
		t.Fatalf("response IDs = %q, %q, %q", a, b, c)
	}
}

func TestCursorDuplexWriterSendsAgentClientMessage(t *testing.T) {
	reader, writer := io.Pipe()
	duplex := &cursorDuplexWriter{writer: writer}
	result := make(chan *cursorproto.AgentClientMessage, 1)
	errs := make(chan error, 1)
	go func() {
		frame, errRead := cursorconnect.ReadFrame(reader)
		if errRead != nil {
			errs <- errRead
			return
		}
		var message cursorproto.AgentClientMessage
		if errDecode := proto.Unmarshal(frame.Payload, &message); errDecode != nil {
			errs <- errDecode
			return
		}
		result <- &message
	}()

	if errSend := duplex.Send(&cursorproto.AgentClientMessage{Message: &cursorproto.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &cursorproto.ClientHeartbeat{}}}); errSend != nil {
		t.Fatalf("Send() error = %v", errSend)
	}
	select {
	case errRead := <-errs:
		t.Fatalf("read frame: %v", errRead)
	case message := <-result:
		if message.GetClientHeartbeat() == nil {
			t.Fatalf("message = %#v", message)
		}
	}
	_ = reader.Close()
	_ = writer.Close()
}

func TestCursorCompletedResponseIsResponsesObject(t *testing.T) {
	payload := cursorCompletedResponse("cursor-model", "resp_cursor_test", "answer", "thinking")
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if value["object"] != "response" || value["status"] != "completed" {
		t.Fatalf("response = %s", payload)
	}
	out := sdktranslator.TranslateNonStream(t.Context(), sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAIResponse, "cursor-model", nil, nil, payload, nil)
	if string(out) != string(payload) {
		t.Fatalf("Responses passthrough changed payload: %s", out)
	}
}

func TestConsumeCursorStreamRepliesWithToolResult(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "cursor_executor.go"), []byte("package executor\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executor := NewCursorExecutor(&config.Config{CursorTools: config.CursorToolsConfig{Workspace: workspace, Files: true}})
	var serverStream bytes.Buffer
	execPayload, errMarshal := proto.Marshal(&cursorproto.AgentServerMessage{Message: &cursorproto.AgentServerMessage_ExecServerMessage{ExecServerMessage: &cursorproto.ExecServerMessage{Id: 21, Message: &cursorproto.ExecServerMessage_ReadArgs{ReadArgs: &cursorproto.ReadArgs{Path: "cursor_executor.go", Limit: proto.Uint32(1)}}}}})
	if errMarshal != nil {
		t.Fatalf("marshal exec message: %v", errMarshal)
	}
	if errFrame := cursorconnect.WriteFrame(&serverStream, execPayload); errFrame != nil {
		t.Fatalf("write exec frame: %v", errFrame)
	}
	endPayload, errMarshal := proto.Marshal(&cursorproto.AgentServerMessage{Message: &cursorproto.AgentServerMessage_InteractionUpdate{InteractionUpdate: &cursorproto.InteractionUpdate{Message: &cursorproto.InteractionUpdate_TurnEnded{TurnEnded: &cursorproto.TurnEndedUpdate{}}}}})
	if errMarshal != nil {
		t.Fatalf("marshal turn ended message: %v", errMarshal)
	}
	if errFrame := cursorconnect.WriteFrame(&serverStream, endPayload); errFrame != nil {
		t.Fatalf("write end frame: %v", errFrame)
	}
	if errFrame := cursorconnect.WriteEndStream(&serverStream, []byte(`{"code":"ok"}`)); errFrame != nil {
		t.Fatalf("write stream end: %v", errFrame)
	}

	reader, writer := io.Pipe()
	duplex := &cursorDuplexWriter{writer: writer}
	events := make(chan cursorEvent)
	go executor.consumeCursorStream(context.Background(), &http.Response{Body: io.NopCloser(bytes.NewReader(serverStream.Bytes()))}, duplex, cursorconnect.NewBlobStorePool(1).ForSession("test"), "", nil, events, nil)

	frame, errFrame := cursorconnect.ReadFrame(reader)
	if errFrame != nil {
		t.Fatalf("read client frame: %v", errFrame)
	}
	var reply cursorproto.AgentClientMessage
	if errDecode := proto.Unmarshal(frame.Payload, &reply); errDecode != nil {
		t.Fatalf("decode client reply: %v", errDecode)
	}
	toolResult := reply.GetExecClientMessage()
	if toolResult == nil || toolResult.GetId() != 21 || toolResult.GetReadResult().GetSuccess() == nil {
		t.Fatalf("tool reply = %#v", reply)
	}
	_ = reader.Close()
	for event := range events {
		if event.err != nil {
			t.Fatalf("consume stream: %v", event.err)
		}
	}
}

func TestCursorHandleExecClosesStreamingShell(t *testing.T) {
	command := "printf stream-ok"
	if runtime.GOOS == "windows" {
		command = "Write-Output stream-ok"
	}
	reader, writer := io.Pipe()
	duplex := &cursorDuplexWriter{writer: writer}
	request := &cursorproto.ExecServerMessage{Id: 22, Message: &cursorproto.ExecServerMessage_ShellStreamArgs{ShellStreamArgs: &cursorproto.ShellArgs{Command: command, WorkingDirectory: "."}}}
	errs := make(chan error, 1)
	executor := NewCursorExecutor(&config.Config{CursorTools: config.CursorToolsConfig{Workspace: t.TempDir(), Shell: true}})
	defer func() { _ = executor.shells.CloseAll() }()
	go func() { errs <- executor.handleCursorExec(t.Context(), duplex, request, "owner", nil) }()

	var start, stdout, exit, closeControl bool
	for frames := 0; frames < 8 && !closeControl; frames++ {
		frame, errFrame := cursorconnect.ReadFrame(reader)
		if errFrame != nil {
			t.Fatalf("read stream frame: %v", errFrame)
		}
		var reply cursorproto.AgentClientMessage
		if errDecode := proto.Unmarshal(frame.Payload, &reply); errDecode != nil {
			t.Fatalf("decode stream reply: %v", errDecode)
		}
		if stream := reply.GetExecClientMessage().GetShellStream(); stream != nil {
			start = start || stream.GetStart() != nil
			stdout = stdout || stream.GetStdout() != nil
			exit = exit || stream.GetExit() != nil
		}
		closeControl = reply.GetExecClientControlMessage().GetStreamClose() != nil
	}
	if !closeControl {
		t.Fatal("stream did not send a close control message")
	}
	_ = reader.Close()
	if errHandle := <-errs; errHandle != nil {
		t.Fatalf("cursorHandleExec() error = %v", errHandle)
	}
	if !start || !stdout || !exit {
		t.Fatalf("stream state = start:%t stdout:%t exit:%t", start, stdout, exit)
	}
}
