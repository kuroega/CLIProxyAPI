package executor

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
	cursorconnect "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursor"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/proto"
)

func TestCursorResponseFormatParity(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := cursorconnect.ReadFrame(r.Body); err != nil {
			return
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		message := &cursorproto.AgentServerMessage{Message: &cursorproto.AgentServerMessage_InteractionUpdate{InteractionUpdate: &cursorproto.InteractionUpdate{Message: &cursorproto.InteractionUpdate_TextDelta{TextDelta: &cursorproto.TextDeltaUpdate{Text: "answer"}}}}}
		payload, err := proto.Marshal(message)
		if err != nil {
			return
		}
		_ = cursorconnect.WriteFrame(w, payload)
		_ = cursorconnect.WriteEndStream(w, []byte(`{}`))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = previous }()
	auth := &cliproxyauth.Auth{ID: "test", Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL}}
	for _, tc := range []struct {
		name   string
		format sdktranslator.Format
		path   string
	}{
		{"responses", sdktranslator.FormatOpenAIResponse, "output.0.content.0.text"},
		{"chat", sdktranslator.FormatOpenAI, "choices.0.message.content"},
		{"claude", sdktranslator.FormatClaude, "content.0.text"},
		{"gemini", sdktranslator.FormatGemini, "candidates.0.content.parts.0.text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := NewCursorExecutor(nil)
			req := cliproxyexecutor.Request{Model: "cursor-model", Payload: []byte(`{"input":"hi"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: tc.format}
			response, err := executor.Execute(t.Context(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(response.Payload, tc.path).String() != "answer" {
				t.Errorf("nonstream output = %s", response.Payload)
			}
			stream, err := executor.ExecuteStream(t.Context(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			if !strings.Contains(output.String(), "answer") {
				t.Errorf("stream omitted text: %s", output.String())
			}
			if tc.format == sdktranslator.FormatOpenAI && !strings.Contains(output.String(), `"choices"`) {
				t.Errorf("chat stream was not translated: %s", output.String())
			}
		})
	}
}
