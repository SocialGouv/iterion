package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zendev-sh/goai"
)

var globSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"pattern": {
			"type": "string",
			"description": "Glob pattern to match files (e.g., '**/*.go', '*.txt')"
		},
		"path": {
			"type": "string",
			"description": "Base directory to search from (optional, defaults to current directory)"
		}
	},
	"required": ["pattern"]
}`)

// Glob returns a goai.Tool that finds files matching a glob pattern.
// Paths are resolved relative to workDir and must not escape it.
func Glob(workDir string) goai.Tool {
	return goai.Tool{
		Name:        "glob",
		Description: "Find files matching a glob pattern. Returns a newline-separated list of matching file paths. Note: only a single ** segment is supported (e.g. '**/*.go' works, but 'src/**/internal/**/*.go' does not).",
		InputSchema: globSchema,
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("glob: parse input: %w", err)
			}
			if params.Pattern == "" {
				return "", fmt.Errorf("glob: 'pattern' is required")
			}

			basePath := workDir
			if basePath == "" {
				basePath = "."
			}
			if params.Path != "" {
				resolved, err := safePath(workDir, params.Path)
				if err != nil {
					return "", fmt.Errorf("glob: %w", err)
				}
				basePath = resolved
			}

			var matches []string

			if strings.Contains(params.Pattern, "**") {
				parts := strings.SplitN(params.Pattern, "**", 2)
				prefix := filepath.Clean(filepath.Join(basePath, parts[0]))
				suffix := strings.TrimPrefix(parts[1], string(os.PathSeparator))

				err := filepath.Walk(prefix, func(path string, info os.FileInfo, err error) error {
					if err != nil || info.IsDir() {
						return nil
					}
					if suffix == "" {
						matches = append(matches, path)
						return nil
					}
					// Match suffix against the relative path from prefix,
					// so patterns like "**/internal/*.go" work correctly.
					rel, relErr := filepath.Rel(prefix, path)
					if relErr != nil {
						rel = filepath.Base(path)
					}
					matched, _ := filepath.Match(suffix, rel)
					if !matched {
						// Fall back to matching just the filename for simple suffixes like "*.go".
						matched, _ = filepath.Match(suffix, filepath.Base(path))
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
				fullPattern := filepath.Join(basePath, params.Pattern)
				found, err := filepath.Glob(fullPattern)
				if err != nil {
					return "", fmt.Errorf("glob: %w", err)
				}
				matches = found
			}

			if len(matches) == 0 {
				return "No files found matching pattern: " + params.Pattern, nil
			}

			return strings.Join(matches, "\n"), nil
		},
	}
}
