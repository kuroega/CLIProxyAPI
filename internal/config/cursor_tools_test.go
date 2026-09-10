package config

import (
	"path/filepath"
	"testing"
)

func TestParseCursorToolsDefaultDenyAndNormalization(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("cursor-tools:\n  workspace: '  " + filepath.ToSlash(t.TempDir()) + "  '\n  files: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CursorTools.Files || cfg.CursorTools.Shell || cfg.CursorTools.MCP {
		t.Fatalf("policy = %#v", cfg.CursorTools)
	}
	if cfg.CursorTools.Workspace == "" || cfg.CursorTools.Workspace[0] == ' ' {
		t.Fatalf("workspace not normalized: %q", cfg.CursorTools.Workspace)
	}
	empty, err := ParseConfigBytes([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if empty.CursorTools.Files || empty.CursorTools.Shell || empty.CursorTools.MCP {
		t.Fatalf("default policy is not deny-all: %#v", empty.CursorTools)
	}
}
