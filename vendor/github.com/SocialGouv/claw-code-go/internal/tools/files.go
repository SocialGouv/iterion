package tools

import (
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"os"
	"path/filepath"
)

// ReadFileTool returns the tool definition for reading files.
func ReadFileTool() api.Tool {
	return api.Tool{
		Name:        "read_file",
		Description: "Read the contents of a file from the filesystem.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"path": {
					Type:        "string",
					Description: "Path to the file to read",
				},
			},
			Required: []string{"path"},
		},
	}
}

// WriteFileTool returns the tool definition for writing files.
func WriteFileTool() api.Tool {
	return api.Tool{
		Name:        "write_file",
		Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"path": {
					Type:        "string",
					Description: "Path to the file to write",
				},
				"content": {
					Type:        "string",
					Description: "Content to write to the file",
				},
			},
			Required: []string{"path", "content"},
		},
	}
}

// ExecuteReadFile reads a file and returns its contents.
func ExecuteReadFile(input map[string]any) (string, error) {
	path, ok := input["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("read_file: 'path' input is required and must be a string")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}

	return string(data), nil
}

// ExecuteWriteFile writes content to a file, creating parent directories as needed.
func ExecuteWriteFile(input map[string]any) (string, error) {
	path, ok := input["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("write_file: 'path' input is required and must be a string")
	}

	content, ok := input["content"].(string)
	if !ok {
		return "", fmt.Errorf("write_file: 'content' input is required and must be a string")
	}

	// Create parent directories if needed
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("write_file: create directories: %w", err)
		}
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}

	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(content), path), nil
}
