package cliproxy

import (
	"testing"

	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestCursorExecutorRegistrationReusesUnchangedConfig(t *testing.T) {
	service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	auth := &coreauth.Auth{ID: "cursor-a", Provider: "cursor"}
	service.ensureExecutorsForAuth(auth)
	first, _ := service.coreManager.Executor("cursor")
	service.ensureExecutorsForAuth(&coreauth.Auth{ID: "cursor-b", Provider: "cursor"})
	second, _ := service.coreManager.Executor("cursor")
	if first != second {
		t.Fatal("registering a second credential replaced the live Cursor executor")
	}
	service.cfg = &config.Config{CursorTools: config.CursorToolsConfig{Workspace: t.TempDir(), Files: true}, CursorMCP: config.CursorMCPConfig{Servers: map[string]config.CursorMCPServer{"new": {Enabled: true, Executable: "test"}}}}
	service.ensureExecutorsForAuth(auth)
	replacement, _ := service.coreManager.Executor("cursor")
	if replacement == first {
		t.Fatal("configuration change retained the old Cursor executor")
	}
	if _, ok := replacement.(*runtimeexecutor.CursorExecutor); !ok {
		t.Fatalf("replacement = %T", replacement)
	}
	if _, ok := first.(coreauth.ExecutionSessionCloser); !ok {
		t.Fatal("replaced Cursor executor cannot close its sessions")
	}
}
