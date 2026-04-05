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

var readFileSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the file to read"
		}
	},
	"required": ["path"]
}`)

var writeFileSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the file to write"
		},
		"content": {
			"type": "string",
			"description": "Content to write to the file"
		}
	},
	"required": ["path", "content"]
}`)

// safePath resolves p relative to workDir and ensures it stays within workDir.
// Returns the cleaned absolute path or an error if the path escapes.
func safePath(workDir, p string) (string, error) {
	if workDir == "" {
		workDir = "."
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve workdir: %w", err)
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Clean(filepath.Join(absWork, p))
	}
	// Ensure the resolved path is within workDir.
	if !strings.HasPrefix(abs, absWork+string(os.PathSeparator)) && abs != absWork {
		return "", fmt.Errorf("path %q escapes working directory %q", p, workDir)
	}
	return abs, nil
}

// ReadFile returns a goai.Tool that reads file contents.
// Paths are resolved relative to workDir and must not escape it.
func ReadFile(workDir string) goai.Tool {
	return goai.Tool{
		Name:        "read_file",
		Description: "Read the contents of a file from the filesystem.",
		InputSchema: readFileSchema,
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("read_file: parse input: %w", err)
			}
			if params.Path == "" {
				return "", fmt.Errorf("read_file: 'path' is required")
			}
			resolved, err := safePath(workDir, params.Path)
			if err != nil {
				return "", fmt.Errorf("read_file: %w", err)
			}
			data, err := os.ReadFile(resolved)
			if err != nil {
				return "", fmt.Errorf("read_file: %w", err)
			}
			return string(data), nil
		},
	}
}

// WriteFile returns a goai.Tool that writes content to a file.
// Paths are resolved relative to workDir and must not escape it.
func WriteFile(workDir string) goai.Tool {
	return goai.Tool{
		Name:        "write_file",
		Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does.",
		InputSchema: writeFileSchema,
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("write_file: parse input: %w", err)
			}
			if params.Path == "" {
				return "", fmt.Errorf("write_file: 'path' is required")
			}

			resolved, err := safePath(workDir, params.Path)
			if err != nil {
				return "", fmt.Errorf("write_file: %w", err)
			}

			dir := filepath.Dir(resolved)
			if dir != "" && dir != "." {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return "", fmt.Errorf("write_file: create directories: %w", err)
				}
			}

			// Preserve existing file permissions; default to 0644 for new files.
			perm := os.FileMode(0o644)
			if info, err := os.Stat(resolved); err == nil {
				perm = info.Mode().Perm()
			}

			if err := os.WriteFile(resolved, []byte(params.Content), perm); err != nil {
				return "", fmt.Errorf("write_file: %w", err)
			}

			return fmt.Sprintf("Successfully wrote %d bytes to %s", len(params.Content), resolved), nil
		},
	}
}
