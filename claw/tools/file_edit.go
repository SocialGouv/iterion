package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/zendev-sh/goai"
)

var fileEditSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"file_path": {
			"type": "string",
			"description": "Path to the file to edit"
		},
		"old_string": {
			"type": "string",
			"description": "Exact string to find and replace (must appear exactly once)"
		},
		"new_string": {
			"type": "string",
			"description": "Replacement string"
		}
	},
	"required": ["file_path", "old_string", "new_string"]
}`)

// FileEdit returns a goai.Tool that performs targeted string replacements.
// Paths are resolved relative to workDir and must not escape it.
func FileEdit(workDir string) goai.Tool {
	return goai.Tool{
		Name:        "file_edit",
		Description: "Edit a file by replacing an exact string with new content. Errors if old_string is not found or appears more than once.",
		InputSchema: fileEditSchema,
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var params struct {
				FilePath  string `json:"file_path"`
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("file_edit: parse input: %w", err)
			}
			if params.FilePath == "" {
				return "", fmt.Errorf("file_edit: 'file_path' is required")
			}

			resolved, err := safePath(workDir, params.FilePath)
			if err != nil {
				return "", fmt.Errorf("file_edit: %w", err)
			}

			data, err := os.ReadFile(resolved)
			if err != nil {
				return "", fmt.Errorf("file_edit: %w", err)
			}

			content := string(data)
			count := strings.Count(content, params.OldString)
			if count == 0 {
				return "", fmt.Errorf("file_edit: old_string not found in %s", resolved)
			}
			if count > 1 {
				return "", fmt.Errorf("file_edit: old_string matches %d locations in %s (must be unique)", count, resolved)
			}

			// Preserve original file permissions.
			perm := os.FileMode(0o644)
			if info, err := os.Stat(resolved); err == nil {
				perm = info.Mode().Perm()
			}

			updated := strings.Replace(content, params.OldString, params.NewString, 1)
			if err := os.WriteFile(resolved, []byte(updated), perm); err != nil {
				return "", fmt.Errorf("file_edit: write: %w", err)
			}

			return fmt.Sprintf("Successfully edited %s", resolved), nil
		},
	}
}
