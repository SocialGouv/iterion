package tool

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	workspaceGrepMaxResults        = 1000
	workspaceGrepMaxOutputBytes    = 240 * 1024
	workspaceGrepMaxVisitedEntries = 10_000
	workspaceGrepMaxScannedBytes   = 64 * 1024 * 1024
)

type workspaceGrepLimits struct {
	maxResults, maxOutputBytes         int
	maxVisitedEntries, maxScannedBytes int
}

var defaultWorkspaceGrepLimits = workspaceGrepLimits{
	maxResults: workspaceGrepMaxResults, maxOutputBytes: workspaceGrepMaxOutputBytes,
	maxVisitedEntries: workspaceGrepMaxVisitedEntries, maxScannedBytes: workspaceGrepMaxScannedBytes,
}

var errGrepInputLimit = errors.New("grep input byte limit reached")

func executeWorkspaceGrep(ctx context.Context, input map[string]any, workspace string) (string, error) {
	return executeWorkspaceGrepWithLimits(ctx, input, workspace, defaultWorkspaceGrepLimits)
}

func executeWorkspaceGrepWithLimits(ctx context.Context, input map[string]any, workspace string, limits workspaceGrepLimits) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	pattern, ok := input["pattern"].(string)
	if !ok || pattern == "" {
		return "", fmt.Errorf("grep: 'pattern' input is required and must be a string")
	}
	rawPath, err := workspacePathInput(input, "grep")
	if err != nil {
		return "", err
	}
	if sensitiveWorkspacePath(rawPath) {
		return "", fmt.Errorf("grep: %q is excluded as a credential or secret file", rawPath)
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
	// Old injected result/output limits remain strict; omitted new work limits
	// inherit bounded defaults instead of accidentally disabling protection.
	if limits.maxVisitedEntries <= 0 {
		limits.maxVisitedEntries = workspaceGrepMaxVisitedEntries
	}
	if limits.maxScannedBytes <= 0 {
		limits.maxScannedBytes = workspaceGrepMaxScannedBytes
	}
	resultReason := fmt.Sprintf("result limit (%d)", limits.maxResults)
	outputReason := fmt.Sprintf("output limit (%d bytes)", limits.maxOutputBytes)
	entryReason := fmt.Sprintf("entry limit (%d)", limits.maxVisitedEntries)
	inputReason := fmt.Sprintf("input limit (%d bytes)", limits.maxScannedBytes)
	markerReserve := 0
	for _, reason := range []string{resultReason, outputReason, entryReason, inputReason} {
		markerReserve = max(markerReserve, len(workspaceGrepPartialMarker(reason)))
	}
	contentBudget := max(0, limits.maxOutputBytes-markerReserve-1)
	var results []string
	outputBytes, remainingInput := 0, limits.maxScannedBytes
	truncated := ""
	visit := func(path string, info os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if sensitiveWorkspacePath(path) || info.IsDir() && sensitiveWorkspacePath(filepath.Join(path, "_")) {
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
		// Lstat semantics preserve the exclusion of symlinks, FIFOs and devices.
		if !info.Mode().IsRegular() {
			return nil
		}
		if globFilter != "" {
			matched, err := filepath.Match(globFilter, filepath.Base(path))
			if err != nil || !matched {
				return nil
			}
		}
		if remainingInput == 0 {
			truncated = inputReason
			return filepath.SkipAll
		}
		f, err := openWorkspaceFile(workspace, path)
		if err != nil {
			return nil
		}
		reader := &grepBudgetReader{ctx: ctx, source: f, remaining: &remainingInput}
		matches, consumed, reason, scanErr := grepWorkspaceReader(reader, re, path, limits.maxResults-len(results), contentBudget-outputBytes, len(results) > 0, resultReason, outputReason)
		_ = f.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(scanErr, errGrepInputLimit) {
			reason = inputReason
		} else if scanErr != nil {
			// An ordinary file error still spent input budget, even though its
			// matches are skipped as before.
			if remainingInput == 0 {
				truncated = inputReason
				return filepath.SkipAll
			}
			return nil
		}
		if reason == "" && remainingInput == 0 && !reader.eof {
			reason = inputReason
		}
		results = append(results, matches...)
		outputBytes += consumed
		if reason != "" {
			truncated = reason
			return filepath.SkipAll
		}
		return nil
	}
	if !info.IsDir() && sensitiveWorkspacePath(searchPath) {
		return "", fmt.Errorf("path is a protected credential file")
	}
	entryLimited, err := walkWorkspaceGrep(ctx, searchPath, info, limits.maxVisitedEntries, visit)
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return "", fmt.Errorf("grep walk: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	if truncated == "" && entryLimited {
		truncated = entryReason
	}
	if len(results) == 0 && truncated == "" {
		return fmt.Sprintf("No matches found for pattern: %s", pattern), nil
	}
	output := strings.Join(results, "\n")
	if truncated != "" {
		marker := workspaceGrepPartialMarker(truncated)
		if len(marker) > limits.maxOutputBytes {
			return "", fmt.Errorf("grep: output limit is too small to report partial results (%s)", truncated)
		}
		if output != "" {
			output += "\n"
		}
		output += marker
	}
	return output, nil
}

func workspaceGrepPartialMarker(reason string) string {
	return fmt.Sprintf("... [grep partial: stopped at %s; narrow path or glob and continue]", reason)
}

// Count directory entries as they are enumerated, even if later excluded.
// All retained names across recursive calls share one budget. A directory
// that fits is visited lexically; an oversized directory yields an explicitly
// partial subset, without pretending it is the global lexical prefix.
func walkWorkspaceGrep(ctx context.Context, root string, info os.FileInfo, limit int, visit func(string, os.FileInfo) error) (bool, error) {
	remaining := limit - 1 // The root itself is an entry.
	limited := false
	var walk func(string, os.FileInfo) error
	walk = func(path string, fi os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(path, fi); err != nil {
			if err == filepath.SkipDir {
				return nil
			}
			return err
		}
		if !fi.IsDir() {
			return nil
		}
		if remaining == 0 {
			limited = true
			return nil
		}
		dir, err := os.Open(path)
		if err != nil {
			return nil
		}
		var names []string
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				_ = dir.Close()
				return err
			}
			batch, readErr := dir.Readdirnames(min(128, remaining))
			names = append(names, batch...)
			remaining -= len(batch)
			if readErr != nil {
				break
			}
			if remaining == 0 {
				limited = true
			}
		}
		_ = dir.Close()
		slices.Sort(names)
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			child := filepath.Join(path, name)
			childInfo, err := os.Lstat(child)
			if err != nil {
				continue
			}
			if err := walk(child, childInfo); err != nil {
				return err
			}
		}
		return nil
	}
	err := walk(root, info)
	return limited, err
}

// Charge actual reads, including scanner read-ahead and nonmatching data.
// No extra EOF probe is permitted after the shared allowance reaches zero.
type grepBudgetReader struct {
	ctx       context.Context
	source    io.Reader
	remaining *int
	eof       bool
}

func (r *grepBudgetReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.eof {
		return 0, io.EOF
	}
	if *r.remaining == 0 {
		return 0, errGrepInputLimit
	}
	p = p[:min(len(p), *r.remaining)]
	n, err := r.source.Read(p)
	*r.remaining -= n
	if errors.Is(err, io.EOF) {
		r.eof = true
	}
	return n, err
}

func grepWorkspaceReader(r *grepBudgetReader, re *regexp.Regexp, path string, remainingResults, remainingBytes int, hasPriorResults bool, resultReason, outputReason string) ([]string, int, string, error) {
	if remainingResults <= 0 {
		return nil, 0, resultReason, nil
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		// Scanner calls its splitter with atEOF after ANY read error. Only
		// true EOF completes an unterminated line; a budget cut must not.
		if atEOF && !r.eof && bytes.IndexByte(data, '\n') < 0 {
			return 0, nil, nil
		}
		return bufio.ScanLines(data, atEOF)
	})
	var results []string
	consumed := 0
	for line := 1; scanner.Scan(); line++ {
		if err := r.ctx.Err(); err != nil {
			return nil, 0, "", err
		}
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
