package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const provider = "workbuddy"

type callback func(string, any, any) error
type credential struct {
	Provider     string `json:"provider"`
	UID          string `json:"uid"`
	Realm        string `json:"realm"`
	Product      string `json:"product,omitempty"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
}
type loginState struct {
	realm    string
	upstream string
	created  time.Time
}

var logins = struct {
	sync.Mutex
	states map[string]loginState
}{states: make(map[string]loginState)}

type upstreamError struct {
	status  int
	message string
}

func (e *upstreamError) Error() string { return e.message }

func realmURL(realm string) string {
	if realm == "cn" {
		return "https://copilot.tencent.com"
	}
	return "https://www.workbuddy.ai"
}
func origin(realm string) string {
	if realm == "cn" {
		return "https://www.codebuddy.cn"
	}
	return "https://www.workbuddy.ai"
}
func validRealm(raw string) bool { return raw == "cn" || raw == "intl" }
func normalizeProduct(raw string) string {
	switch raw {
	case "cli", "vscode":
		return raw
	default:
		return "workbuddy"
	}
}
func commonHeaders(realm string) http.Header {
	o := origin(realm)
	h := httpHeaders(map[string]string{"Accept": "application/json, text/plain, */*", "Content-Type": "application/json", "Origin": o, "Referer": o + "/", "X-Requested-With": "XMLHttpRequest"})
	if realm == "cn" {
		h.Set("User-Agent", "WorkBuddy/5.5.6")
	} else {
		h.Set("User-Agent", "WorkBuddy/5.5.2")
	}
	return h
}
func upstreamJSON(call callback, method, endpoint string, headers http.Header, body []byte) (map[string]any, error) {
	var resp pluginapi.HTTPResponse
	err := call(pluginabi.MethodHostHTTPDo, hostRequest{HTTPRequest: pluginapi.HTTPRequest{Method: method, URL: endpoint, Headers: headers, Body: body}}, &resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &upstreamError{resp.StatusCode, fmt.Sprintf("WorkBuddy HTTP %d", resp.StatusCode)}
	}
	var obj map[string]any
	if err = json.Unmarshal(resp.Body, &obj); err != nil {
		return nil, fmt.Errorf("decode WorkBuddy JSON: %w", err)
	}
	return obj, nil
}
func dataObject(obj map[string]any) map[string]any {
	data, _ := obj["data"].(map[string]any)
	if inner, ok := data["data"].(map[string]any); ok {
		return inner
	}
	return data
}
func field(obj map[string]any, key string) string { s, _ := obj[key].(string); return s }
func number(obj map[string]any, key string) int64 {
	n, _ := obj[key].(float64)
	if n > 1e11 {
		n /= 1000
	}
	return int64(n)
}
func tokenUID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil {
		return ""
	}
	return field(claims, "sub")
}
func authData(c credential, name string) (pluginapi.AuthData, error) {
	if c.UID == "" || c.AccessToken == "" || !validRealm(c.Realm) {
		return pluginapi.AuthData{}, errors.New("credential needs UID, access token and realm")
	}
	c.Provider = provider
	c.Product = normalizeProduct(c.Product)
	raw, err := json.Marshal(c)
	if err != nil {
		return pluginapi.AuthData{}, err
	}
	digest := sha256.Sum256([]byte(c.UID))
	id := provider + "-" + c.Realm + "-" + hex.EncodeToString(digest[:8])
	data := pluginapi.AuthData{Provider: provider, ID: id, FileName: id + ".json", Label: "WorkBuddy " + c.Realm + " (" + c.UID + ")", StorageJSON: raw, Attributes: map[string]string{"realm": c.Realm}}
	if name != "" {
		data.FileName = name
	}
	expiry := c.ExpiresAt
	if expiry == 0 {
		expiry = jwtExpiry(c.AccessToken)
	}
	if expiry > 0 {
		data.NextRefreshAfter = time.Unix(expiry, 0).Add(-5 * time.Minute)
	}
	return data, nil
}
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil {
		return 0
	}
	return number(claims, "exp")
}
func decodeCredential(raw []byte) (credential, error) {
	var c credential
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.Provider != "" && c.Provider != provider {
		return c, errors.New("not a WorkBuddy credential")
	}
	if c.Realm == "" {
		return c, errors.New("realm must be intl or cn")
	}
	if !validRealm(c.Realm) || c.UID == "" || c.AccessToken == "" {
		return c, errors.New("invalid WorkBuddy credential")
	}
	return c, nil
}
func parseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.Provider != "" && req.Provider != provider {
		return success(pluginapi.AuthParseResponse{})
	}
	c, err := decodeCredential(req.RawJSON)
	if err != nil {
		return success(pluginapi.AuthParseResponse{})
	}
	data, err := authData(c, req.FileName)
	if err != nil {
		return nil, err
	}
	return success(pluginapi.AuthParseResponse{Handled: true, Auth: data})
}
func startLogin(raw []byte, call callback) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	realm := "intl"
	if r, ok := req.Metadata["realm"].(string); ok && r != "" {
		if !validRealm(r) {
			return nil, errors.New("realm must be intl or cn")
		}
		realm = r
	}
	endpoint := realmURL(realm) + "/v2/plugin/auth/state?platform=CLI"
	obj, err := upstreamJSON(call, http.MethodPost, endpoint, commonHeaders(realm), []byte("{}"))
	if err != nil {
		return nil, err
	}
	data := dataObject(obj)
	state, authURL := field(data, "state"), field(data, "authUrl")
	if state == "" || authURL == "" {
		return nil, errors.New("WorkBuddy login did not return state and authUrl")
	}
	localState := uuid.NewString()
	logins.Lock()
	logins.states[localState] = loginState{realm: realm, upstream: state, created: time.Now()}
	logins.Unlock()
	return success(pluginapi.AuthLoginStartResponse{Provider: provider, State: localState, URL: authURL, ExpiresAt: time.Now().Add(10 * time.Minute), Metadata: map[string]any{"realm": realm, "upstream_state": state}})
}
func pollLogin(raw []byte, call callback) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	logins.Lock()
	info, ok := logins.states[req.State]
	logins.Unlock()
	// The host may send the polling metadata after a process reload.
	if !ok {
		if r, yes := req.Metadata["realm"].(string); yes && validRealm(r) {
			upstream, _ := req.Metadata["upstream_state"].(string)
			info = loginState{realm: r, upstream: upstream, created: time.Now()}
			ok = true
		}
	}
	if !ok || time.Since(info.created) > 10*time.Minute {
		return success(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login expired; start again"})
	}
	obj, err := upstreamJSON(call, http.MethodGet, realmURL(info.realm)+"/v2/plugin/auth/token?state="+url.QueryEscape(info.upstream), commonHeaders(info.realm), nil)
	if err != nil {
		return nil, err
	}
	if code, ok := obj["code"].(float64); ok && int(code) == 11217 {
		return success(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending})
	}
	if code, ok := obj["code"].(float64); ok && code != 0 {
		return success(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "WorkBuddy rejected login"})
	}
	data := dataObject(obj)
	token := field(data, "accessToken")
	if token == "" {
		return success(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending})
	}
	c := credential{Realm: info.realm, UID: tokenUID(token), AccessToken: token, RefreshToken: field(data, "refreshToken"), ExpiresAt: number(data, "expiresAt")}
	auth, err := authData(c, "")
	if err != nil {
		return nil, err
	}
	logins.Lock()
	delete(logins.states, req.State)
	logins.Unlock()
	return success(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: auth})
}
func refreshAuth(raw []byte, call callback) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	c, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	if c.RefreshToken == "" {
		return nil, errors.New("WorkBuddy refresh token missing; sign in again")
	}
	h := commonHeaders(c.Realm)
	h.Set("X-Refresh-Token", c.RefreshToken)
	h.Set("X-User-Id", c.UID)
	if c.Realm == "cn" {
		h.Set("X-Auth-Refresh-Source", "workbuddy")
		h.Set("X-Domain", "copilot.tencent.com")
	} else {
		h.Set("X-Auth-Refresh-Source", "plugin")
		h.Set("X-Domain", "www.workbuddy.ai")
	}
	h.Set("X-CodeBuddy-Request", "1")
	obj, err := upstreamJSON(call, http.MethodPost, realmURL(c.Realm)+"/v2/plugin/auth/token/refresh", h, []byte("{}"))
	if err != nil {
		return nil, err
	}
	data := dataObject(obj)
	token := field(data, "accessToken")
	if token == "" {
		return nil, errors.New("WorkBuddy refresh returned no access token")
	}
	c.AccessToken = token
	if next := field(data, "refreshToken"); next != "" {
		c.RefreshToken = next
	}
	c.ExpiresAt = jwtExpiry(token)
	if c.ExpiresAt == 0 {
		c.ExpiresAt = number(data, "expiresAt")
	}
	auth, err := authData(c, "")
	if err != nil {
		return nil, err
	}
	return success(pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: auth.NextRefreshAfter})
}

var intlModels = []string{"hy4-preview-f", "hy3", "deepseek-v4.1-flash", "gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "grok-4.7", "gemini-3.5-flash", "glm-5.3-flash", "glm-5.3", "glm-5.2", "kimi-k3", "kimi-k2.6", "kimi-k2.8-preview"}
var cnModels = []string{"hy4-preview-f", "hy3", "deepseek-v4.1-flash", "deepseek-v4-pro", "glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo", "minimax-m3", "kimi-k3-1", "kimi-k2.8-preview", "kimi-k2.7", "kimi-k2.6"}

func catalog(realm string) []pluginapi.ModelInfo {
	ids := intlModels
	if realm == "cn" {
		ids = cnModels
	} else if realm == "all" {
		ids = append(append([]string{}, intlModels...), cnModels...)
	}
	seen := make(map[string]bool)
	out := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: provider, SupportedGenerationMethods: []string{"chat"}})
	}
	return out
}
func modelsForAuth(raw []byte, call callback) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	c, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	models := catalog(c.Realm)
	h := commonHeaders(c.Realm)
	h.Set("Authorization", "Bearer "+c.AccessToken)
	h.Set("X-User-Id", c.UID)
	if c.Realm == "cn" {
		h.Set("User-Agent", "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1")
	} else {
		h.Set("User-Agent", "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2")
	}
	obj, err := upstreamJSON(call, http.MethodGet, realmURL(c.Realm)+"/v3/config", h, nil)
	if err == nil {
		if ids := remoteModelIDs(obj); len(ids) > 0 {
			models = make([]pluginapi.ModelInfo, 0, len(ids))
			for _, id := range ids {
				models = append(models, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: provider, SupportedGenerationMethods: []string{"chat"}})
			}
		}
	}
	return success(pluginapi.ModelResponse{Provider: provider, Models: models})
}

func remoteModelIDs(obj map[string]any) []string {
	root := obj
	if data, ok := obj["data"].(map[string]any); ok {
		root = data
	}
	agents, ok := root["agents"].(map[string]any)
	if !ok {
		return nil
	}
	cli, ok := agents["cli"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := cli["models"].([]any)
	if !ok {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, entry := range list {
		id, ok := entry.(string)
		if !ok || id == "" || seen[id] || strings.HasSuffix(id, "-sg") || strings.HasSuffix(id, "-x") {
			continue
		}
		switch id {
		case "default-model", "fast-model", "balanced-model", "primary-model", "deep-model", "auto":
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
func upstreamBody(req []byte, model string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(req))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("invalid chat request: %w", err)
	}
	if body == nil {
		return nil, errors.New("chat request must be an object")
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("messages are required")
	}
	for _, v := range messages {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if m["role"] == "developer" {
			m["role"] = "system"
		}
	}
	if first, ok := messages[0].(map[string]any); !ok || first["role"] != "system" {
		body["messages"] = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, messages...)
	}
	if n, ok := body["max_completion_tokens"]; ok {
		if _, exists := body["max_tokens"]; !exists {
			body["max_tokens"] = n
		}
		delete(body, "max_completion_tokens")
	}
	if tc, ok := body["tool_choice"].(map[string]any); ok {
		switch tc["type"] {
		case "function":
			fn, _ := tc["function"].(map[string]any)
			body["tool_choice"] = field(fn, "name")
		case "none", "auto", "required":
			body["tool_choice"] = tc["type"]
		default:
			delete(body, "tool_choice")
		}
	}
	if strings.HasPrefix(strings.ToLower(model), "deepseek") {
		thinking, _ := body["thinking"].(map[string]any)
		effort, _ := body["reasoning_effort"].(string)
		if thinking["type"] != "disabled" && effort != "none" {
			if thinking == nil {
				body["thinking"] = map[string]any{"type": "enabled"}
			}
			if effort == "" {
				body["reasoning_effort"] = "high"
			}
			for _, v := range messages {
				m, ok := v.(map[string]any)
				if !ok || m["role"] != "assistant" {
					continue
				}
				rc, _ := m["reasoning_content"].(string)
				if rc == "" {
					rc, _ = m["reasoning"].(string)
				}
				m["reasoning_content"] = rc
				if value, _ := m["reasoning"].(string); value == "" {
					if rc == "" {
						m["reasoning"] = " "
					} else {
						m["reasoning"] = rc
					}
				}
			}
		}
	}
	for key := range body {
		if strings.HasPrefix(key, "_") {
			delete(body, key)
		}
	}
	body["model"] = model
	body["stream"] = true
	body["stream_options"] = map[string]bool{"include_usage": true}
	return json.Marshal(body)
}
