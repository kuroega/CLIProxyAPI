package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func jwt(sub string) string {
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":%q,"exp":1900000000}`, sub))) + ".sig"
}
func TestAuthAndRefresh(t *testing.T) {
	token := jwt("test-uid")
	var sent hostRequest
	call := func(method string, request any, result any) error {
		sent = request.(hostRequest)
		var body []byte
		switch {
		case strings.Contains(sent.URL, "/auth/state"):
			body = []byte(`{"data":{"state":"s","authUrl":"https://example.com/login"}}`)
		case strings.Contains(sent.URL, "/auth/token?"):
			body = []byte(`{"code":0,"data":{"accessToken":` + fmt.Sprintf("%q", token) + `,"refreshToken":"old"}}`)
		case strings.Contains(sent.URL, "/token/refresh"):
			body = []byte(`{"data":{"data":{"accessToken":` + fmt.Sprintf("%q", token) + `,"refreshToken":"new"}}}`)
		default:
			t.Fatalf("unexpected URL %s", sent.URL)
		}
		*result.(*pluginapi.HTTPResponse) = pluginapi.HTTPResponse{StatusCode: 200, Body: body}
		return nil
	}
	start, _ := json.Marshal(pluginapi.AuthLoginStartRequest{Metadata: map[string]any{"realm": "cn"}})
	raw, err := startLogin(start, call)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err = json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var started pluginapi.AuthLoginStartResponse
	_ = json.Unmarshal(env.Result, &started)
	if started.State == "s" || started.State == "" || !strings.Contains(sent.URL, "copilot.tencent.com") {
		t.Fatalf("start: %+v %s", started, sent.URL)
	}
	poll, _ := json.Marshal(pluginapi.AuthLoginPollRequest{State: started.State})
	raw, err = pollLogin(poll, call)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &env)
	var done pluginapi.AuthLoginPollResponse
	_ = json.Unmarshal(env.Result, &done)
	if done.Status != pluginapi.AuthLoginStatusSuccess || done.Auth.Provider != provider {
		t.Fatalf("poll: %+v", done)
	}
	if strings.Contains(done.Auth.FileName, "test-uid") {
		t.Fatal("file name should not expose uid")
	}
	refresh, _ := json.Marshal(pluginapi.AuthRefreshRequest{StorageJSON: done.Auth.StorageJSON})
	raw, err = refreshAuth(refresh, call)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &env)
	var updated pluginapi.AuthRefreshResponse
	_ = json.Unmarshal(env.Result, &updated)
	c, err := decodeCredential(updated.Auth.StorageJSON)
	if err != nil || c.RefreshToken != "new" || c.UID != "test-uid" {
		t.Fatalf("refresh: %+v %v", c, err)
	}
	if sent.Headers.Get("X-Auth-Refresh-Source") != "workbuddy" {
		t.Fatal("incorrect refresh headers")
	}
}
func TestUpstreamBody(t *testing.T) {
	payload := []byte(`{"model":"old","messages":[{"role":"developer","content":"rules"},{"role":"user","content":"hi"}],"max_completion_tokens":42,"tool_choice":{"type":"function","function":{"name":"run"}},"_private":1}`)
	raw, err := upstreamBody(payload, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if body["model"] != "deepseek-v4.1-flash" || body["tool_choice"] != "run" || body["max_tokens"] != float64(42) || body["_private"] != nil {
		t.Fatalf("body: %s", raw)
	}
	msgs := body["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("roles: %s", raw)
	}
}
func TestSSEChunkBoundariesAndAggregate(t *testing.T) {
	frames := []byte("data: {\"id\":\"abc\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	chunks := [][]byte{frames[:24], frames[24:77], frames[77:]}
	i := 0
	closed := false
	call := func(method string, request any, result any) error {
		switch method {
		case pluginabi.MethodHostHTTPStreamRead:
			r := result.(*hostChunk)
			if i < len(chunks) {
				r.Payload = chunks[i]
				i++
			} else {
				r.Done = true
			}
		case pluginabi.MethodHostHTTPStreamClose:
			closed = true
		default:
			t.Fatalf("unexpected callback: %s", method)
		}
		return nil
	}
	agg := newAggregator("test-model")
	var got []string
	err := readEvents(call, "stream", func(event []byte) error { got = append(got, string(event)); return agg.accept(event) })
	if err != nil || !closed || len(got) != 3 {
		t.Fatalf("events: %d, closed=%v, err=%v", len(got), closed, err)
	}
	raw, err := agg.result()
	if err != nil || !strings.Contains(string(raw), "Hello world") {
		t.Fatalf("result: %s %v", raw, err)
	}
}
func TestRealmModelsAndErrors(t *testing.T) {
	cn := catalog("cn")
	intl := catalog("intl")
	if len(cn) == 0 || len(intl) == 0 {
		t.Fatal("empty catalogs")
	}
	c, _ := authData(credential{UID: "/../../x", Realm: "cn", AccessToken: jwt("x")}, "")
	if strings.Contains(c.FileName, "/") {
		t.Fatalf("unsafe filename %s", c.FileName)
	}
	if err := decodeAndCheckError(); err != nil {
		t.Fatal(err)
	}
}
func decodeAndCheckError() error {
	raw := fail(&upstreamError{429, "rate limited"})
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if env.Error == nil || env.Error.HTTPStatus != 429 {
		return fmt.Errorf("missing status: %s", raw)
	}
	return nil
}
