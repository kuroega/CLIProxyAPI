package cursor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
)

// ToolWorkspace checks explicit grants before any workspace or process access.
func ToolWorkspace(policy config.CursorToolsConfig, request *cursorproto.ExecServerMessage) (string, error) {
	if request == nil {
		return "", fmt.Errorf("Cursor tool request is missing")
	}
	allowed := false
	switch {
	case request.GetReadArgs() != nil, request.GetWriteArgs() != nil, request.GetDeleteArgs() != nil, request.GetLsArgs() != nil, request.GetGrepArgs() != nil:
		allowed = policy.Files
	case request.GetShellArgs() != nil, request.GetShellStreamArgs() != nil, request.GetBackgroundShellSpawnArgs() != nil, request.GetWriteShellStdinArgs() != nil, request.GetForceBackgroundShellArgs() != nil:
		allowed = policy.Shell
	case request.GetMcpArgs() != nil, request.GetMcpStateExecArgs() != nil, request.GetListMcpResourcesExecArgs() != nil:
		allowed = policy.MCP
	case request.GetReadMcpResourceExecArgs() != nil:
		allowed = policy.MCP && (request.GetReadMcpResourceExecArgs().GetDownloadPath() == "" || policy.Files)
	case request.GetRequestContextArgs() != nil:
		allowed = policy.Files || policy.Shell || policy.MCP
	}
	if !allowed {
		return "", fmt.Errorf("Cursor tool is disabled by cursor-tools policy")
	}
	workspace := strings.TrimSpace(policy.Workspace)
	if workspace == "" || !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("Cursor tools require an explicit absolute workspace")
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("Cursor workspace is unavailable: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("Cursor workspace is unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Cursor workspace must be a directory")
	}
	return resolved, nil
}
