package cursor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
	"google.golang.org/protobuf/proto"
)

func TestFetchUsableModelsUsesConnectProtocol(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != usableModelsPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Connect-Protocol-Version") != "1" || r.Header.Get("Content-Type") != "application/proto" || r.Header.Get("X-Cursor-Client-Version") != DefaultClientVersion {
			t.Errorf("headers = %#v", r.Header)
		}
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read request: %v", errRead)
		}
		var request cursorproto.GetUsableModelsRequest
		if errDecode := proto.Unmarshal(body, &request); errDecode != nil {
			t.Fatalf("decode request: %v", errDecode)
		}
		payload, errMarshal := proto.Marshal(&cursorproto.GetUsableModelsResponse{Models: []*cursorproto.ModelDetails{{ModelId: "model-a", DisplayName: "Model A"}, {ModelId: "model-b", DisplayModelId: "Model B", MaxMode: proto.Bool(true)}}})
		if errMarshal != nil {
			t.Fatalf("marshal response: %v", errMarshal)
		}
		w.Header().Set("Content-Type", "application/proto")
		if _, errWrite := w.Write(payload); errWrite != nil {
			t.Fatalf("write response: %v", errWrite)
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	models, err := FetchUsableModels(t.Context(), server.Client(), server.URL, "token")
	if err != nil {
		t.Fatalf("FetchUsableModels() error = %v", err)
	}
	if len(models) != 2 || models[0].ID != "model-a" || models[1].DisplayName != "Model B" || !models[1].MaxMode {
		t.Fatalf("models = %#v", models)
	}
}
