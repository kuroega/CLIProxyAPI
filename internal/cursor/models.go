// Package cursor contains native Cursor AgentService protocol helpers.
package cursor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
	"google.golang.org/protobuf/proto"
)

const (
	usableModelsPath     = "/agent.v1.AgentService/GetUsableModels"
	DefaultClientVersion = "cli-2026.02.13-41ac335"
)

// Model describes an entitlement returned by Cursor's GetUsableModels RPC.
type Model struct {
	ID          string
	DisplayName string
	MaxMode     bool
}

// FetchUsableModels discovers the native models available to one Cursor credential.
func FetchUsableModels(ctx context.Context, client *http.Client, baseURL, accessToken string) ([]Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("cursor model discovery: missing access token")
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api2.cursor.sh"
	}
	if client == nil {
		client = &http.Client{}
	}
	requestPayload, errMarshal := proto.Marshal(&cursorproto.GetUsableModelsRequest{})
	if errMarshal != nil {
		return nil, fmt.Errorf("cursor model discovery: marshal request: %w", errMarshal)
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+usableModelsPath, bytes.NewReader(requestPayload))
	if errRequest != nil {
		return nil, fmt.Errorf("cursor model discovery: create request: %w", errRequest)
	}
	request.Header.Set("Accept", "application/proto")
	request.Header.Set("Content-Type", "application/proto")
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("X-Ghost-Mode", "true")
	request.Header.Set("X-Cursor-Client-Type", "cli")
	request.Header.Set("X-Cursor-Client-Version", DefaultClientVersion)
	response, errDo := client.Do(request)
	if errDo != nil {
		return nil, fmt.Errorf("cursor model discovery: execute request: %w", errDo)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		data, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("cursor model discovery: status %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	responsePayload, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		return nil, fmt.Errorf("cursor model discovery: read response: %w", errRead)
	}
	var decoded cursorproto.GetUsableModelsResponse
	if errDecode := proto.Unmarshal(responsePayload, &decoded); errDecode != nil {
		return nil, fmt.Errorf("cursor model discovery: decode response: %w", errDecode)
	}
	models := make([]Model, 0, len(decoded.GetModels()))
	seen := make(map[string]struct{}, len(decoded.GetModels()))
	for _, detail := range decoded.GetModels() {
		if detail == nil || strings.TrimSpace(detail.GetModelId()) == "" {
			continue
		}
		id := strings.TrimSpace(detail.GetModelId())
		if _, exists := seen[strings.ToLower(id)]; exists {
			continue
		}
		seen[strings.ToLower(id)] = struct{}{}
		name := strings.TrimSpace(detail.GetDisplayName())
		if name == "" {
			name = strings.TrimSpace(detail.GetDisplayModelId())
		}
		if name == "" {
			name = id
		}
		models = append(models, Model{ID: id, DisplayName: name, MaxMode: detail.GetMaxMode()})
	}
	return models, nil
}
