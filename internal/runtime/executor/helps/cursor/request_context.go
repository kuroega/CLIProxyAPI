package cursor

import (
	"fmt"
	"path/filepath"
	"runtime"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
)

// WorkspaceRequestContext returns the non-sensitive local environment Cursor needs for workspace tools.
func WorkspaceRequestContext(workspace string) *cursorproto.RequestContextResult {
	root, errRoot := filepath.Abs(workspace)
	if errRoot != nil {
		return &cursorproto.RequestContextResult{Result: &cursorproto.RequestContextResult_Error{Error: &cursorproto.RequestContextError{Error: fmt.Sprintf("resolve workspace: %v", errRoot)}}}
	}
	resolved, errResolve := workspacePath(root, ".")
	if errResolve != nil {
		return &cursorproto.RequestContextResult{Result: &cursorproto.RequestContextResult_Error{Error: &cursorproto.RequestContextError{Error: errResolve.Error()}}}
	}
	shell := "sh"
	if runtime.GOOS == "windows" {
		shell = "powershell.exe"
	}
	return &cursorproto.RequestContextResult{Result: &cursorproto.RequestContextResult_Success{Success: &cursorproto.RequestContextSuccess{RequestContext: &cursorproto.RequestContext{Env: &cursorproto.RequestContextEnv{OsVersion: runtime.GOOS, WorkspacePaths: []string{resolved}, Shell: shell}}}}}
}
