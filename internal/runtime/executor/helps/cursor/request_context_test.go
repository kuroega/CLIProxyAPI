package cursor

import (
	"runtime"
	"testing"
)

func TestWorkspaceRequestContextUsesResolvedWorkspace(t *testing.T) {
	root := t.TempDir()
	result := WorkspaceRequestContext(root)
	success := result.GetSuccess()
	if success == nil || success.GetRequestContext().GetEnv() == nil {
		t.Fatalf("result = %#v", result)
	}
	env := success.GetRequestContext().GetEnv()
	if len(env.GetWorkspacePaths()) != 1 || env.GetWorkspacePaths()[0] != root || env.GetOsVersion() != runtime.GOOS {
		t.Fatalf("environment = %#v", env)
	}
	if runtime.GOOS == "windows" && env.GetShell() != "powershell.exe" {
		t.Fatalf("shell = %q", env.GetShell())
	}
}
