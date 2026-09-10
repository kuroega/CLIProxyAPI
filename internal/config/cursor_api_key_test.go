package config

import "testing"

func TestCursorAPIKeyConfigNormalization(t *testing.T) {
	weight := 2
	cfg := &Config{CursorKey: []CursorKey{{
		APIKey:         " cursor-key ",
		BaseURL:        " https://api2.cursor.sh/ ",
		ProxyURL:       " https://proxy.example ",
		Prefix:         " team ",
		Headers:        map[string]string{" X-Trace ": " value "},
		ExcludedModels: []string{" model-a ", "model-a"},
		Weight:         &weight,
	}, {APIKey: "missing-url"}}}

	cfg.SanitizeCursorKeys()
	if len(cfg.CursorKey) != 1 {
		t.Fatalf("CursorKey length = %d, want 1", len(cfg.CursorKey))
	}
	key := cfg.CursorKey[0]
	if key.APIKey != "cursor-key" || key.BaseURL != "https://api2.cursor.sh/" || key.ProxyURL != "https://proxy.example" || key.Prefix != "team" {
		t.Fatalf("normalized cursor key = %+v", key)
	}
	if got := key.Headers["X-Trace"]; got != "value" {
		t.Fatalf("normalized header = %q, want value", got)
	}
	if len(key.ExcludedModels) != 1 || key.ExcludedModels[0] != "model-a" {
		t.Fatalf("normalized exclusions = %#v", key.ExcludedModels)
	}
	if errValidate := cfg.ValidateCredentialWeights(); errValidate != nil {
		t.Fatalf("ValidateCredentialWeights() error = %v", errValidate)
	}
}
