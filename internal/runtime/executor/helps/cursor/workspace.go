package cursor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto"
)

const (
	maxReadBytes        = 1 << 20
	maxGrepFileBytes    = 1 << 20
	maxShellOutputBytes = 1 << 20
	maxGrepResults      = 1000
	maxDirectoryNodes   = 1000
	maxDirectoryDepth   = 16
	maxLineOutputBytes  = 16 << 10
	maxShellTimeout     = 30 * time.Minute
)

// HandleWorkspaceExec executes Cursor workspace tools rooted at workspace.
func HandleWorkspaceExec(ctx context.Context, workspace string, request *cursorproto.ExecServerMessage) (*cursorproto.ExecClientMessage, error) {
	if request == nil {
		return nil, fmt.Errorf("cursor workspace tool: exec request is nil")
	}
	root, errRoot := filepath.Abs(workspace)
	if errRoot != nil {
		return nil, fmt.Errorf("cursor workspace tool: resolve workspace: %w", errRoot)
	}
	result := &cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId()}
	switch {
	case request.GetReadArgs() != nil:
		result.Message = &cursorproto.ExecClientMessage_ReadResult{ReadResult: readWorkspaceFile(root, request.GetReadArgs())}
	case request.GetGrepArgs() != nil:
		result.Message = &cursorproto.ExecClientMessage_GrepResult{GrepResult: grepWorkspace(ctx, root, request.GetGrepArgs())}
	case request.GetLsArgs() != nil:
		result.Message = &cursorproto.ExecClientMessage_LsResult{LsResult: listWorkspace(ctx, root, request.GetLsArgs())}
	case request.GetWriteArgs() != nil:
		result.Message = &cursorproto.ExecClientMessage_WriteResult{WriteResult: writeWorkspaceFile(root, request.GetWriteArgs())}
	case request.GetDeleteArgs() != nil:
		result.Message = &cursorproto.ExecClientMessage_DeleteResult{DeleteResult: deleteWorkspaceFile(root, request.GetDeleteArgs())}
	case request.GetShellArgs() != nil:
		result.Message = &cursorproto.ExecClientMessage_ShellResult{ShellResult: runWorkspaceShell(ctx, root, request.GetShellArgs())}
	default:
		return nil, fmt.Errorf("cursor workspace tool: unsupported exec request %d", request.GetId())
	}
	return result, nil
}

func readWorkspaceFile(root string, args *cursorproto.ReadArgs) *cursorproto.ReadResult {
	path, errPath := workspacePath(root, args.GetPath())
	if errPath != nil {
		return &cursorproto.ReadResult{Result: &cursorproto.ReadResult_Rejected{Rejected: &cursorproto.ReadRejected{Path: args.GetPath(), Reason: errPath.Error()}}}
	}
	file, errOpen := openWorkspaceFile(root, path)
	if errOpen != nil {
		if errors.Is(errOpen, fs.ErrNotExist) {
			return &cursorproto.ReadResult{Result: &cursorproto.ReadResult_FileNotFound{FileNotFound: &cursorproto.ReadFileNotFound{Path: path}}}
		}
		return readError(path, errOpen)
	}
	defer func() { _ = file.Close() }()
	info, errStat := file.Stat()
	if errStat != nil {
		return readError(path, errStat)
	}
	if !info.Mode().IsRegular() {
		return &cursorproto.ReadResult{Result: &cursorproto.ReadResult_InvalidFile{InvalidFile: &cursorproto.ReadInvalidFile{Path: path, Reason: "path is not a regular file"}}}
	}
	data, errRead := io.ReadAll(io.LimitReader(file, maxReadBytes+1))
	if errRead != nil {
		return readError(path, errRead)
	}
	truncated := len(data) > maxReadBytes
	if truncated {
		data = data[:maxReadBytes]
	}
	if !utf8.Valid(data) {
		return &cursorproto.ReadResult{Result: &cursorproto.ReadResult_InvalidFile{InvalidFile: &cursorproto.ReadInvalidFile{Path: path, Reason: "file is not valid UTF-8"}}}
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	totalLines := len(lines)
	offset := int(args.GetOffset())
	if offset < 0 {
		offset = 0
	}
	if offset > totalLines {
		offset = totalLines
	}
	limit := int(args.GetLimit())
	if limit <= 0 || limit > maxGrepResults {
		limit = maxGrepResults
	}
	end := offset + limit
	if end > totalLines {
		end = totalLines
	}
	content := strings.Join(lines[offset:end], "\n")
	return &cursorproto.ReadResult{Result: &cursorproto.ReadResult_Success{Success: &cursorproto.ReadSuccess{
		Path:         path,
		TotalLines:   int32(totalLines),
		FileSize:     info.Size(),
		Truncated:    truncated,
		RangeApplied: args.Offset != nil || args.Limit != nil,
		Output:       &cursorproto.ReadSuccess_Content{Content: content},
	}}}
}

func readError(path string, err error) *cursorproto.ReadResult {
	return &cursorproto.ReadResult{Result: &cursorproto.ReadResult_Error{Error: &cursorproto.ReadError{Path: path, Error: err.Error()}}}
}

func grepWorkspace(ctx context.Context, root string, args *cursorproto.GrepArgs) *cursorproto.GrepResult {
	pattern, errCompile := regexp.Compile(args.GetPattern())
	if errCompile != nil {
		return grepError(errCompile)
	}
	if args.CaseInsensitive != nil && args.GetCaseInsensitive() {
		pattern, errCompile = regexp.Compile("(?i)" + args.GetPattern())
		if errCompile != nil {
			return grepError(errCompile)
		}
	}
	path, errPath := workspacePath(root, args.GetPath())
	if errPath != nil {
		return grepError(errPath)
	}
	mode := args.GetOutputMode()
	if mode == "" {
		mode = "content"
	}
	if mode != "content" && mode != "files_with_matches" && mode != "count" {
		return grepError(fmt.Errorf("unsupported output mode %q", mode))
	}
	limit := int(args.GetHeadLimit())
	if limit <= 0 || limit > maxGrepResults {
		limit = maxGrepResults
	}
	matches := make([]*cursorproto.GrepFileMatch, 0)
	files := make([]string, 0)
	counts := make([]*cursorproto.GrepFileCount, 0)
	totalMatches := 0
	truncated := false
	errWalk := filepath.WalkDir(path, func(current string, entry fs.DirEntry, errWalk error) error {
		if errWalk != nil {
			return errWalk
		}
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		if entry.IsDir() {
			if entry.Name() == ".git" && current != path {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !matchesGlob(current, args.GetGlob()) {
			return nil
		}
		data, errRead := readWorkspaceBytes(root, current, maxGrepFileBytes)
		if errRead != nil || len(data) > maxGrepFileBytes || !utf8.Valid(data) {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		fileMatches := make([]*cursorproto.GrepContentMatch, 0)
		for number, line := range lines {
			if !pattern.MatchString(line) {
				continue
			}
			totalMatches++
			if len(matches) < limit {
				fileMatches = append(fileMatches, &cursorproto.GrepContentMatch{LineNumber: int32(number + 1), Content: truncateLine(line)})
			}
		}
		if len(fileMatches) == 0 {
			return nil
		}
		if len(matches) >= limit {
			truncated = true
			return nil
		}
		files = append(files, current)
		counts = append(counts, &cursorproto.GrepFileCount{File: current, Count: int32(len(fileMatches))})
		matches = append(matches, &cursorproto.GrepFileMatch{File: current, Matches: fileMatches})
		return nil
	})
	if errWalk != nil {
		return grepError(errWalk)
	}
	workspace := make(map[string]*cursorproto.GrepUnionResult, 1)
	switch mode {
	case "files_with_matches":
		workspace[root] = &cursorproto.GrepUnionResult{Result: &cursorproto.GrepUnionResult_Files{Files: &cursorproto.GrepFilesResult{Files: files, TotalFiles: int32(len(files)), ClientTruncated: truncated}}}
	case "count":
		workspace[root] = &cursorproto.GrepUnionResult{Result: &cursorproto.GrepUnionResult_Count{Count: &cursorproto.GrepCountResult{Counts: counts, TotalFiles: int32(len(counts)), TotalMatches: int32(totalMatches), ClientTruncated: truncated}}}
	default:
		workspace[root] = &cursorproto.GrepUnionResult{Result: &cursorproto.GrepUnionResult_Content{Content: &cursorproto.GrepContentResult{Matches: matches, TotalLines: int32(totalMatches), TotalMatchedLines: int32(totalMatches), ClientTruncated: truncated}}}
	}
	return &cursorproto.GrepResult{Result: &cursorproto.GrepResult_Success{Success: &cursorproto.GrepSuccess{Pattern: args.GetPattern(), Path: path, OutputMode: mode, WorkspaceResults: workspace}}}
}

func grepError(err error) *cursorproto.GrepResult {
	return &cursorproto.GrepResult{Result: &cursorproto.GrepResult_Error{Error: &cursorproto.GrepError{Error: err.Error()}}}
}

func listWorkspace(ctx context.Context, root string, args *cursorproto.LsArgs) *cursorproto.LsResult {
	path, errPath := workspacePath(root, args.GetPath())
	if errPath != nil {
		return &cursorproto.LsResult{Result: &cursorproto.LsResult_Rejected{Rejected: &cursorproto.LsRejected{Path: args.GetPath(), Reason: errPath.Error()}}}
	}
	nodes := 0
	tree, errTree := listDirectory(ctx, path, args.GetIgnore(), 0, &nodes)
	if errTree != nil {
		return &cursorproto.LsResult{Result: &cursorproto.LsResult_Error{Error: &cursorproto.LsError{Path: path, Error: errTree.Error()}}}
	}
	return &cursorproto.LsResult{Result: &cursorproto.LsResult_Success{Success: &cursorproto.LsSuccess{DirectoryTreeRoot: tree}}}
}

func listDirectory(ctx context.Context, path string, ignore []string, depth int, nodes *int) (*cursorproto.LsDirectoryTreeNode, error) {
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}
	entries, errRead := os.ReadDir(path)
	if errRead != nil {
		return nil, errRead
	}
	node := &cursorproto.LsDirectoryTreeNode{AbsPath: path, ChildrenWereProcessed: true, FullSubtreeExtensionCounts: make(map[string]int32)}
	for _, entry := range entries {
		if matchesAnyGlob(entry.Name(), ignore) {
			continue
		}
		*nodes++
		if *nodes > maxDirectoryNodes {
			node.ChildrenWereProcessed = false
			break
		}
		if entry.IsDir() {
			if depth >= maxDirectoryDepth || entry.Name() == ".git" {
				node.ChildrenWereProcessed = false
				continue
			}
			child, errChild := listDirectory(ctx, filepath.Join(path, entry.Name()), ignore, depth+1, nodes)
			if errChild != nil {
				return nil, errChild
			}
			node.ChildrenDirs = append(node.ChildrenDirs, child)
			continue
		}
		if entry.Type().IsRegular() {
			node.ChildrenFiles = append(node.ChildrenFiles, &cursorproto.CursorLsDirectoryTreeNode_File{Name: entry.Name()})
			node.NumFiles++
			extension := strings.ToLower(filepath.Ext(entry.Name()))
			node.FullSubtreeExtensionCounts[extension]++
		}
	}
	return node, nil
}

func writeWorkspaceFile(root string, args *cursorproto.WriteArgs) *cursorproto.WriteResult {
	path, errPath := workspacePath(root, args.GetPath())
	if errPath != nil {
		return &cursorproto.WriteResult{Result: &cursorproto.WriteResult_Rejected{Rejected: &cursorproto.WriteRejected{Path: args.GetPath(), Reason: errPath.Error()}}}
	}
	if errMkdir := mkdirWorkspaceAll(root, path, 0o700); errMkdir != nil {
		return writeError(path, errMkdir)
	}
	data := args.GetFileBytes()
	if len(data) == 0 {
		data = []byte(args.GetFileText())
	}
	if errWrite := writeWorkspaceBytes(root, path, data, 0o600); errWrite != nil {
		return writeError(path, errWrite)
	}
	contentAfterWrite := ""
	if args.GetReturnFileContentAfterWrite() && utf8.Valid(data) {
		contentAfterWrite = string(data)
	}
	lines := 0
	if len(data) > 0 && utf8.Valid(data) {
		lines = len(strings.Split(string(data), "\n"))
		if strings.HasSuffix(string(data), "\n") {
			lines--
		}
	}
	success := &cursorproto.WriteSuccess{Path: path, LinesCreated: int32(lines), FileSize: int32(len(data))}
	if args.GetReturnFileContentAfterWrite() {
		success.FileContentAfterWrite = &contentAfterWrite
	}
	return &cursorproto.WriteResult{Result: &cursorproto.WriteResult_Success{Success: success}}
}

func writeError(path string, err error) *cursorproto.WriteResult {
	return &cursorproto.WriteResult{Result: &cursorproto.WriteResult_Error{Error: &cursorproto.WriteError{Path: path, Error: err.Error()}}}
}

func deleteWorkspaceFile(root string, args *cursorproto.DeleteArgs) *cursorproto.DeleteResult {
	path, errPath := workspacePath(root, args.GetPath())
	if errPath != nil {
		return &cursorproto.DeleteResult{Result: &cursorproto.DeleteResult_Rejected{Rejected: &cursorproto.DeleteRejected{Path: args.GetPath(), Reason: errPath.Error()}}}
	}
	info, errStat := statWorkspace(root, path)
	if errStat != nil {
		if errors.Is(errStat, fs.ErrNotExist) {
			return &cursorproto.DeleteResult{Result: &cursorproto.DeleteResult_FileNotFound{FileNotFound: &cursorproto.DeleteFileNotFound{Path: path}}}
		}
		return deleteError(path, errStat)
	}
	if !info.Mode().IsRegular() {
		actualType := "other"
		if info.IsDir() {
			actualType = "directory"
		}
		return &cursorproto.DeleteResult{Result: &cursorproto.DeleteResult_NotFile{NotFile: &cursorproto.DeleteNotFile{Path: path, ActualType: actualType}}}
	}
	previous := []byte(nil)
	if info.Size() <= maxReadBytes {
		data, errRead := readWorkspaceBytes(root, path, maxReadBytes)
		if errRead == nil && utf8.Valid(data) {
			previous = data
		}
	}
	if errRemove := removeWorkspace(root, path); errRemove != nil {
		return deleteError(path, errRemove)
	}
	return &cursorproto.DeleteResult{Result: &cursorproto.DeleteResult_Success{Success: &cursorproto.DeleteSuccess{Path: path, DeletedFile: path, FileSize: info.Size(), PrevContent: string(previous)}}}
}

func deleteError(path string, err error) *cursorproto.DeleteResult {
	return &cursorproto.DeleteResult{Result: &cursorproto.DeleteResult_Error{Error: &cursorproto.DeleteError{Path: path, Error: err.Error()}}}
}

// HandleWorkspaceExecStream executes Cursor's streaming shell protocol. It reports whether request was a streaming shell request.
func HandleWorkspaceExecStream(ctx context.Context, workspace string, request *cursorproto.ExecServerMessage, emit func(*cursorproto.ExecClientMessage) error) (bool, error) {
	if request == nil || request.GetShellStreamArgs() == nil {
		return false, nil
	}
	if emit == nil {
		return true, fmt.Errorf("cursor workspace tool: stream emitter is nil")
	}
	root, errRoot := filepath.Abs(workspace)
	if errRoot != nil {
		return true, fmt.Errorf("cursor workspace tool: resolve workspace: %w", errRoot)
	}
	return true, streamWorkspaceShell(ctx, root, request, request.GetShellStreamArgs(), emit)
}

func streamWorkspaceShell(ctx context.Context, root string, request *cursorproto.ExecServerMessage, args *cursorproto.ShellArgs, emit func(*cursorproto.ExecClientMessage) error) error {
	workingDirectory, errPath := workspacePath(root, args.GetWorkingDirectory())
	if errPath != nil {
		return emitShellStream(request, &cursorproto.ShellStream{Event: &cursorproto.ShellStream_Rejected{Rejected: &cursorproto.ShellRejected{Command: args.GetCommand(), WorkingDirectory: args.GetWorkingDirectory(), Reason: errPath.Error()}}}, emit)
	}
	if args.GetIsBackground() {
		return emitShellStream(request, &cursorproto.ShellStream{Event: &cursorproto.ShellStream_Rejected{Rejected: &cursorproto.ShellRejected{Command: args.GetCommand(), WorkingDirectory: workingDirectory, Reason: "background streaming shell commands are not implemented"}}}, emit)
	}
	timeout := shellTimeout(args)
	executionContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := shellCommand(executionContext, args.GetCommand())
	command.Dir = workingDirectory
	stdout, errStdout := command.StdoutPipe()
	if errStdout != nil {
		return errStdout
	}
	stderr, errStderr := command.StderrPipe()
	if errStderr != nil {
		return errStderr
	}
	if errStart := command.Start(); errStart != nil {
		return emitShellStream(request, &cursorproto.ShellStream{Event: &cursorproto.ShellStream_Rejected{Rejected: &cursorproto.ShellRejected{Command: args.GetCommand(), WorkingDirectory: workingDirectory, Reason: errStart.Error()}}}, emit)
	}
	if errEmit := emitShellStream(request, &cursorproto.ShellStream{Event: &cursorproto.ShellStream_Start{Start: &cursorproto.ShellStreamStart{}}}, emit); errEmit != nil {
		return errEmit
	}

	var outputMu sync.Mutex
	outputSize := 0
	streamErrs := make(chan error, 2)
	pump := func(reader io.Reader, event func(string) *cursorproto.ShellStream) {
		buffer := make([]byte, 8<<10)
		for {
			count, errRead := reader.Read(buffer)
			if count > 0 {
				outputMu.Lock()
				allowed := maxShellOutputBytes - outputSize
				if allowed > count {
					allowed = count
				}
				if allowed > 0 {
					outputSize += allowed
				}
				outputMu.Unlock()
				if allowed > 0 {
					if errEmit := emitShellStream(request, event(string(buffer[:allowed])), emit); errEmit != nil {
						streamErrs <- errEmit
						return
					}
				}
				if allowed < count {
					cancel()
					streamErrs <- fmt.Errorf("cursor shell output exceeds %d byte limit", maxShellOutputBytes)
					return
				}
			}
			if errRead == io.EOF {
				streamErrs <- nil
				return
			}
			if errRead != nil {
				streamErrs <- errRead
				return
			}
		}
	}
	go pump(stdout, func(data string) *cursorproto.ShellStream {
		return &cursorproto.ShellStream{Event: &cursorproto.ShellStream_Stdout{Stdout: &cursorproto.ShellStreamStdout{Data: data}}}
	})
	go pump(stderr, func(data string) *cursorproto.ShellStream {
		return &cursorproto.ShellStream{Event: &cursorproto.ShellStream_Stderr{Stderr: &cursorproto.ShellStreamStderr{Data: data}}}
	})
	waitErr := command.Wait()
	firstPumpErr := <-streamErrs
	secondPumpErr := <-streamErrs
	if firstPumpErr != nil {
		return firstPumpErr
	}
	if secondPumpErr != nil {
		return secondPumpErr
	}
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return waitErr
		}
	}
	return emitShellStream(request, &cursorproto.ShellStream{Event: &cursorproto.ShellStream_Exit{Exit: &cursorproto.ShellStreamExit{Code: uint32(exitCode), Cwd: workingDirectory, Aborted: executionContext.Err() != nil}}}, emit)
}

func emitShellStream(request *cursorproto.ExecServerMessage, stream *cursorproto.ShellStream, emit func(*cursorproto.ExecClientMessage) error) error {
	return emit(&cursorproto.ExecClientMessage{Id: request.GetId(), ExecId: request.GetExecId(), Message: &cursorproto.ExecClientMessage_ShellStream{ShellStream: stream}})
}

func shellTimeout(args *cursorproto.ShellArgs) time.Duration {
	timeout := time.Duration(args.GetTimeout()) * time.Millisecond
	if args.GetHardTimeout() != 0 {
		timeout = time.Duration(args.GetHardTimeout()) * time.Millisecond
	}
	if timeout <= 0 || timeout > maxShellTimeout {
		return maxShellTimeout
	}
	return timeout
}

func runWorkspaceShell(ctx context.Context, root string, args *cursorproto.ShellArgs) *cursorproto.ShellResult {
	if args.GetIsBackground() {
		return &cursorproto.ShellResult{Result: &cursorproto.ShellResult_Rejected{Rejected: &cursorproto.ShellRejected{Command: args.GetCommand(), Reason: "background shell commands are not implemented"}}}
	}
	workingDirectory, errPath := workspacePath(root, args.GetWorkingDirectory())
	if errPath != nil {
		return &cursorproto.ShellResult{Result: &cursorproto.ShellResult_Rejected{Rejected: &cursorproto.ShellRejected{Command: args.GetCommand(), Reason: errPath.Error()}}}
	}
	timeout := shellTimeout(args)
	executionContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := shellCommand(executionContext, args.GetCommand())
	command.Dir = workingDirectory
	output, errRun := command.CombinedOutput()
	if len(output) > maxShellOutputBytes {
		output = output[:maxShellOutputBytes]
	}
	if executionContext.Err() == context.DeadlineExceeded {
		return &cursorproto.ShellResult{Result: &cursorproto.ShellResult_Timeout{Timeout: &cursorproto.ShellTimeout{Command: args.GetCommand(), WorkingDirectory: workingDirectory, TimeoutMs: int32(timeout / time.Millisecond)}}}
	}
	if errRun != nil {
		exitCode := int32(1)
		var exitErr *exec.ExitError
		if errors.As(errRun, &exitErr) {
			exitCode = int32(exitErr.ExitCode())
		}
		return &cursorproto.ShellResult{Result: &cursorproto.ShellResult_Failure{Failure: &cursorproto.ShellFailure{Command: args.GetCommand(), WorkingDirectory: workingDirectory, ExitCode: exitCode, Stderr: string(output)}}}
	}
	return &cursorproto.ShellResult{Result: &cursorproto.ShellResult_Success{Success: &cursorproto.ShellSuccess{Command: args.GetCommand(), WorkingDirectory: workingDirectory, Stdout: string(output)}}}
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command)
	}
	return exec.CommandContext(ctx, "sh", "-lc", command)
}

// ResolveWorkspacePath resolves a requested path while enforcing the workspace boundary.
func ResolveWorkspacePath(root, requested string) (string, error) {
	return workspacePath(root, requested)
}

func workspaceRelative(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path is outside the configured workspace")
	}
	return filepath.ToSlash(rel), nil
}

func openWorkspaceFile(root, path string) (*os.File, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	rel, err := workspaceRelative(root, path)
	if err != nil {
		return nil, err
	}
	return r.Open(rel)
}
func readWorkspaceBytes(root, path string, limit int) ([]byte, error) {
	f, err := openWorkspaceFile(root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(limit)+1))
}
func mkdirWorkspaceAll(root, path string, perm os.FileMode) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	rel, err := workspaceRelative(root, path)
	if err != nil {
		return err
	}
	return r.MkdirAll(filepath.ToSlash(filepath.Join(filepath.Dir(rel), ".")), perm)
}
func writeWorkspaceBytes(root, path string, data []byte, perm os.FileMode) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	rel, err := workspaceRelative(root, path)
	if err != nil {
		return err
	}
	return r.WriteFile(rel, data, perm)
}
func statWorkspace(root, path string) (os.FileInfo, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	rel, err := workspaceRelative(root, path)
	if err != nil {
		return nil, err
	}
	return r.Stat(rel)
}
func removeWorkspace(root, path string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	rel, err := workspaceRelative(root, path)
	if err != nil {
		return err
	}
	return r.Remove(rel)
}

func workspacePath(root, requested string) (string, error) {
	root, errRoot := filepath.EvalSymlinks(root)
	if errRoot != nil {
		return "", fmt.Errorf("resolve workspace: %w", errRoot)
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return root, nil
	}
	path := requested
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	resolved, errResolve := resolvePathSymlinks(path)
	if errResolve != nil {
		return "", errResolve
	}
	rel, errRel := filepath.Rel(root, resolved)
	if errRel != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path is outside the configured workspace")
	}
	return resolved, nil
}

func resolvePathSymlinks(path string) (string, error) {
	for current := path; ; current = filepath.Dir(current) {
		resolved, errResolve := filepath.EvalSymlinks(current)
		if errResolve == nil {
			rel, errRel := filepath.Rel(current, path)
			if errRel != nil {
				return "", errRel
			}
			return filepath.Join(resolved, rel), nil
		}
		if !errors.Is(errResolve, fs.ErrNotExist) {
			return "", errResolve
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errResolve
		}
	}
}

func matchesGlob(path, glob string) bool {
	if glob == "" {
		return true
	}
	matched, errMatch := filepath.Match(glob, filepath.Base(path))
	return errMatch == nil && matched
}

func matchesAnyGlob(name string, globs []string) bool {
	for _, glob := range globs {
		matched, errMatch := filepath.Match(glob, name)
		if errMatch == nil && matched {
			return true
		}
	}
	return false
}

func truncateLine(line string) string {
	if len(line) <= maxLineOutputBytes {
		return line
	}
	return line[:maxLineOutputBytes]
}
