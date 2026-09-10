package executor

import (
	"encoding/json"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCursorRequestRejectsUnsupportedInput(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		format        sdktranslator.Format
	}{
		{"invalid JSON", `{`, sdktranslator.FormatOpenAIResponse},
		{"null", `null`, sdktranslator.FormatOpenAIResponse},
		{"missing input", `{}`, sdktranslator.FormatOpenAIResponse},
		{"object input", `{"input":{"role":"user","content":"hi"}}`, sdktranslator.FormatOpenAIResponse},
		{"image", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/image"}]}]}`, sdktranslator.FormatOpenAIResponse},
		{"tools", `{"input":"hi","tools":[{"type":"function","name":"read"}]}`, sdktranslator.FormatOpenAIResponse},
		{"tool result", `{"input":[{"type":"function_call_output","call_id":"a","output":"text"}]}`, sdktranslator.FormatOpenAIResponse},
		{"previous response", `{"input":"hi","previous_response_id":"resp_a"}`, sdktranslator.FormatOpenAIResponse},
		{"assistant continuation", `{"input":[{"role":"assistant","content":"continue"}]}`, sdktranslator.FormatOpenAIResponse},
		{"unknown role", `{"input":[{"role":"alien","content":"hi"}]}`, sdktranslator.FormatOpenAIResponse},
		{"chat image before translation", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image"}}]}]}`, sdktranslator.FormatOpenAI},
		{"chat functions", `{"messages":[{"role":"user","content":"hi"}],"functions":[{"name":"tool"}]}`, sdktranslator.FormatOpenAI},
		{"chat tool history", `{"messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"tool","arguments":"{}"}}]},{"role":"user","content":"hi"}]}`, sdktranslator.FormatOpenAI},
		{"claude image", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"abc"}}]}]}`, sdktranslator.FormatClaude},
		{"gemini inline data", `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"abc"}}]}]}`, sdktranslator.FormatGemini},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cursorCanonicalRequest(cliproxyexecutor.Request{Model: "cursor-model", Payload: []byte(tc.payload)}, cliproxyexecutor.Options{SourceFormat: tc.format})
			if err == nil {
				t.Fatal("unsupported request accepted")
			}
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != 400 {
				t.Fatalf("expected HTTP 400, got %v", err)
			}
		})
	}
}

func TestCursorRequestPreservesTextConversation(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		format        sdktranslator.Format
	}{
		{"responses", `{"instructions":"system instruction","input":[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"role":"user","content":[{"type":"input_text","text":"last"}]}]}`, sdktranslator.FormatOpenAIResponse},
		{"chat", `{"messages":[{"role":"system","content":"system instruction"},{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"last"}]}`, sdktranslator.FormatOpenAI},
		{"claude", `{"system":"system instruction","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"last"}]}`, sdktranslator.FormatClaude},
		{"gemini", `{"systemInstruction":{"parts":[{"text":"system instruction"}]},"contents":[{"role":"user","parts":[{"text":"first"}]},{"role":"model","parts":[{"text":"answer"}]},{"role":"user","parts":[{"text":"last"}]}]}`, sdktranslator.FormatGemini},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := cursorCanonicalRequest(cliproxyexecutor.Request{Model: "cursor-model", Payload: []byte(tc.payload)}, cliproxyexecutor.Options{SourceFormat: tc.format})
			if err != nil {
				t.Fatal(err)
			}
			run := buildCursorRunRequest(payload, "cursor-model", "session")
			root := run.GetConversationState().GetRootPromptMessagesJson()
			if len(root) != 4 {
				t.Fatalf("root message count = %d; canonical=%s", len(root), payload)
			}
			for i, role := range []string{"system", "user", "assistant", "user"} {
				if i == 0 && tc.format != sdktranslator.FormatOpenAIResponse {
					role = "developer"
				}
				if !json.Valid(root[i]) || gjson.GetBytes(root[i], "role").String() != role {
					t.Fatalf("message %d = %s, expected role %s", i, root[i], role)
				}
			}
			if got := run.GetAction().GetUserMessageAction().GetUserMessage().GetText(); got != "last" {
				t.Fatalf("action = %q", got)
			}
		})
	}
}
