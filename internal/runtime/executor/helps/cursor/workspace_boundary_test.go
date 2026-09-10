package cursor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
)

func TestWorkspaceGrepSkipsOversizedFiles(t *testing.T) {
	root := t.TempDir()
	data := make([]byte, maxGrepFileBytes+1)
	copy(data, []byte("needle"))
	if err := os.WriteFile(filepath.Join(root, "large.txt"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	req := &cursorproto.ExecServerMessage{Message: &cursorproto.ExecServerMessage_GrepArgs{GrepArgs: &cursorproto.GrepArgs{Pattern: "needle"}}}
	got, err := HandleWorkspaceExec(context.Background(), root, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetGrepResult().GetSuccess().GetWorkspaceResults()[root].GetContent().GetMatches()) != 0 {
		t.Fatalf("oversized file was searched")
	}
}

func TestWorkspaceWriteRejectsSymlinkParent(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	req := &cursorproto.ExecServerMessage{Message: &cursorproto.ExecServerMessage_WriteArgs{WriteArgs: &cursorproto.WriteArgs{Path: "link/escape.txt", FileText: "no"}}}
	got, err := HandleWorkspaceExec(context.Background(), root, req)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetWriteResult().GetSuccess() != nil {
		t.Fatalf("write through symlink unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file created: %v", err)
	}
}
