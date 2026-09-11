package tool

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"
)

// Keep a read result below the 256 KiB transport/sink boundary used by a few
// agent providers. The continuation marker is part of the result, so a large
// file can never look complete merely because a downstream clipped its tail.
const workspaceReadMaxBytes = 240 * 1024

// A regular file past this size is not something to page through 240 KiB
// at a time, and counting its lines for the continuation marker would
// stream it all through the agent's turn. Refuse it with an actionable
// error — grep and glob still reach it.
const workspaceReadMaxFileBytes = 64 * 1024 * 1024

const (
	workspaceGrepMaxResults     = 1000
	workspaceGrepMaxOutputBytes = 240 * 1024
)

type workspaceGrepLimits struct {
	maxResults     int
	maxOutputBytes int
}

var defaultWorkspaceGrepLimits = workspaceGrepLimits{
	maxResults:     workspaceGrepMaxResults,
	maxOutputBytes: workspaceGrepMaxOutputBytes,
}

func workspaceReadFileTool() api.Tool {
	t := clawtools.ReadFileTool()
	t.Description = "Read a file inside the active workspace. Credential files and internal run stores are excluded. " +
		"Large files are returned in explicit line chunks; " +
		"when the result is partial, call it again with the next start_line printed in the marker."
	t.InputSchema.Properties["start_line"] = api.Property{
		Type:        "integer",
		Description: "1-based first line to return (optional, defaults to 1)",
	}
	t.InputSchema.Properties["line_count"] = api.Property{
		Type:        "integer",
		Description: "Maximum number of lines to return (optional; the byte safety cap still applies)",
	}
	return t
}

func workspaceGrepTool() api.Tool {
	t := clawtools.GrepTool()
	t.Name = "workspace_grep"
	t.Description = "Search file contents inside the active workspace. Credential files and internal run stores are excluded. " +
		"The path is optional; an omitted or empty path searches the active workspace root. " +
		"Use this to discover source and project artifacts by content, then read the matching files."
	// The handler has an explicit active-workspace default for the optional
	// path. Keep the public schema in agreement so providers do not reject the
	// same call before it reaches the handler.
	t.InputSchema.Required = []string{"pattern"}
	return t
}

func diagnosticShellTool() api.Tool {
	t := clawtools.BashTool()
	t.Name = "diagnostic_shell"
	t.Description = "Run an exceptional diagnostic shell command after explicit operator approval. " +
		"Use only when workspace read/search and host run tools cannot answer the question; prefer bounded read-only commands."
	return t
}

func executeWorkspaceReadFile(input map[string]any, workspace string) (string, error) {
	rawPath, ok := input["path"].(string)
	if !ok || strings.TrimSpace(rawPath) == "" {
		return "", fmt.Errorf("read_file: 'path' input is required and must be a string")
	}
	// Same boundary as workspace_grep and glob: the path is resolved and
	// contained inside the active workspace, and a credential file is
	// refused. Without this an absolute path skipped the join entirely and
	// only got filepath.Clean, so the model could read ~/.ssh/id_rsa or
	// ~/.iterion/secrets.json through the one read tool that had no guard.
	path, err := resolveWorkspacePath(workspace, rawPath)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	if sensitiveWorkspacePath(path) {
		return "", fmt.Errorf("read_file: %q is excluded as a credential or secret file", rawPath)
	}

	start, err := positiveIntInput(input, "start_line", 1)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	lineCount, err := nonNegativeIntInput(input, "line_count")
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}

	lines, total, err := workspaceFileWindow(path, start, workspaceReadMaxBytes)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	if total == 0 {
		return "", nil
	}
	if start > total {
		return "", fmt.Errorf("start_line %d is past end of file (%d lines)", start, total)
	}

	var out strings.Builder
	end := start - 1
	byteLimited := false
	for i, line := range lines {
		if lineCount > 0 && i >= lineCount {
			break
		}
		if out.Len()+len(line) > workspaceReadMaxBytes {
			byteLimited = true
			if out.Len() == 0 {
				room := workspaceReadMaxBytes
				for room > 0 && room < len(line) && !utf8.RuneStart(line[room]) {
					room--
				}
				out.WriteString(line[:room])
				return out.String() + fmt.Sprintf("\n\n[read_file partial: line %d exceeds the %d-byte chunk cap; reformat or search this file instead]", start, workspaceReadMaxBytes), nil
			}
			break
		}
		out.WriteString(line)
		end = start + i
	}

	partial := end < total
	if partial {
		reason := "line_count reached"
		if byteLimited {
			reason = "byte cap reached"
		}
		fmt.Fprintf(&out, "\n\n[read_file partial: lines %d-%d of %d; %s; continue with path %q and start_line %d]",
			start, end, total, reason, rawPath, end+1)
	}
	return out.String(), nil
}

// workspaceFileWindow streams path once and returns the lines from `start`
// on — retaining at most maxBytes+1 bytes overall, and at most maxBytes+1
// bytes of any single line — together with the file's total line count.
// Lines keep their trailing newline.
//
// The retention rule is what makes the caller's chunk cap bound the READ
// and not merely the output: os.ReadFile used to pull the whole file into
// memory first, so a large file in the workspace cost the host process its
// address space on a single model tool call. Retaining one line PAST the
// cap is deliberate — it is what lets the caller distinguish "byte cap
// reached" from "that was the whole file".
func workspaceFileWindow(path string, start, maxBytes int) ([]string, int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	// A FIFO blocks os.Open forever and a character device never ends;
	// neither is a workspace file the agent has any business reading.
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%q is not a regular file", filepath.Base(path))
	}
	if info.Size() > workspaceReadMaxFileBytes {
		return nil, 0, fmt.Errorf("%q is %d bytes, past the %d-byte read ceiling; search it with grep instead",
			filepath.Base(path), info.Size(), int64(workspaceReadMaxFileBytes))
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64*1024)
	var window []string
	retained, total := 0, 0
	for {
		line, readErr := readLineCapped(reader, maxBytes+1)
		if readErr != nil && readErr != io.EOF {
			return nil, 0, readErr
		}
		if line != "" {
			total++
			if total >= start && retained <= maxBytes {
				window = append(window, line)
				retained += len(line)
			}
		}
		if readErr == io.EOF {
			return window, total, nil
		}
	}
}

// readLineCapped reads one '\n'-terminated line (delimiter included),
// retaining at most limit bytes of it and discarding the rest — the
// caller's own chunk cap would have cut an over-long line anyway, and
// keeping it whole is how one pathological line grows the process.
func readLineCapped(r *bufio.Reader, limit int) (string, error) {
	var b strings.Builder
	for {
		chunk, err := r.ReadSlice('\n')
		if room := limit - b.Len(); room > 0 {
			if room > len(chunk) {
				room = len(chunk)
			}
			b.Write(chunk[:room])
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return b.String(), err
	}
}

func executeWorkspaceGrep(input map[string]any, workspace string) (string, error) {
	return executeWorkspaceGrepWithLimits(input, workspace, defaultWorkspaceGrepLimits)
}

func executeWorkspaceGrepWithLimits(input map[string]any, workspace string, limits workspaceGrepLimits) (string, error) {
	pattern, ok := input["pattern"].(string)
	if !ok || pattern == "" {
		return "", fmt.Errorf("grep: 'pattern' input is required and must be a string")
	}
	rawPath, err := workspacePathInput(input, "grep")
	if err != nil {
		return "", err
	}
	searchPath, err := resolveWorkspacePath(workspace, rawPath)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("grep: invalid pattern: %w", err)
	}
	globFilter, _ := input["glob"].(string)

	info, err := os.Stat(searchPath)
	if err != nil {
		return "", fmt.Errorf("grep: stat path: %w", err)
	}
	if limits.maxResults <= 0 || limits.maxOutputBytes <= 0 {
		return "", fmt.Errorf("grep: result and output limits must be positive")
	}
	resultReason := fmt.Sprintf("result limit (%d)", limits.maxResults)
	outputReason := fmt.Sprintf("output limit (%d bytes)", limits.maxOutputBytes)
	markerReserve := max(
		len(workspaceGrepPartialMarker(resultReason)),
		len(workspaceGrepPartialMarker(outputReason)),
	)
	contentBudget := max(0, limits.maxOutputBytes-markerReserve-1)
	var results []string
	outputBytes := 0
	truncated := ""
	visit := func(path string, info os.FileInfo) error {
		if sensitiveWorkspacePath(path) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			if path != searchPath && ignoredSearchDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		// filepath.Walk lstats, so a symlink is reported here as a plain
		// non-dir entry — and os.Open below follows it. That re-opened both
		// guards this tool advertises: a `notes.txt -> ~/.ssh/id_rsa` link
		// escapes containment, and sensitiveWorkspacePath above judged the
		// LINK's name rather than the target's. A FIFO in the tree is the
		// other half: os.Open blocks on it until a writer appears, hanging
		// the agent's turn. Only regular files are searchable.
		if !info.Mode().IsRegular() {
			return nil
		}
		if globFilter != "" {
			matched, matchErr := filepath.Match(globFilter, filepath.Base(path))
			if matchErr != nil || !matched {
				return nil
			}
		}
		matches, consumed, reason, scanErr := grepWorkspaceFile(
			re,
			path,
			limits.maxResults-len(results),
			contentBudget-outputBytes,
			len(results) > 0,
			resultReason,
			outputReason,
		)
		if scanErr != nil {
			return nil
		}
		results = append(results, matches...)
		outputBytes += consumed
		if reason != "" {
			truncated = reason
			return filepath.SkipAll
		}
		return nil
	}

	if info.IsDir() {
		err = filepath.Walk(searchPath, func(path string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			return visit(path, fi)
		})
		if err != nil && err != filepath.SkipAll {
			return "", fmt.Errorf("grep walk: %w", err)
		}
	} else if sensitiveWorkspacePath(searchPath) {
		return "", fmt.Errorf("path is a protected credential file")
	} else if err := visit(searchPath, info); err != nil && err != filepath.SkipAll {
		return "", err
	}

	if len(results) == 0 && truncated == "" {
		return fmt.Sprintf("No matches found for pattern: %s", pattern), nil
	}
	output := strings.Join(results, "\n")
	if truncated != "" {
		if output != "" {
			output += "\n"
		}
		output += workspaceGrepPartialMarker(truncated)
	}
	return output, nil
}

func workspaceGrepPartialMarker(reason string) string {
	return fmt.Sprintf("... [grep partial: stopped at %s; narrow path or glob and continue]", reason)
}

// workspacePathInput applies the workspace-scoped path contract shared by
// grep and glob: an omitted or exactly empty path means the active workspace
// root. Whitespace-only values remain invalid instead of being silently
// trimmed into a root request.
func workspacePathInput(input map[string]any, toolName string) (string, error) {
	rawPath := "."
	supplied, ok := input["path"]
	if !ok {
		return rawPath, nil
	}
	path, ok := supplied.(string)
	if !ok {
		return "", fmt.Errorf("%s: 'path' must be a string when provided", toolName)
	}
	if path == "" {
		return rawPath, nil
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s: 'path' must be a non-empty string when provided", toolName)
	}
	return path, nil
}

func grepWorkspaceFile(
	re *regexp.Regexp,
	path string,
	remainingResults int,
	remainingBytes int,
	hasPriorResults bool,
	resultReason string,
	outputReason string,
) ([]string, int, string, error) {
	if remainingResults <= 0 {
		return nil, 0, resultReason, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	results := make([]string, 0)
	consumed := 0
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if strings.IndexByte(text, 0) >= 0 {
			return results, consumed, "", nil
		}
		if re.MatchString(text) {
			candidate := fmt.Sprintf("%s:%d:%s", path, line, text)
			separator := 0
			if hasPriorResults || len(results) > 0 {
				separator = 1
			}
			if separator+len(candidate) > remainingBytes-consumed {
				return results, consumed, outputReason, nil
			}
			results = append(results, candidate)
			consumed += separator + len(candidate)
			if len(results) >= remainingResults {
				return results, consumed, resultReason, nil
			}
		}
	}
	return results, consumed, "", scanner.Err()
}

func resolveWorkspacePath(workspace, raw string) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", fmt.Errorf("active workspace is required")
	}
	path := raw
	if !filepath.IsAbs(path) && workspace != "" {
		path = filepath.Join(workspace, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside workspace", raw)
	}
	return resolved, nil
}

func sensitiveWorkspacePath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	base := filepath.Base(clean)
	if base == ".env" || strings.HasPrefix(base, ".env.") || base == ".netrc" || base == ".git-credentials" || base == "cli-auth.json" {
		return true
	}
	if strings.Contains(base, "id_rsa") || strings.Contains(base, "id_ed25519") {
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".pem", ".key", ".p12":
		return true
	}
	for _, fragment := range []string{
		"/.ssh/", "/.aws/", "/.gnupg/", "/.docker/config.json",
		"/.claude/.credentials.json", "/.codex/auth.json",
		"/.iterion/secrets.json", "/.iterion/secrets.key",
	} {
		if strings.Contains("/"+clean, fragment) {
			return true
		}
	}
	return false
}

func ignoredSearchDir(name string) bool {
	switch name {
	case ".git", ".iterion", ".venv", "node_modules", "vendor":
		return true
	default:
		return false
	}
}

func positiveIntInput(input map[string]any, key string, def int) (int, error) {
	v, ok := input[key]
	if !ok {
		return def, nil
	}
	n, ok := jsonNumberAsInt(v)
	if !ok || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

func nonNegativeIntInput(input map[string]any, key string) (int, error) {
	v, ok := input[key]
	if !ok {
		return 0, nil
	}
	n, ok := jsonNumberAsInt(v)
	if !ok || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return n, nil
}

func jsonNumberAsInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}
