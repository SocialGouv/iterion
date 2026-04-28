package tools

import (
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"os"
	"strings"
)

// FileEditTool returns the tool definition for targeted string-replace file edits.
func FileEditTool() api.Tool {
	return api.Tool{
		Name:        "file_edit",
		Description: "Edit a file by replacing an exact string with new content. Errors if old_string is not found or appears more than once.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"file_path": {
					Type:        "string",
					Description: "Path to the file to edit",
				},
				"old_string": {
					Type:        "string",
					Description: "Exact string to find and replace (must appear exactly once)",
				},
				"new_string": {
					Type:        "string",
					Description: "Replacement string",
				},
			},
			Required: []string{"file_path", "old_string", "new_string"},
		},
	}
}

// ExecuteFileEdit performs a targeted string replacement in a file.
func ExecuteFileEdit(input map[string]any) (string, error) {
	filePath, ok := input["file_path"].(string)
	if !ok || filePath == "" {
		return "", fmt.Errorf("file_edit: 'file_path' is required")
	}
	oldString, ok := input["old_string"].(string)
	if !ok {
		return "", fmt.Errorf("file_edit: 'old_string' is required")
	}
	newString, ok := input["new_string"].(string)
	if !ok {
		return "", fmt.Errorf("file_edit: 'new_string' is required")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("file_edit: %w", err)
	}

	content := string(data)
	count := strings.Count(content, oldString)
	if count == 0 {
		return "", fmt.Errorf("file_edit: old_string not found in %s", filePath)
	}
	if count > 1 {
		return "", fmt.Errorf("file_edit: old_string matches %d locations in %s (must be unique)", count, filePath)
	}

	updated := strings.Replace(content, oldString, newString, 1)
	if err := os.WriteFile(filePath, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("file_edit: write: %w", err)
	}

	return fmt.Sprintf("Successfully edited %s", filePath), nil
}
