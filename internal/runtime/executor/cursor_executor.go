package executor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cursoragent "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cursorconnect "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/proto"
)

const (
	cursorDefaultBaseURL = "https://api2.cursor.sh"
	cursorRunPath        = "/agent.v1.AgentService/Run"
	cursorMaxMCPContent  = 1 << 20
)

// CursorExecutor executes requests through Cursor's native AgentService Connect endpoint.
type CursorExecutor struct {
	cfg        *config.Config
	tools      config.CursorToolsConfig
	runs       *cursorconnect.RunRegistry
	mcpServers map[string]config.CursorMCPServer
	mcpOwners  map[string]*cursorconnect.MCPStdioManager
	blobStores *cursorconnect.BlobStorePool
	shells     *cursorconnect.BackgroundShellManager
	mcp        *cursorconnect.MCPStdioManager
	shellMu    sync.Mutex
	closed     bool
	execShells map[string]map[uint32]uint32
}

func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	mcpServers := map[string]config.CursorMCPServer(nil)
	tools := config.CursorToolsConfig{}
	if cfg != nil {
		tools = cfg.CursorTools
		if tools.MCP {
			mcpServers = cfg.CursorMCP.Servers
		}
	}
	return &CursorExecutor{
		cfg:        cfg,
		tools:      tools,
		runs:       cursorconnect.NewRunRegistry(),
		mcpServers: mcpServers,
		mcpOwners:  make(map[string]*cursorconnect.MCPStdioManager),
		blobStores: cursorconnect.NewBlobStorePool(0),
		shells:     cursorconnect.NewBackgroundShellManager(),
		mcp:        cursorconnect.NewMCPStdioManager(mcpServers),
		execShells: make(map[string]map[uint32]uint32),
	}
}

func (e *CursorExecutor) UsesConfig(cfg *config.Config) bool {
	e.shellMu.Lock()
	defer e.shellMu.Unlock()
	return !e.closed && e.cfg == cfg
}

func (e *CursorExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.runs == nil {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		if err := e.Close(); err != nil {
			helps.LogWithRequestID(context.Background()).Debugf("cursor executor: close all sessions: %v", err)
		}
		return
	}
	e.runs.CloseSession(sessionID)
}

func (e *CursorExecutor) Close() error {
	if e == nil {
		return nil
	}
	e.shellMu.Lock()
	if e.closed {
		e.shellMu.Unlock()
		return nil
	}
	e.closed = true
	managers := make([]*cursorconnect.MCPStdioManager, 0, len(e.mcpOwners)+1)
	for _, manager := range e.mcpOwners {
		managers = append(managers, manager)
	}
	e.mcpOwners = nil
	e.shellMu.Unlock()
	if e.runs != nil {
		e.runs.Close()
	}
	var closeErr error
	if e.shells != nil {
		closeErr = e.shells.CloseAll()
	}
	for _, manager := range managers {
		closeErr = errors.Join(closeErr, manager.CloseAll())
	}
	if e.mcp != nil {
		closeErr = errors.Join(closeErr, e.mcp.CloseAll())
	}
	return closeErr
}

func (e *CursorExecutor) closeCursorOwner(owner string) {
	e.shellMu.Lock()
	manager := e.mcpOwners[owner]
	delete(e.mcpOwners, owner)
	delete(e.execShells, owner)
	e.shellMu.Unlock()
	if err := manager.CloseAll(); err != nil {
		helps.LogWithRequestID(context.Background()).Debugf("cursor executor: close MCP: %v", err)
	}
	if err := e.shells.CloseOwner(owner); err != nil {
		helps.LogWithRequestID(context.Background()).Debugf("cursor executor: close shells: %v", err)
	}
}

func (e *CursorExecutor) cursorMCPExecutor(owner string) *CursorExecutor {
	e.shellMu.Lock()
	defer e.shellMu.Unlock()
	manager := e.mcpOwners[owner]
	if manager == nil {
		manager = cursorconnect.NewMCPStdioManager(e.mcpServers)
		if e.mcpOwners == nil {
			e.mcpOwners = make(map[string]*cursorconnect.MCPStdioManager)
		}
		e.mcpOwners[owner] = manager
	}
	return &CursorExecutor{mcp: manager, tools: e.tools}
}

func (e *CursorExecutor) Identifier() string { return "cursor" }

// DiscoverModels fetches the models entitled for this Cursor credential.
func (e *CursorExecutor) DiscoverModels(ctx context.Context, auth *cliproxyauth.Auth) ([]cursoragent.Model, error) {
	accessToken, baseURL := cursorCredentials(auth)
	return cursoragent.FetchUsableModels(ctx, e.httpClient(auth), baseURL, accessToken)
}

func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	stream, err := e.execute(ctx, auth, req, opts)
	if err != nil {
		return resp, err
	}
	defer stream.cancel()
	var text, reasoning strings.Builder
	for event := range stream.events {
		if event.err != nil {
			return resp, event.err
		}
		text.WriteString(event.text)
		reasoning.WriteString(event.reasoning)
	}
	if err := stream.ctx.Err(); err != nil {
		return resp, err
	}
	payload := cursorCompletedResponse(req.Model, stream.responseID, text.String(), reasoning.String())
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	var param any
	payload = []byte(`{"type":"response.completed","response":` + string(payload) + `}`)
	payload = sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatCodex, responseFormat, req.Model, req.Payload, nil, payload, &param)
	return cliproxyexecutor.Response{Payload: payload, Headers: stream.headers}, nil
}

func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	stream, err := e.execute(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer stream.cancel()
		responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
		var param any
		emit := func(payload []byte) bool {
			chunks := sdktranslator.TranslateStream(ctx, sdktranslator.FormatCodex, responseFormat, req.Model, req.Payload, nil, payload, &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		if !emit(cursorStreamEvent("response.created", stream.responseID, req.Model, "")) ||
			!emit(cursorStreamEvent("response.in_progress", stream.responseID, req.Model, "")) {
			return
		}
		for event := range stream.events {
			if event.err != nil {
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: event.err}:
				case <-ctx.Done():
				}
				return
			}
			if event.reasoning != "" && !emit(cursorReasoningDelta(stream.responseID, event.reasoning)) {
				return
			}
			if event.text != "" && !emit(cursorTextDelta(stream.responseID, event.text)) {
				return
			}
		}
		if err := stream.ctx.Err(); err != nil {
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: err}:
			case <-ctx.Done():
			}
			return
		}
		_ = emit(cursorStreamEvent("response.completed", stream.responseID, req.Model, ""))
	}()
	return &cliproxyexecutor.StreamResult{Headers: stream.headers, Chunks: out}, nil
}

func (e *CursorExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, requestScopedError{msg: "cursor executor: token counting is not supported"}
}

func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("cursor executor: request is nil")
	}
	apiKey, _ := cursorCredentials(auth)
	if apiKey == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "cursor executor: missing access token"}
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return e.httpClient(auth).Do(req)
}

type cursorStream struct {
	headers    http.Header
	responseID string
	events     <-chan cursorEvent
	ctx        context.Context
	cancel     context.CancelFunc
}

type cursorEvent struct {
	text      string
	reasoning string
	err       error
}

func (e *CursorExecutor) execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cursorStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	apiKey, baseURL := cursorCredentials(auth)
	if apiKey == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "cursor executor: missing access token"}
	}
	if baseURL == "" {
		baseURL = cursorDefaultBaseURL
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if baseModel == "" {
		baseModel = req.Model
	}
	payload, err := cursorCanonicalRequest(req, opts)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	owner := cursorShellOwner(auth, req, opts)
	ctx, finish, err := e.runs.Start(ctx, owner, cursorConversationID(req, opts))
	if err != nil {
		cancel()
		return nil, requestScopedError{msg: err.Error()}
	}
	handedOff := false
	defer func() {
		if !handedOff {
			cancel()
			finish()
		}
	}()
	runRequest := buildCursorRunRequest(payload, baseModel, cursorConversationID(req, opts))
	bodyReader, bodyWriter := io.Pipe()
	duplex := &cursorDuplexWriter{writer: bodyWriter}
	url := strings.TrimRight(baseURL, "/") + cursorRunPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bodyReader)
	if err != nil {
		_ = bodyReader.Close()
		_ = bodyWriter.Close()
		return nil, fmt.Errorf("cursor executor: create Run request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/connect+proto")
	httpReq.Header.Set("Content-Type", "application/connect+proto")
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	httpReq.Header.Set("TE", "trailers")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("X-Ghost-Mode", "true")
	httpReq.Header.Set("X-Cursor-Client-Type", "cli")
	httpReq.Header.Set("X-Cursor-Client-Version", cursoragent.DefaultClientVersion)
	for key, value := range cursorHeaders(auth) {
		if httpReq.Header.Get(key) == "" {
			httpReq.Header.Set(key, value)
		}
	}

	responseCh := make(chan struct {
		response *http.Response
		err      error
	}, 1)
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	go func() {
		response, errDo := reporter.TrackHTTPClientRoundTripOnly(e.httpClient(auth)).Do(httpReq)
		responseCh <- struct {
			response *http.Response
			err      error
		}{response: response, err: errDo}
	}()
	initial := &cursorproto.AgentClientMessage{Message: &cursorproto.AgentClientMessage_RunRequest{RunRequest: runRequest}}
	if errSend := duplex.Send(initial); errSend != nil {
		duplex.Close()
		return nil, fmt.Errorf("cursor executor: send Run request: %w", errSend)
	}
	select {
	case responseResult := <-responseCh:
		if responseResult.err != nil {
			duplex.Close()
			return nil, responseResult.err
		}
		httpResp := responseResult.response
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			defer httpResp.Body.Close()
			duplex.Close()
			data, _ := io.ReadAll(httpResp.Body)
			return nil, newCursorStatusErr(httpResp.StatusCode, data)
		}
		out := make(chan cursorEvent)
		blobs := e.blobStores.ForSession(owner)
		handedOff = true
		go e.consumeCursorStream(ctx, httpResp, duplex, blobs, owner, opts.ExecutionLifecycle, out, func() {
			e.closeCursorOwner(owner)
			finish()
		})
		return &cursorStream{headers: httpResp.Header.Clone(), responseID: cursorResponseID(req, opts), events: out, ctx: ctx, cancel: cancel}, nil
	case <-ctx.Done():
		duplex.Close()
		return nil, ctx.Err()
	}
}

type cursorDuplexWriter struct {
	mu     sync.Mutex
	writer *io.PipeWriter
	closed bool
}

func (w *cursorDuplexWriter) Send(message *cursorproto.AgentClientMessage) error {
	if w == nil || message == nil {
		return fmt.Errorf("cursor executor: client message is nil")
	}
	payload, errMarshal := proto.Marshal(message)
	if errMarshal != nil {
		return fmt.Errorf("cursor executor: marshal client message: %w", errMarshal)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return io.ErrClosedPipe
	}
	if errWrite := cursorconnect.WriteFrame(w.writer, payload); errWrite != nil {
		return fmt.Errorf("cursor executor: write client frame: %w", errWrite)
	}
	return nil
}

func (w *cursorDuplexWriter) Close() {
	if w == nil {
		return
	}
	// Close the pipe before locking so a blocked Send can release the mutex.
	_ = w.writer.Close()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
}

func (e *CursorExecutor) consumeCursorStream(ctx context.Context, response *http.Response, duplex *cursorDuplexWriter, blobs *cursorconnect.BlobStore, shellOwner string, binder cursorconnect.ShellResourceBinder, out chan<- cursorEvent, onDone func()) {
	defer close(out)
	if onDone != nil {
		defer onDone()
	}
	defer duplex.Close()
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			helps.LogWithRequestID(ctx).Debugf("cursor executor: close response body: %v", errClose)
		}
	}()

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = duplex.Send(&cursorproto.AgentClientMessage{Message: &cursorproto.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &cursorproto.ClientHeartbeat{}}})
			}
		}
	}()

	for {
		frame, errRead := cursorconnect.ReadFrame(response.Body)
		if errRead == io.EOF {
			errRead = io.ErrUnexpectedEOF
		}
		if errRead != nil {
			if ctx.Err() == nil {
				cursorEmit(ctx, out, cursorEvent{err: fmt.Errorf("cursor executor: read Connect frame: %w", errRead)})
			}
			return
		}
		if frame.EndStream {
			if errEnd := cursorconnect.EndStreamError(frame.Payload); errEnd != nil {
				cursorEmit(ctx, out, cursorEvent{err: errEnd})
			}
			return
		}
		var server cursorproto.AgentServerMessage
		if errDecode := proto.Unmarshal(frame.Payload, &server); errDecode != nil {
			cursorEmit(ctx, out, cursorEvent{err: fmt.Errorf("cursor executor: decode server message: %w", errDecode)})
			return
		}
		if control := server.GetExecServerControlMessage(); control != nil {
			if abort := control.GetAbort(); abort != nil {
				if errAbort := e.abortCursorExec(shellOwner, abort.GetId()); errAbort != nil {
					cursorEmit(ctx, out, cursorEvent{err: errAbort})
					return
				}
			}
			continue
		}
		if exec := server.GetExecServerMessage(); exec != nil {
			if errSend := e.handleCursorExec(ctx, duplex, exec, shellOwner, binder); errSend != nil {
				cursorEmit(ctx, out, cursorEvent{err: errSend})
				return
			}
			continue
		}
		if kv := server.GetKvServerMessage(); kv != nil {
			if errSend := cursorHandleKV(duplex, blobs, kv); errSend != nil {
				cursorEmit(ctx, out, cursorEvent{err: errSend})
				return
			}
			continue
		}
		if interaction := server.GetInteractionQuery(); interaction != nil {
			if errSend := cursorRejectInteraction(duplex, interaction); errSend != nil {
				cursorEmit(ctx, out, cursorEvent{err: errSend})
				return
			}
			continue
		}
		update := server.GetInteractionUpdate()
		if update == nil {
			continue
		}
		if update.GetTurnEnded() != nil {
			return
		}
		event := cursorEvent{}
		if text := update.GetTextDelta(); text != nil {
			event.text = text.GetText()
		}
		if thought := update.GetThinkingDelta(); thought != nil {
			event.reasoning = thought.GetText()
		}
		if event.text != "" || event.reasoning != "" {
			if !cursorEmit(ctx, out, event) {
				return
			}
		}
	}
}

func (e *CursorExecutor) handleCursorExec(ctx context.Context, duplex *cursorDuplexWriter, request *cursorproto.ExecServerMessage, shellOwner string, binder cursorconnect.ShellResourceBinder) error {
	workspace, errWorkspace := cursorconnect.ToolWorkspace(e.tools, request)
	if errWorkspace != nil {
		return cursorRejectExec(duplex, request.GetId(), errWorkspace.Error())
	}
	if e.shells == nil {
		e.shells = cursorconnect.NewBackgroundShellManager()
	}
	switch {
	case request.GetBackgroundShellSpawnArgs() != nil:
		result := e.shells.Spawn(ctx, workspace, shellOwner, request.GetBackgroundShellSpawnArgs(), binder)
		if success := result.GetSuccess(); success != nil {
			e.rememberShell(shellOwner, request.GetId(), success.GetShellId())
		}
		return cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId(), Message: &cursorproto.ExecClientMessage_BackgroundShellSpawnResult{BackgroundShellSpawnResult: result}})
	case request.GetWriteShellStdinArgs() != nil:
		result := e.shells.WriteStdin(ctx, shellOwner, request.GetWriteShellStdinArgs())
		return cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId(), Message: &cursorproto.ExecClientMessage_WriteShellStdinResult{WriteShellStdinResult: result}})
	case request.GetForceBackgroundShellArgs() != nil:
		foreground, shellID, promoted := e.shells.PromoteForeground(shellOwner, request.GetForceBackgroundShellArgs().GetToolCallId())
		if !promoted {
			result := &cursorproto.ForceBackgroundShellResult{Status: cursorproto.ForceBackgroundShellStatus_FORCE_BACKGROUND_SHELL_STATUS_NOT_FOUND}
			return cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId(), Message: &cursorproto.ExecClientMessage_ForceBackgroundShellResult{ForceBackgroundShellResult: result}})
		}
		e.rememberShell(shellOwner, foreground.ExecID(), shellID)
		result := &cursorproto.ForceBackgroundShellResult{Status: cursorproto.ForceBackgroundShellStatus_FORCE_BACKGROUND_SHELL_STATUS_ACCEPTED}
		if errResult := cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId(), Message: &cursorproto.ExecClientMessage_ForceBackgroundShellResult{ForceBackgroundShellResult: result}}); errResult != nil {
			return errResult
		}
		command, directory, pid := foreground.BackgroundDetails()
		reason := cursorproto.ShellBackgroundReason_SHELL_BACKGROUND_REASON_USER_REQUEST
		if errEvent := cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: foreground.ExecID(), ExecId: foreground.ExecKey(), Message: &cursorproto.ExecClientMessage_ShellStream{ShellStream: &cursorproto.ShellStream{Event: &cursorproto.ShellStream_Backgrounded{Backgrounded: &cursorproto.ShellStreamBackgrounded{ShellId: shellID, Command: command, WorkingDirectory: directory, Pid: &pid, Reason: &reason}}}}}); errEvent != nil {
			return errEvent
		}
		return cursorCloseExecStream(duplex, foreground.ExecID())
	case request.GetMcpStateExecArgs() != nil:
		result := e.cursorMCPExecutor(shellOwner).handleCursorMCPState(ctx, request.GetMcpStateExecArgs())
		return cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId(), Message: &cursorproto.ExecClientMessage_McpStateExecResult{McpStateExecResult: result}})
	case request.GetMcpArgs() != nil:
		result := e.cursorMCPExecutor(shellOwner).handleCursorMCPCall(ctx, request.GetMcpArgs())
		return cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId(), Message: &cursorproto.ExecClientMessage_McpResult{McpResult: result}})
	case request.GetListMcpResourcesExecArgs() != nil:
		result := e.cursorMCPExecutor(shellOwner).handleCursorMCPResourceList(ctx, request.GetListMcpResourcesExecArgs())
		return cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId(), Message: &cursorproto.ExecClientMessage_ListMcpResourcesExecResult{ListMcpResourcesExecResult: result}})
	case request.GetReadMcpResourceExecArgs() != nil:
		result := e.cursorMCPExecutor(shellOwner).handleCursorMCPResourceRead(ctx, workspace, request.GetReadMcpResourceExecArgs())
		return cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId(), Message: &cursorproto.ExecClientMessage_ReadMcpResourceExecResult{ReadMcpResourceExecResult: result}})
	case request.GetRequestContextArgs() != nil:
		result := cursorconnect.WorkspaceRequestContext(workspace)
		return cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId(), Message: &cursorproto.ExecClientMessage_RequestContextResult{RequestContextResult: result}})
	case request.GetShellStreamArgs() != nil:
		foreground, errStart := e.shells.StartForeground(ctx, workspace, shellOwner, request.GetId(), request.GetExecId(), request.GetShellStreamArgs())
		if errStart != nil {
			return cursorRejectExec(duplex, request.GetId(), "CLIProxyAPI cannot start Cursor shell stream")
		}
		go e.runCursorForegroundShell(ctx, duplex, request.GetId(), foreground)
		return nil
	}
	streamed, errStream := cursorconnect.HandleWorkspaceExecStream(ctx, workspace, request, func(result *cursorproto.ExecClientMessage) error {
		return cursorSendExecResult(duplex, result)
	})
	if errStream != nil {
		return fmt.Errorf("cursor executor: stream exec %d: %w", request.GetId(), errStream)
	}
	if streamed {
		return cursorCloseExecStream(duplex, request.GetId())
	}
	result, errHandle := cursorconnect.HandleWorkspaceExec(ctx, workspace, request)
	if errHandle != nil {
		return cursorRejectExec(duplex, request.GetId(), "CLIProxyAPI Cursor tool is not supported")
	}
	return cursorSendExecResult(duplex, result)
}

func cursorSendExecResult(duplex *cursorDuplexWriter, result *cursorproto.ExecClientMessage) error {
	message := &cursorproto.AgentClientMessage{Message: &cursorproto.AgentClientMessage_ExecClientMessage{ExecClientMessage: result}}
	if errSend := duplex.Send(message); errSend != nil {
		return fmt.Errorf("cursor executor: reply to exec %d: %w", result.GetId(), errSend)
	}
	return nil
}

func (e *CursorExecutor) handleCursorMCPState(ctx context.Context, args *cursorproto.McpStateExecArgs) *cursorproto.McpStateExecResult {
	if e == nil || e.mcp == nil {
		return &cursorproto.McpStateExecResult{Result: &cursorproto.McpStateExecResult_Error{Error: &cursorproto.McpStateError{Error: "Cursor MCP is not configured"}}}
	}
	requested := make(map[string]struct{})
	if args != nil {
		for _, id := range args.GetServerIdentifiers() {
			requested[id] = struct{}{}
		}
	}
	servers := make([]*cursorproto.McpStateServer, 0)
	for _, id := range e.mcp.ServerIDs() {
		if len(requested) > 0 {
			if _, ok := requested[id]; !ok {
				continue
			}
		}
		response, errCall := e.mcp.Call(ctx, id, "tools/list", map[string]any{})
		status := "ready"
		tools := make([]*cursorproto.McpToolDefinition, 0)
		if errCall == nil {
			var list struct {
				Tools []struct {
					Name        string          `json:"name"`
					Description string          `json:"description"`
					InputSchema json.RawMessage `json:"inputSchema"`
				} `json:"tools"`
			}
			if json.Unmarshal(response, &list) == nil {
				for _, tool := range list.Tools {
					tools = append(tools, &cursorproto.McpToolDefinition{Name: tool.Name, ProviderIdentifier: id, ToolName: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
				}
			} else {
				status = "failed"
			}
		} else {
			status = "failed"
		}
		servers = append(servers, &cursorproto.McpStateServer{ServerName: id, ServerIdentifier: id, Tools: tools, Status: &status})
	}
	return &cursorproto.McpStateExecResult{Result: &cursorproto.McpStateExecResult_Success{Success: &cursorproto.McpStateSuccess{Servers: servers}}}
}

func (e *CursorExecutor) handleCursorMCPResourceList(ctx context.Context, args *cursorproto.ListMcpResourcesExecArgs) *cursorproto.ListMcpResourcesExecResult {
	if e == nil || e.mcp == nil {
		return &cursorproto.ListMcpResourcesExecResult{Result: &cursorproto.ListMcpResourcesExecResult_Error{Error: &cursorproto.ListMcpResourcesError{Error: "Cursor MCP is not configured"}}}
	}
	serverID := ""
	if args != nil {
		serverID = strings.TrimSpace(args.GetServer())
	}
	serverIDs := e.mcp.ServerIDs()
	if serverID != "" {
		found := false
		for _, id := range serverIDs {
			if id == serverID {
				found = true
				break
			}
		}
		if !found {
			return &cursorproto.ListMcpResourcesExecResult{Result: &cursorproto.ListMcpResourcesExecResult_Rejected{Rejected: &cursorproto.ListMcpResourcesRejected{Reason: "Cursor MCP server is not configured"}}}
		}
		serverIDs = []string{serverID}
	}
	resources := make([]*cursorproto.CursorListMcpResourcesExecResult_McpResource, 0)
	for _, id := range serverIDs {
		response, errCall := e.mcp.Call(ctx, id, "resources/list", map[string]any{})
		if errCall != nil {
			return &cursorproto.ListMcpResourcesExecResult{Result: &cursorproto.ListMcpResourcesExecResult_Error{Error: &cursorproto.ListMcpResourcesError{Error: "Cursor MCP resource listing failed"}}}
		}
		var list struct {
			Resources []struct {
				URI         string            `json:"uri"`
				Name        string            `json:"name"`
				Description string            `json:"description"`
				MimeType    string            `json:"mimeType"`
				Annotations map[string]string `json:"annotations"`
			} `json:"resources"`
		}
		if errDecode := json.Unmarshal(response, &list); errDecode != nil {
			return &cursorproto.ListMcpResourcesExecResult{Result: &cursorproto.ListMcpResourcesExecResult_Error{Error: &cursorproto.ListMcpResourcesError{Error: "Cursor MCP resource listing is invalid"}}}
		}
		for _, resource := range list.Resources {
			if strings.TrimSpace(resource.URI) == "" {
				continue
			}
			name, description, mime := resource.Name, resource.Description, resource.MimeType
			resources = append(resources, &cursorproto.CursorListMcpResourcesExecResult_McpResource{Uri: resource.URI, Name: optionalString(name), Description: optionalString(description), MimeType: optionalString(mime), Server: id, Annotations: resource.Annotations})
		}
	}
	sort.Slice(resources, func(i, j int) bool {
		if resources[i].GetServer() == resources[j].GetServer() {
			return resources[i].GetUri() < resources[j].GetUri()
		}
		return resources[i].GetServer() < resources[j].GetServer()
	})
	return &cursorproto.ListMcpResourcesExecResult{Result: &cursorproto.ListMcpResourcesExecResult_Success{Success: &cursorproto.ListMcpResourcesSuccess{Resources: resources}}}
}

func (e *CursorExecutor) handleCursorMCPResourceRead(ctx context.Context, workspace string, args *cursorproto.ReadMcpResourceExecArgs) *cursorproto.ReadMcpResourceExecResult {
	if e == nil || e.mcp == nil || args == nil {
		return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Error{Error: &cursorproto.ReadMcpResourceError{Uri: args.GetUri(), Error: "Cursor MCP is not configured"}}}
	}
	serverID := strings.TrimSpace(args.GetServer())
	uri := strings.TrimSpace(args.GetUri())
	if serverID == "" {
		return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Rejected{Rejected: &cursorproto.ReadMcpResourceRejected{Uri: uri, Reason: "Cursor MCP server is required"}}}
	}
	if uri == "" {
		return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Rejected{Rejected: &cursorproto.ReadMcpResourceRejected{Uri: uri, Reason: "Cursor MCP resource URI is required"}}}
	}
	response, errCall := e.mcp.Call(ctx, serverID, "resources/read", map[string]any{"uri": uri})
	if errCall != nil {
		return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Error{Error: &cursorproto.ReadMcpResourceError{Uri: uri, Error: "Cursor MCP resource read failed"}}}
	}
	var result struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		} `json:"contents"`
	}
	if errDecode := json.Unmarshal(response, &result); errDecode != nil || len(result.Contents) == 0 {
		return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Error{Error: &cursorproto.ReadMcpResourceError{Uri: uri, Error: "Cursor MCP resource result is invalid"}}}
	}
	content := result.Contents[0]
	resourceURI := content.URI
	if resourceURI == "" {
		resourceURI = uri
	}
	success := &cursorproto.ReadMcpResourceSuccess{Uri: resourceURI, MimeType: optionalString(content.MimeType)}
	if content.Blob != "" {
		data, errDecode := base64.StdEncoding.DecodeString(content.Blob)
		if errDecode != nil || len(data) > cursorMaxMCPContent {
			return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Error{Error: &cursorproto.ReadMcpResourceError{Uri: uri, Error: "Cursor MCP resource blob is invalid"}}}
		}
		success.Content = &cursorproto.ReadMcpResourceSuccess_Blob{Blob: data}
	} else {
		if len(content.Text) > cursorMaxMCPContent {
			return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Error{Error: &cursorproto.ReadMcpResourceError{Uri: uri, Error: "Cursor MCP resource text exceeds byte limit"}}}
		}
		if args.GetDownloadPath() != "" {
			path, errPath := cursorResourceDownloadPath(workspace, args.GetDownloadPath())
			if errPath != nil {
				return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Rejected{Rejected: &cursorproto.ReadMcpResourceRejected{Uri: uri, Reason: errPath.Error()}}}
			}
			if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
				return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Error{Error: &cursorproto.ReadMcpResourceError{Uri: uri, Error: "Cursor MCP resource download failed"}}}
			}
			if errWrite := os.WriteFile(path, []byte(content.Text), 0o600); errWrite != nil {
				return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Error{Error: &cursorproto.ReadMcpResourceError{Uri: uri, Error: "Cursor MCP resource download failed"}}}
			}
			downloadPath := path
			success.DownloadPath = &downloadPath
		} else {
			success.Content = &cursorproto.ReadMcpResourceSuccess_Text{Text: content.Text}
		}
	}
	return &cursorproto.ReadMcpResourceExecResult{Result: &cursorproto.ReadMcpResourceExecResult_Success{Success: success}}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func cursorResourceDownloadPath(workspace, requested string) (string, error) {
	return cursorconnect.ResolveWorkspacePath(workspace, requested)
}

func (e *CursorExecutor) handleCursorMCPCall(ctx context.Context, args *cursorproto.McpArgs) *cursorproto.McpResult {
	if args == nil || e == nil || e.mcp == nil {
		return &cursorproto.McpResult{Result: &cursorproto.McpResult_Error{Error: &cursorproto.McpError{Error: "Cursor MCP is not configured"}}}
	}
	serverID := strings.TrimSpace(args.GetServerIdentifier())
	toolName := strings.TrimSpace(args.GetToolName())
	if toolName == "" {
		toolName = strings.TrimSpace(args.GetName())
	}
	if serverID == "" {
		return &cursorproto.McpResult{Result: &cursorproto.McpResult_ServerNotFound{ServerNotFound: &cursorproto.McpServerNotFound{Name: ""}}}
	}
	if toolName == "" || (args.GetName() != "" && args.GetToolName() != "" && args.GetName() != args.GetToolName()) {
		return &cursorproto.McpResult{Result: &cursorproto.McpResult_ToolNotFound{ToolNotFound: &cursorproto.McpToolNotFound{Name: toolName}}}
	}
	arguments := make(map[string]any, len(args.GetArgs()))
	for key, value := range args.GetArgs() {
		var decoded any
		if errDecode := json.Unmarshal(value, &decoded); errDecode != nil {
			decoded = string(value)
		}
		arguments[key] = decoded
	}
	response, errCall := e.mcp.Call(ctx, serverID, "tools/call", map[string]any{"name": toolName, "arguments": arguments})
	if errCall != nil {
		return &cursorproto.McpResult{Result: &cursorproto.McpResult_Error{Error: &cursorproto.McpError{Error: "Cursor MCP tool call failed"}}}
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if errDecode := json.Unmarshal(response, &result); errDecode != nil {
		return &cursorproto.McpResult{Result: &cursorproto.McpResult_Error{Error: &cursorproto.McpError{Error: "Cursor MCP tool result is invalid"}}}
	}
	content := make([]*cursorproto.McpToolResultContentItem, 0, len(result.Content))
	for _, item := range result.Content {
		if item.Type != "text" {
			continue
		}
		content = append(content, &cursorproto.McpToolResultContentItem{Content: &cursorproto.McpToolResultContentItem_Text{Text: &cursorproto.McpTextContent{Text: item.Text}}})
	}
	return &cursorproto.McpResult{Result: &cursorproto.McpResult_Success{Success: &cursorproto.McpSuccess{Content: content, IsError: result.IsError}}}
}

func (e *CursorExecutor) runCursorForegroundShell(ctx context.Context, duplex *cursorDuplexWriter, execID uint32, foreground *cursorconnect.ForegroundShell) {
	promoted, errStream := foreground.StreamForeground(ctx, func(stream *cursorproto.ShellStream) error {
		return cursorSendExecResult(duplex, &cursorproto.ExecClientMessage{Id: execID, ExecId: foreground.ExecKey(), Message: &cursorproto.ExecClientMessage_ShellStream{ShellStream: stream}})
	})
	if errStream != nil {
		_ = cursorRejectExec(duplex, execID, "CLIProxyAPI Cursor shell stream failed")
		return
	}
	if !promoted {
		_ = cursorCloseExecStream(duplex, execID)
	}
}

func (e *CursorExecutor) rememberShell(owner string, execID, shellID uint32) {
	if e == nil || execID == 0 || shellID == 0 {
		return
	}
	e.shellMu.Lock()
	defer e.shellMu.Unlock()
	if e.execShells == nil {
		e.execShells = make(map[string]map[uint32]uint32)
	}
	if e.execShells[owner] == nil {
		e.execShells[owner] = make(map[uint32]uint32)
	}
	e.execShells[owner][execID] = shellID
}

func (e *CursorExecutor) abortCursorExec(owner string, execID uint32) error {
	if e == nil || e.shells == nil || execID == 0 {
		return nil
	}
	e.shellMu.Lock()
	shellID := e.execShells[owner][execID]
	delete(e.execShells[owner], execID)
	if len(e.execShells[owner]) == 0 {
		delete(e.execShells, owner)
	}
	e.shellMu.Unlock()
	if shellID == 0 {
		e.shells.AbortForeground(owner, execID)
		return nil
	}
	return e.shells.CloseShell(owner, shellID)
}

func cursorCloseExecStream(duplex *cursorDuplexWriter, id uint32) error {
	message := &cursorproto.AgentClientMessage{Message: &cursorproto.AgentClientMessage_ExecClientControlMessage{
		ExecClientControlMessage: &cursorproto.ExecClientControlMessage{Message: &cursorproto.ExecClientControlMessage_StreamClose{
			StreamClose: &cursorproto.ExecClientStreamClose{Id: id},
		}},
	}}
	if errSend := duplex.Send(message); errSend != nil {
		return fmt.Errorf("cursor executor: close exec stream %d: %w", id, errSend)
	}
	return nil
}

func cursorRejectExec(duplex *cursorDuplexWriter, id uint32, reason string) error {
	message := &cursorproto.AgentClientMessage{Message: &cursorproto.AgentClientMessage_ExecClientControlMessage{
		ExecClientControlMessage: &cursorproto.ExecClientControlMessage{Message: &cursorproto.ExecClientControlMessage_Throw{
			Throw: &cursorproto.ExecClientThrow{Id: id, Error: reason},
		}},
	}}
	if errSend := duplex.Send(message); errSend != nil {
		return fmt.Errorf("cursor executor: reject unsupported exec %d: %w", id, errSend)
	}
	return nil
}

func cursorHandleKV(duplex *cursorDuplexWriter, blobs *cursorconnect.BlobStore, request *cursorproto.KvServerMessage) error {
	if blobs == nil || request == nil {
		return fmt.Errorf("cursor executor: KV request is nil")
	}
	response := &cursorproto.KvClientMessage{Id: request.GetId()}
	switch {
	case request.GetGetBlobArgs() != nil:
		response.Message = &cursorproto.KvClientMessage_GetBlobResult{GetBlobResult: &cursorproto.GetBlobResult{BlobData: blobs.Get(request.GetGetBlobArgs().GetBlobId())}}
	case request.GetSetBlobArgs() != nil:
		args := request.GetSetBlobArgs()
		result := &cursorproto.SetBlobResult{}
		if errSet := blobs.Set(args.GetBlobId(), args.GetBlobData()); errSet != nil {
			result.Error = &cursorproto.Error{Message: errSet.Error()}
		}
		response.Message = &cursorproto.KvClientMessage_SetBlobResult{SetBlobResult: result}
	default:
		return fmt.Errorf("cursor executor: unsupported KV request %d", request.GetId())
	}
	message := &cursorproto.AgentClientMessage{Message: &cursorproto.AgentClientMessage_KvClientMessage{KvClientMessage: response}}
	if errSend := duplex.Send(message); errSend != nil {
		return fmt.Errorf("cursor executor: reply to KV request %d: %w", request.GetId(), errSend)
	}
	return nil
}

func cursorRejectInteraction(duplex *cursorDuplexWriter, query *cursorproto.InteractionQuery) error {
	if query == nil {
		return fmt.Errorf("cursor executor: interaction query is nil")
	}
	const reason = "CLIProxyAPI Cursor interaction bridge is not implemented"
	response := &cursorproto.InteractionResponse{Id: query.GetId()}
	switch {
	case query.GetWebFetchRequestQuery() != nil:
		response.Result = &cursorproto.InteractionResponse_WebFetchRequestResponse{WebFetchRequestResponse: &cursorproto.WebFetchRequestResponse{Result: &cursorproto.WebFetchRequestResponse_Rejected{Rejected: &cursorproto.CursorWebFetchRequestResponse_Rejected{Reason: reason}}}}
	case query.GetWebSearchRequestQuery() != nil:
		response.Result = &cursorproto.InteractionResponse_WebSearchRequestResponse{WebSearchRequestResponse: &cursorproto.WebSearchRequestResponse{Result: &cursorproto.WebSearchRequestResponse_Rejected{Rejected: &cursorproto.CursorWebSearchRequestResponse_Rejected{Reason: reason}}}}
	case query.GetSwitchModeRequestQuery() != nil:
		response.Result = &cursorproto.InteractionResponse_SwitchModeRequestResponse{SwitchModeRequestResponse: &cursorproto.SwitchModeRequestResponse{Result: &cursorproto.SwitchModeRequestResponse_Rejected{Rejected: &cursorproto.CursorSwitchModeRequestResponse_Rejected{Reason: reason}}}}
	case query.GetExaSearchRequestQuery() != nil:
		response.Result = &cursorproto.InteractionResponse_ExaSearchRequestResponse{ExaSearchRequestResponse: &cursorproto.ExaSearchRequestResponse{Result: &cursorproto.ExaSearchRequestResponse_Rejected{Rejected: &cursorproto.CursorExaSearchRequestResponse_Rejected{Reason: reason}}}}
	case query.GetExaFetchRequestQuery() != nil:
		response.Result = &cursorproto.InteractionResponse_ExaFetchRequestResponse{ExaFetchRequestResponse: &cursorproto.ExaFetchRequestResponse{Result: &cursorproto.ExaFetchRequestResponse_Rejected{Rejected: &cursorproto.CursorExaFetchRequestResponse_Rejected{Reason: reason}}}}
	case query.GetCreatePlanRequestQuery() != nil:
		response.Result = &cursorproto.InteractionResponse_CreatePlanRequestResponse{CreatePlanRequestResponse: &cursorproto.CreatePlanRequestResponse{Result: &cursorproto.CreatePlanResult{Result: &cursorproto.CreatePlanResult_Error{Error: &cursorproto.CreatePlanError{Error: reason}}}}}
	case query.GetSetupVmEnvironmentArgs() != nil:
		response.Result = &cursorproto.InteractionResponse_SetupVmEnvironmentResult{SetupVmEnvironmentResult: &cursorproto.SetupVmEnvironmentResult{Result: &cursorproto.SetupVmEnvironmentResult_Success{Success: &cursorproto.SetupVmEnvironmentSuccess{}}}}
	case query.GetAskQuestionInteractionQuery() != nil:
		response.Result = &cursorproto.InteractionResponse_AskQuestionInteractionResponse{AskQuestionInteractionResponse: &cursorproto.AskQuestionInteractionResponse{Result: &cursorproto.AskQuestionResult{Result: &cursorproto.AskQuestionResult_Rejected{Rejected: &cursorproto.AskQuestionRejected{Reason: reason}}}}}
	default:
		return fmt.Errorf("cursor executor: unsupported interaction query")
	}
	if errSend := duplex.Send(&cursorproto.AgentClientMessage{Message: &cursorproto.AgentClientMessage_InteractionResponse{InteractionResponse: response}}); errSend != nil {
		return fmt.Errorf("cursor executor: reject interaction query %d: %w", query.GetId(), errSend)
	}
	return nil
}

func cursorEmit(ctx context.Context, out chan<- cursorEvent, event cursorEvent) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func (e *CursorExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "cursor executor: auth is nil"}
	}
	refreshToken := cursorMetadataString(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, nil
	}
	tokens, err := cursorauth.Exchange(ctx, e.httpClient(auth), refreshToken)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = tokens.AccessToken
	auth.Metadata["refresh_token"] = tokens.RefreshToken
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	auth.Metadata["type"] = "cursor"
	return auth, nil
}

func (e *CursorExecutor) httpClient(auth *cliproxyauth.Auth) *http.Client {
	client := helps.NewProxyAwareHTTPClient(context.Background(), e.cfg, auth, 0)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		if defaultTransport, okDefault := http.DefaultTransport.(*http.Transport); okDefault && defaultTransport != nil {
			transport = defaultTransport.Clone()
		} else {
			transport = &http.Transport{}
		}
	}
	transport.ForceAttemptHTTP2 = true
	client.Transport = transport
	return client
}

func cursorCredentials(auth *cliproxyauth.Auth) (string, string) {
	if auth == nil {
		return "", ""
	}
	apiKey, baseURL := "", ""
	if auth.Attributes != nil {
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if apiKey == "" {
		apiKey = cursorMetadataString(auth.Metadata, "access_token")
	}
	return apiKey, baseURL
}

func cursorHeaders(auth *cliproxyauth.Auth) map[string]string {
	if auth == nil || auth.Attributes == nil {
		return nil
	}
	headers := make(map[string]string)
	for key, value := range auth.Attributes {
		if header, ok := strings.CutPrefix(key, "header:"); ok && strings.TrimSpace(header) != "" {
			headers[header] = value
		}
	}
	return headers
}

func cursorMetadataString(metadata map[string]any, key string) string {
	if value, ok := metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func cursorCanonicalRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) ([]byte, error) {
	from := opts.SourceFormat
	if from == "" {
		from = sdktranslator.FormatOpenAIResponse
	}
	if err := cursorconnect.ValidateTextRequest(req.Payload, string(from)); err != nil {
		return nil, requestScopedError{msg: "cursor executor: " + err.Error()}
	}
	body := sdktranslator.TranslateRequest(from, sdktranslator.FormatCodex, req.Model, req.Payload, false)
	if err := cursorconnect.ValidateTextRequest(body, string(sdktranslator.FormatCodex)); err != nil {
		return nil, requestScopedError{msg: "cursor executor: converted request: " + err.Error()}
	}
	return body, nil
}

func buildCursorRunRequest(payload []byte, model, conversationID string) *cursorproto.AgentRunRequest {
	text := cursorPromptText(payload)
	instruction := gjson.GetBytes(payload, "instructions").String()
	root := cursorRootPromptMessages(payload, instruction)
	maxMode := false
	if gjson.GetBytes(payload, "reasoning.effort").String() == "xhigh" {
		maxMode = true
	}
	return &cursorproto.AgentRunRequest{
		ConversationState: &cursorproto.ConversationStateStructure{RootPromptMessagesJson: root},
		Action:            &cursorproto.ConversationAction{Action: &cursorproto.ConversationAction_UserMessageAction{UserMessageAction: &cursorproto.UserMessageAction{UserMessage: &cursorproto.UserMessage{Text: text}}}},
		ModelDetails:      &cursorproto.ModelDetails{ModelId: model, DisplayModelId: model, DisplayName: model, MaxMode: &maxMode},
		RequestedModel:    &cursorproto.RequestedModel{ModelId: model, MaxMode: maxMode},
		ConversationId:    &conversationID,
	}
}

// cursorRootPromptMessages preserves the Responses input transcript instead of flattening it.
func cursorRootPromptMessages(payload []byte, instruction string) [][]byte {
	root := make([][]byte, 0, 1)
	if instruction != "" {
		root = append(root, []byte(`{"role":"system","content":`+mustJSON(instruction)+`}`))
	}
	input := gjson.GetBytes(payload, "input")
	if input.Type == gjson.String {
		root = append(root, []byte(`{"role":"user","content":`+mustJSON(input.String())+`}`))
		return root
	}
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.JSON {
			root = append(root, []byte(item.Raw))
		}
		return true
	})
	return root
}

func cursorPromptText(payload []byte) string {
	input := gjson.GetBytes(payload, "input")
	if input.Type == gjson.String {
		return input.String()
	}
	latest := ""
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("role").String() != "user" {
			return true
		}
		var parts []string
		content := item.Get("content")
		if content.Type == gjson.String {
			parts = append(parts, content.String())
		} else {
			content.ForEach(func(_, part gjson.Result) bool {
				typ := part.Get("type").String()
				if typ == "input_text" || typ == "text" || typ == "output_text" {
					if text := part.Get("text").String(); text != "" {
						parts = append(parts, text)
					}
				}
				return true
			})
		}
		if len(parts) > 0 {
			latest = strings.Join(parts, "\n")
		}
		return true
	})
	return latest
}

func cursorConversationID(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if opts.Metadata != nil {
		if value, ok := opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return cursorResponseID(req, opts)
}

func cursorShellOwner(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	sessionID := cursorConversationID(req, opts)
	if opts.Metadata != nil {
		if value, ok := opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string); ok && strings.TrimSpace(value) != "" {
			sessionID = strings.TrimSpace(value)
		}
	}
	return authID + "\x00" + sessionID
}

func cursorResponseID(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	seed := req.Model + "\x00" + string(req.Payload)
	if opts.Metadata != nil {
		if value, ok := opts.Metadata[cliproxyexecutor.DerivedSessionIDMetadataKey].(string); ok {
			seed += "\x00" + value
		}
	}
	sum := sha256.Sum256([]byte(seed))
	return "resp_cursor_" + hex.EncodeToString(sum[:8])
}

func mustJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func cursorStreamEvent(eventType, responseID, model, text string) []byte {
	body := map[string]any{"type": eventType, "response": map[string]any{"id": responseID, "object": "response", "model": model, "status": "in_progress"}}
	if eventType == "response.completed" {
		body["response"].(map[string]any)["status"] = "completed"
	}
	encoded, _ := json.Marshal(body)
	return append([]byte("data: "), append(encoded, []byte("\n\n")...)...)
}

func cursorTextDelta(responseID, text string) []byte {
	encoded, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "response_id": responseID, "output_index": 0, "content_index": 0, "delta": text})
	return append([]byte("data: "), append(encoded, []byte("\n\n")...)...)
}

func cursorReasoningDelta(responseID, text string) []byte {
	encoded, _ := json.Marshal(map[string]any{"type": "response.reasoning_summary_text.delta", "response_id": responseID, "output_index": 0, "summary_index": 0, "delta": text})
	return append([]byte("data: "), append(encoded, []byte("\n\n")...)...)
}

func cursorCompletedResponse(model, responseID, text, reasoning string) []byte {
	content := make([]map[string]any, 0, 2)
	if reasoning != "" {
		content = append(content, map[string]any{"type": "reasoning", "summary": []map[string]any{{"type": "summary_text", "text": reasoning}}})
	}
	content = append(content, map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}})
	encoded, _ := json.Marshal(map[string]any{"id": responseID, "object": "response", "status": "completed", "model": model, "output": content, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}})
	return encoded
}

type cursorStatusError struct {
	code int
	body []byte
}

func newCursorStatusErr(code int, body []byte) *cursorStatusError {
	return &cursorStatusError{code: code, body: append([]byte(nil), body...)}
}
func (e *cursorStatusError) Error() string {
	return fmt.Sprintf("cursor executor: upstream status %d: %s", e.code, helps.SummarizeErrorBody("application/json", e.body))
}
func (e *cursorStatusError) StatusCode() int { return e.code }

type requestScopedError struct{ msg string }

func (e requestScopedError) StatusCode() int       { return http.StatusBadRequest }
func (e requestScopedError) Error() string         { return e.msg }
func (e requestScopedError) IsRequestScoped() bool { return true }
