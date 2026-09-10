package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
)

func TestConfigSynthesizerCursorKeys(t *testing.T) {
	weight := 3
	ctx := &SynthesisContext{
		Config: &config.Config{CursorKey: []config.CursorKey{{
			APIKey: "cursor-token", BaseURL: "https://api2.cursor.sh", Prefix: "cursor", Weight: &weight,
			Models: []config.CursorModel{{Name: "cursor-model", Alias: "cursor-alias"}},
		}}},
		Now: time.Unix(1, 0), IDGenerator: NewStableIDGenerator(),
	}
	auths, err := NewConfigSynthesizer().Synthesize(ctx)
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d", len(auths))
	}
	auth := auths[0]
	if auth.Provider != constant.Cursor || auth.Attributes["api_key"] != "cursor-token" || auth.Attributes["base_url"] != "https://api2.cursor.sh" {
		t.Fatalf("auth = %#v", auth)
	}
	if auth.Attributes["models_hash"] == "" || auth.Attributes["weight"] != "3" {
		t.Fatalf("attributes = %#v", auth.Attributes)
	}
}
