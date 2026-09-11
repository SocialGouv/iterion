package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"
)

// A glob result is delivered in a model turn, so keep it below the same
// transport boundary as workspace reads. The visited-entry limit is separate:
// a sparse pattern must not make an unbounded walk cheap merely because it
// produces few matches.
const (
	workspaceGlobMaxResults        = 1_000
	workspaceGlobMaxOutputBytes    = 240 * 1024
	workspaceGlobMaxVisitedEntries = 10_000
)

type workspaceGlobLimits struct {
	maxResults        int
	maxOutputBytes    int
	maxVisitedEntries int
}

var defaultWorkspaceGlobLimits = workspaceGlobLimits{
	maxResults:        workspaceGlobMaxResults,
	maxOutputBytes:    workspaceGlobMaxOutputBytes,
	maxVisitedEntries: workspaceGlobMaxVisitedEntries,
}

func workspaceGlobTool() api.Tool {
	t := clawtools.GlobTool()
	t.Description = "Find files inside the active workspace by name or layout. " +
		"Recursive searches are bounded; credential files and internal run stores are excluded. " +
		"The path is optional; an omitted or empty path searches the active workspace root. " +
		"Use a narrow path whenever you know the bundle or directory."
	return t
}

func executeWorkspaceGlob(ctx context.Context, input map[string]any, workspace string) (string, error) {
	return executeWorkspaceGlobWithLimits(ctx, input, workspace, defaultWorkspaceGlobLimits)
}

func executeWorkspaceGlobWithLimits(ctx context.Context, input map[string]any, workspace string, limits workspaceGlobLimits) (string, error) {
	pattern, ok := input["pattern"].(string)
	if !ok || strings.TrimSpace(pattern) == "" {
		return "", fmt.Errorf("glob: 'pattern' input is required and must be a string")
	}
	if err := validateWorkspaceGlobPattern(pattern); err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}

	rawPath, err := workspacePathInput(input, "glob")
	if err != nil {
		return "", err
	}
	searchPath, err := resolveWorkspacePath(workspace, rawPath)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	info, err := os.Stat(searchPath)
	if err != nil {
		return "", fmt.Errorf("glob: stat path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("glob: path %q is not a directory", rawPath)
	}
	if sensitiveWorkspacePath(searchPath) {
		return "", fmt.Errorf("glob: path is a protected credential directory")
	}

	root := searchPath
	if workspace != "" {
		root, err = filepath.EvalSymlinks(workspace)
		if err != nil {
			return "", fmt.Errorf("glob: resolve workspace: %w", err)
		}
	}

	var (
		matches    []string
		outputSize int
		visited    int
		truncated  string
	)
	err = filepath.Walk(searchPath, func(path string, info os.FileInfo, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil || info == nil {
			return nil
		}
		visited++
		if visited > limits.maxVisitedEntries {
			truncated = fmt.Sprintf("visited-entry limit (%d)", limits.maxVisitedEntries)
			return filepath.SkipAll
		}
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

		relativeToSearch, err := filepath.Rel(searchPath, path)
		if err != nil {
			return nil
		}
		matched, err := workspaceGlobMatch(pattern, relativeToSearch)
		if err != nil || !matched {
			return nil
		}
		relativeToWorkspace, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		candidate := filepath.ToSlash(relativeToWorkspace)
		separator := 0
		if len(matches) > 0 {
			separator = 1
		}
		if len(matches) >= limits.maxResults {
			truncated = fmt.Sprintf("result limit (%d)", limits.maxResults)
			return filepath.SkipAll
		}
		if outputSize+separator+len(candidate) > limits.maxOutputBytes {
			truncated = fmt.Sprintf("output limit (%d bytes)", limits.maxOutputBytes)
			return filepath.SkipAll
		}
		matches = append(matches, candidate)
		outputSize += separator + len(candidate)
		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return "", fmt.Errorf("glob: %w", err)
	}

	if len(matches) == 0 && truncated == "" {
		return "No files found matching pattern: " + pattern, nil
	}
	output := strings.Join(matches, "\n")
	if truncated != "" {
		if output != "" {
			output += "\n"
		}
		output += fmt.Sprintf("... [glob partial: stopped at %s; narrow path or pattern and continue]", truncated)
	}
	return output, nil
}

func validateWorkspaceGlobPattern(pattern string) error {
	if filepath.IsAbs(pattern) {
		return fmt.Errorf("pattern %q must be relative", pattern)
	}
	for _, part := range strings.FieldsFunc(filepath.ToSlash(pattern), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return fmt.Errorf("pattern %q must not traverse outside its path", pattern)
		}
	}
	return nil
}

func workspaceGlobMatch(pattern, path string) (bool, error) {
	patternParts := strings.Split(filepath.ToSlash(filepath.Clean(pattern)), "/")
	pathParts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	type matchState struct{ pattern, path int }
	type matchResult struct {
		matched bool
		err     error
	}
	memo := make(map[matchState]matchResult)
	var matches func(int, int) (matched bool, err error)
	matches = func(patternIndex, pathIndex int) (matched bool, err error) {
		state := matchState{pattern: patternIndex, path: pathIndex}
		if result, ok := memo[state]; ok {
			return result.matched, result.err
		}
		defer func() { memo[state] = matchResult{matched: matched, err: err} }()
		if patternIndex == len(patternParts) {
			return pathIndex == len(pathParts), nil
		}
		if patternParts[patternIndex] == "**" {
			for nextPath := pathIndex; nextPath <= len(pathParts); nextPath++ {
				matched, err := matches(patternIndex+1, nextPath)
				if err != nil {
					return false, err
				}
				if matched {
					return true, nil
				}
			}
			return false, nil
		}
		if pathIndex == len(pathParts) {
			return false, nil
		}
		matched, err = filepath.Match(patternParts[patternIndex], pathParts[pathIndex])
		if err != nil {
			return false, fmt.Errorf("invalid pattern: %w", err)
		}
		if !matched {
			return false, nil
		}
		return matches(patternIndex+1, pathIndex+1)
	}
	return matches(0, 0)
}
