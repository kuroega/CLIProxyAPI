package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestPutCursorKeysValidatesReplacement(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantKey    string
	}{
		{"array", `[{"api-key":" new-key ","base-url":"https://api2.cursor.sh"}]`, 200, "new-key"},
		{"wrapper", `{"items":[{"api-key":"new-key","base-url":"https://api2.cursor.sh"}]}`, 200, "new-key"},
		{"empty array", `[]`, 200, ""},
		{"empty wrapper", `{"items":[]}`, 200, ""},
		{"missing items", `{}`, 400, "existing-key"},
		{"unknown field", `{"item":[]}`, 400, "existing-key"},
		{"null", `null`, 400, "existing-key"},
		{"null items", `{"items":null}`, 400, "existing-key"},
		{"invalid JSON", `{`, 400, "existing-key"},
		{"invalid weight", `[{"api-key":"new-key","base-url":"https://api2.cursor.sh","weight":1000001}]`, 400, "existing-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{cfg: &config.Config{CursorKey: []config.CursorKey{{APIKey: "existing-key"}}}, configFilePath: writeTestConfigFile(t)}
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/cursor-api-key", strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			h.PutCursorKeys(ctx)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.wantKey == "" {
				if len(h.cfg.CursorKey) != 0 {
					t.Fatalf("keys = %#v, want empty", h.cfg.CursorKey)
				}
			} else if len(h.cfg.CursorKey) != 1 || h.cfg.CursorKey[0].APIKey != tc.wantKey {
				t.Fatalf("keys = %#v, want %q", h.cfg.CursorKey, tc.wantKey)
			}
		})
	}
}
