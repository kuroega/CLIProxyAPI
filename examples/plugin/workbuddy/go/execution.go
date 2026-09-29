package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// identityHeaders mimics the selected WorkBuddy product's request identity.
// These headers are upstream protocol requirements, not user-configurable
// profile names. The machine/session hashes are deterministic per account;
// never log these headers because they also contain the bearer token.
func identityHeaders(c credential) http.Header {
	h := commonHeaders(c.Realm)
	h.Set("Authorization", "Bearer "+c.AccessToken)
	h.Set("X-User-Id", c.UID)
	h.Set("X-CodeBuddy-Request", "1")
	machine := md5.Sum([]byte("machine:" + c.UID))
	session := md5.Sum([]byte("session:" + c.UID))
	h.Set("X-Machine-ID", hex.EncodeToString(machine[:]))
	h.Set("X-Session-ID", hex.EncodeToString(session[:]))
	h.Set("X-Request-ID", uuid.NewString())
	h.Set("X-Agent-Purpose", "conversation")
	h.Set("Accept-Language", "en-US")
	if c.Realm == "cn" {
		h.Set("Accept-Language", "zh-CN")
	}
	switch normalizeProduct(c.Product) {
	case "cli":
		h.Set("X-Agent-Intent", "craft")
		h.Del("X-Agent-Purpose")
		h.Set("X-IDE-Name", "CLI")
		h.Set("X-IDE-Type", "CLI")
		h.Set("X-IDE-Version", "2.117.2")
		h.Set("X-Product", "SaaS")
		h.Set("User-Agent", "CLI/2.117.2 CodeBuddy/2.117.2")
	case "vscode":
		h.Set("X-Agent-Intent", "craft")
		h.Del("X-Agent-Purpose")
		h.Set("X-IDE-Name", "VSCode")
		h.Set("X-IDE-Type", "VSCode")
		h.Set("X-IDE-Version", "1.119.0")
		h.Set("X-Product", "SaaS")
		h.Set("X-Product-Version", "4.9.29177644")
		h.Set("X-Env-ID", "production")
		h.Set("User-Agent", "VSCode/1.119.0 WorkBuddy/4.9.29177644")
	default:
		version, ua := "5.5.2", "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
		if c.Realm == "cn" {
			version, ua = "5.5.6", "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
		}
		h.Set("X-IDE-Name", "WorkBuddy")
		h.Set("X-IDE-Type", "WorkBuddy")
		h.Set("X-IDE-Version", version)
		h.Set("X-Product", "WorkBuddy")
		h.Set("User-Agent", ua)
	}
	domain := "www.workbuddy.ai"
	if c.Realm == "cn" {
		domain = "copilot.tencent.com"
		if normalizeProduct(c.Product) == "vscode" {
			domain = "www.workbuddy.cn"
		}
	}
	h.Set("X-Domain", domain)
	if normalizeProduct(c.Product) != "workbuddy" {
		conv := strings.ToUpper(uuid.NewString())
		h.Set("X-Conversation-ID", conv)
		h.Set("X-Conversation-Request-ID", strings.ReplaceAll(uuid.NewString(), "-", ""))
		h.Set("X-Conversation-Message-ID", strings.ReplaceAll(uuid.NewString(), "-", ""))
	}
	return h
}
func chatEndpoint(c credential) string {
	if c.Realm == "cn" && normalizeProduct(c.Product) == "vscode" {
		return "https://www.workbuddy.cn/v2/chat/completions"
	}
	return realmURL(c.Realm) + "/v2/chat/completions"
}

// execute opens one upstream SSE stream for both downstream modes. Streaming
// emits raw JSON chat chunks to the host stream API; non-streaming accumulates
// the same chunks into one OpenAI-compatible chat completion.
func execute(raw []byte, call callback, streaming bool) ([]byte, error) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	c, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	body, err := upstreamBody(req.Payload, req.Model)
	if err != nil {
		return nil, err
	}
	var resp hostStream
	err = call(pluginabi.MethodHostHTTPDoStream, hostRequest{HTTPRequest: pluginapi.HTTPRequest{Method: http.MethodPost, URL: chatEndpoint(c), Headers: identityHeaders(c), Body: body}, HostCallbackID: req.HostCallbackID}, &resp)
	if err != nil {
		return nil, err
	}
	if resp.StreamID == "" {
		return nil, errors.New("WorkBuddy returned no stream")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		closeUpstream(call, resp.StreamID)
		return nil, &upstreamError{resp.StatusCode, fmt.Sprintf("WorkBuddy HTTP %d", resp.StatusCode)}
	}
	if streaming {
		if req.StreamID == "" {
			closeUpstream(call, resp.StreamID)
			return nil, errors.New("executor stream ID is required")
		}
		// Return the stream response immediately; the goroutine owns reading
		// and closing the upstream and downstream streams from this point on.
		go func() {
			errRun := readEvents(call, resp.StreamID, func(event []byte) error {
				payload := bytes.TrimSpace(bytes.TrimPrefix(event, []byte("data:")))
				// Emit only the JSON payload: the host adds "data:" framing and
				// [DONE]. Sending our own SSE frame here would double-frame it.
				if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
					return nil
				}
				return call(pluginabi.MethodHostStreamEmit, map[string]any{"stream_id": req.StreamID, "payload": payload}, nil)
			})
			message := ""
			if errRun != nil {
				message = errRun.Error()
			}
			_ = call(pluginabi.MethodHostStreamClose, map[string]any{"stream_id": req.StreamID, "error": message}, nil)
		}()
		return success(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
	}
	agg := newAggregator(req.Model)
	err = readEvents(call, resp.StreamID, func(event []byte) error { return agg.accept(event) })
	if err != nil {
		return nil, err
	}
	out, err := agg.result()
	if err != nil {
		return nil, err
	}
	return success(pluginapi.ExecutorResponse{Payload: out, Headers: http.Header{"Content-Type": []string{"application/json"}}})
}
func closeUpstream(call callback, id string) {
	_ = call(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": id}, nil)
}

// readEvents assembles complete SSE events even when a host transport chunk
// splits a line or multiple frames arrive together. It closes the upstream
// stream on every exit path. Keep the pending frame bound to avoid unbounded
// memory use on a malformed or malicious stream.
func readEvents(call callback, id string, consume func([]byte) error) error {
	defer closeUpstream(call, id)
	var pending []byte
	for {
		var chunk hostChunk
		if err := call(pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": id}, &chunk); err != nil {
			return err
		}
		if chunk.Error != "" {
			return errors.New(chunk.Error)
		}
		pending = append(pending, chunk.Payload...)
		if len(pending) > 8<<20 {
			return errors.New("WorkBuddy SSE frame exceeds 8 MiB")
		}
		for {
			pos, delim := bytes.Index(pending, []byte("\n\n")), 2
			if pos < 0 {
				pos = bytes.Index(pending, []byte("\r\n\r\n"))
				delim = 4
			}
			if pos < 0 {
				break
			}
			frame := append([]byte(nil), pending[:pos]...)
			pending = pending[pos+delim:]
			var lines []string
			for _, line := range strings.Split(strings.ReplaceAll(string(frame), "\r\n", "\n"), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "data:") {
					lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				}
			}
			if len(lines) == 0 {
				continue
			}
			data := strings.Join(lines, "\n")
			if data == "[DONE]" {
				if err := consume([]byte("data: [DONE]\n\n")); err != nil {
					return err
				}
				return nil
			}
			var obj map[string]any
			if json.Unmarshal([]byte(data), &obj) != nil {
				continue
			}
			// WorkBuddy can signal quota/rate/auth errors inside an HTTP 200 SSE
			// response; propagate them instead of returning a partial answer.
			if code, ok := obj["code"].(float64); ok && code != 0 {
				status := http.StatusBadGateway
				if code == 6004 {
					status = http.StatusTooManyRequests
				}
				if code == 11128 {
					status = http.StatusForbidden
				}
				return &upstreamError{status, fmt.Sprintf("WorkBuddy stream error (code %d)", int(code))}
			}
			choices, _ := obj["choices"].([]any)
			for _, v := range choices {
				choice, _ := v.(map[string]any)
				delta, _ := choice["delta"].(map[string]any)
				if delta == nil {
					continue
				}
				if fc, ok := delta["function_call"].(map[string]any); ok && field(fc, "name") == "" {
					delete(delta, "function_call")
				}
				if tools, ok := delta["tool_calls"].([]any); ok && len(tools) == 0 {
					delete(delta, "tool_calls")
				}
			}
			clean, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			if err = consume(append(append([]byte("data: "), clean...), []byte("\n\n")...)); err != nil {
				return err
			}
		}
		if chunk.Done {
			if len(bytes.TrimSpace(pending)) > 0 {
				return errors.New("incomplete WorkBuddy SSE response")
			}
			return nil
		}
	}
}

type aggregatedTool struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool calls arrive in partial deltas keyed by index, not as complete JSON
// tools; collect fragments by index and concatenate names/arguments.
type aggregator struct {
	model     string
	id        string
	created   int64
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*aggregatedTool
	finish    string
	usage     any
	seen      bool
}

func newAggregator(model string) *aggregator {
	return &aggregator{model: model, id: "chatcmpl-" + uuid.NewString(), created: time.Now().Unix(), tools: make(map[int]*aggregatedTool)}
}
func (a *aggregator) accept(frame []byte) error {
	data := strings.TrimSpace(strings.TrimPrefix(string(frame), "data:"))
	if data == "[DONE]" {
		return nil
	}
	var v struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage any `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return err
	}
	if v.ID != "" {
		a.id = v.ID
	}
	if v.Created > 0 {
		a.created = v.Created
	}
	if v.Usage != nil {
		a.usage = v.Usage
	}
	for _, choice := range v.Choices {
		a.seen = true
		a.content.WriteString(choice.Delta.Content)
		a.reasoning.WriteString(choice.Delta.ReasoningContent)
		if choice.FinishReason != "" {
			a.finish = choice.FinishReason
		}
		for _, part := range choice.Delta.ToolCalls {
			tool := a.tools[part.Index]
			if tool == nil {
				tool = &aggregatedTool{Type: "function"}
				a.tools[part.Index] = tool
			}
			if part.ID != "" {
				tool.ID = part.ID
			}
			tool.Function.Name += part.Function.Name
			tool.Function.Arguments += part.Function.Arguments
		}
	}
	return nil
}

// A stream with only usage metadata or [DONE] is not a successful completion.
func (a *aggregator) result() ([]byte, error) {
	if !a.seen {
		return nil, errors.New("WorkBuddy returned no completion")
	}
	msg := map[string]any{"role": "assistant", "content": a.content.String()}
	if a.reasoning.Len() > 0 {
		msg["reasoning_content"] = a.reasoning.String()
	}
	if len(a.tools) > 0 {
		tools := make([]*aggregatedTool, 0, len(a.tools))
		for i := 0; i < len(a.tools); i++ {
			if t := a.tools[i]; t != nil {
				tools = append(tools, t)
			}
		}
		msg["tool_calls"] = tools
	}
	finish := a.finish
	if finish == "" {
		finish = "stop"
	}
	out := map[string]any{"id": a.id, "object": "chat.completion", "created": a.created, "model": a.model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}}
	if a.usage != nil {
		out["usage"] = a.usage
	}
	return json.Marshal(out)
}
