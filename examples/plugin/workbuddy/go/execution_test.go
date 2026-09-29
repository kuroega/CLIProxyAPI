package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestExecuteChatOffline(t *testing.T) {
	c := credential{UID: "test-user", Realm: "intl", AccessToken: "secret", Product: "workbuddy"}
	storage, _ := json.Marshal(c)
	request, _ := json.Marshal(rpcRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "hy3", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), StorageJSON: storage}})
	read := 0
	closed := false
	call := func(method string, payload any, result any) error {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			req := payload.(hostRequest)
			if req.URL != "https://www.workbuddy.ai/v2/chat/completions" || req.Headers.Get("Authorization") != "Bearer secret" || req.Headers.Get("X-Machine-ID") == "" {
				t.Fatalf("incorrect request: %+v", req)
			}
			*result.(*hostStream) = hostStream{StatusCode: http.StatusOK, StreamID: "1"}
		case pluginabi.MethodHostHTTPStreamRead:
			r := result.(*hostChunk)
			if read == 0 {
				r.Payload = []byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			} else {
				r.Done = true
			}
			read++
		case pluginabi.MethodHostHTTPStreamClose:
			closed = true
		default:
			t.Fatalf("unexpected callback %s", method)
		}
		return nil
	}
	raw, err := execute(request, call, false)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err = json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var reply pluginapi.ExecutorResponse
	if err = json.Unmarshal(env.Result, &reply); err != nil {
		t.Fatal(err)
	}
	if !closed || !strings.Contains(string(reply.Payload), `"content":"Hi"`) {
		t.Fatalf("bad reply: %s, closed=%v", reply.Payload, closed)
	}
}
func TestExecuteStreamEmitsJSONNotSSE(t *testing.T) {
	c := credential{UID: "test-user", Realm: "cn", AccessToken: "secret"}
	storage, _ := json.Marshal(c)
	request, _ := json.Marshal(rpcRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "hy3", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), StorageJSON: storage}, StreamID: "output"})
	finished := make(chan struct{})
	var chunks [][]byte
	read := 0
	call := func(method string, payload any, result any) error {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			*result.(*hostStream) = hostStream{StatusCode: http.StatusOK, StreamID: "upstream"}
		case pluginabi.MethodHostHTTPStreamRead:
			r := result.(*hostChunk)
			if read == 0 {
				r.Payload = []byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")
			} else {
				r.Done = true
			}
			read++
		case pluginabi.MethodHostStreamEmit:
			chunks = append(chunks, payload.(map[string]any)["payload"].([]byte))
		case pluginabi.MethodHostStreamClose:
			close(finished)
		case pluginabi.MethodHostHTTPStreamClose:
		default:
			t.Fatalf("unexpected callback: %s", method)
		}
		return nil
	}
	if _, err := execute(request, call, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close")
	}
	if len(chunks) != 1 || !json.Valid(chunks[0]) || strings.HasPrefix(string(chunks[0]), "data:") {
		t.Fatalf("executor stream chunks must be JSON, got %q", chunks)
	}
}

func TestRemoteModelIDs(t *testing.T) {
	got := remoteModelIDs(map[string]any{"data": map[string]any{"agents": map[string]any{"cli": map[string]any{"models": []any{"deepseek-v4.1-flash", "deepseek-v4.1-flash-sg", "auto", "deepseek-v4.1-flash", "new-model"}}}}})
	if len(got) != 2 || got[1] != "new-model" {
		t.Fatalf("models: %v", got)
	}
}
func TestIdentityRegion(t *testing.T) {
	c := credential{UID: "a", Realm: "cn", Product: "vscode", AccessToken: "secret"}
	if chatEndpoint(c) != "https://www.workbuddy.cn/v2/chat/completions" {
		t.Fatal("wrong CN VSCode endpoint")
	}
	h := identityHeaders(c)
	if h.Get("X-Product") != "SaaS" || h.Get("X-Domain") != "www.workbuddy.cn" {
		t.Fatalf("identity: %v", h)
	}
}
