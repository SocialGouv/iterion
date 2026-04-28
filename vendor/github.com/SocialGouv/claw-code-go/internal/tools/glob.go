package tools

import (
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"os"
	"path/filepath"
	"strings"
)

// GlobTool returns the tool definition for glob file search.
func GlobTool() api.Tool {
	return api.Tool{
		Name:        "glob",
		Description: "Find files matching a glob pattern. Returns a newline-separated list of matching file paths.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"pattern": {
					Type:        "string",
					Description: "Glob pattern to match files (e.g., '**/*.go', '*.txt')",
				},
				"path": {
					Type:        "string",
					Description: "Base directory to search from (optional, defaults to current directory)",
				},
			},
			Required: []string{"pattern"},
		},
	}
}

// ExecuteGlob finds files matching a glob pattern.
func ExecuteGlob(input map[string]any) (string, error) {
	pattern, ok := input["pattern"].(string)
	if !ok || pattern == "" {
		return "", fmt.Errorf("glob: 'pattern' input is required and must be a string")
	}

	basePath := "."
	if p, ok := input["path"].(string); ok && p != "" {
		basePath = p
	}

	var matches []string

	// Check if pattern contains "**" for recursive matching
	if strings.Contains(pattern, "**") {
		// Use filepath.Walk for recursive glob
		parts := strings.SplitN(pattern, "**", 2)
		prefix := filepath.Clean(filepath.Join(basePath, parts[0]))
		suffix := strings.TrimPrefix(parts[1], string(os.PathSeparator))

		err := filepath.Walk(prefix, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // Skip errors
			}
			if info.IsDir() {
				return nil
			}
			if suffix == "" {
				matches = append(matches, path)
				return nil
			}
			matched, err := filepath.Match(suffix, filepath.Base(path))
			if err != nil {
				return nil
			}
			if matched {
				matches = append(matches, path)
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("glob walk: %w", err)
		}
	} else {
		// Use standard filepath.Glob
		fullPattern := filepath.Join(basePath, pattern)
		found, err := filepath.Glob(fullPattern)
		if err != nil {
			return "", fmt.Errorf("glob: %w", err)
		}
		matches = found
	}

	if len(matches) == 0 {
		return "No files found matching pattern: " + pattern, nil
	}

	return strings.Join(matches, "\n"), nil
}
