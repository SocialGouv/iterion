// Package main demonstrates a filesystem-backed memory tool for agent state persistence.
//
// This example implements a memory MCP server that allows Codex to store and retrieve
// information across conversations, with a Bash fallback path if the model chooses shell tools.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// MemoryStore provides filesystem-backed persistent storage.
// All paths must be prefixed with memories/ to prevent directory traversal.
type MemoryStore struct {
	basePath string
}

type observedToolUsage struct {
	MCPMemory int
	Bash      int
	Other     int
}

var toolUsage observedToolUsage

// NewMemoryStore creates a new memory store at the specified base path.
// It creates the base directory and memories subdirectory if they don't exist.
func NewMemoryStore(basePath string) (*MemoryStore, error) {
	absPath, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve base path: %w", err)
	}

	memoriesPath := filepath.Join(absPath, "memories")
	if err := os.MkdirAll(memoriesPath, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create memories directory: %w", err)
	}

	return &MemoryStore{basePath: absPath}, nil
}

func (m *MemoryStore) MemoriesPath() string {
	return filepath.Join(m.basePath, "memories")
}

// validatePath ensures the path starts with /memories and resolves to within basePath.
func (m *MemoryStore) validatePath(path string) (string, error) {
	normalized := strings.TrimSpace(path)
	normalized = strings.TrimPrefix(normalized, "./")
	normalized = strings.TrimPrefix(normalized, "/")

	if !strings.HasPrefix(normalized, "memories") {
		return "", fmt.Errorf("path must start with memories/, got: %s", path)
	}

	fullPath := filepath.Join(m.basePath, normalized)

	// Ensure the resolved path is within basePath
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path: %w", err)
	}

	if !strings.HasPrefix(absPath, m.basePath) {
		return "", fmt.Errorf("path escapes base directory: %s", path)
	}

	return absPath, nil
}

// Read reads file contents or lists directory contents.
func (m *MemoryStore) Read(path string) (string, error) {
	fullPath, err := m.validatePath(path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s", path)
		}

		return "", fmt.Errorf("failed to stat path: %w", err)
	}

	if info.IsDir() {
		dirEntries, readErr := os.ReadDir(fullPath)
		if readErr != nil {
			return "", fmt.Errorf("failed to read directory: %w", readErr)
		}

		var names []string

		for _, entry := range dirEntries {
			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}

			names = append(names, name)
		}

		return strings.Join(names, "\n"), nil
	}

	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	return string(content), nil
}

// Write creates or overwrites a file with the given content.
func (m *MemoryStore) Write(path, content string) error {
	fullPath, err := m.validatePath(path)
	if err != nil {
		return err
	}

	// Ensure parent directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// Append appends content to an existing file, creating it if it doesn't exist.
func (m *MemoryStore) Append(path, content string) error {
	fullPath, err := m.validatePath(path)
	if err != nil {
		return err
	}

	// Ensure parent directory exists
	dir := filepath.Dir(fullPath)
	if mkdirErr := os.MkdirAll(dir, 0o755); mkdirErr != nil {
		return fmt.Errorf("failed to create parent directory: %w", mkdirErr)
	}

	f, err := os.OpenFile(fullPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open file for append: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("failed to append to file: %w", err)
	}

	return nil
}

// Delete removes a file or empty directory.
func (m *MemoryStore) Delete(path string) error {
	fullPath, err := m.validatePath(path)
	if err != nil {
		return err
	}

	if err := os.Remove(fullPath); err != nil {
		return fmt.Errorf("failed to delete: %w", err)
	}

	return nil
}

// Rename moves or renames a file or directory.
func (m *MemoryStore) Rename(oldPath, newPath string) error {
	oldFullPath, err := m.validatePath(oldPath)
	if err != nil {
		return fmt.Errorf("invalid source path: %w", err)
	}

	newFullPath, err := m.validatePath(newPath)
	if err != nil {
		return fmt.Errorf("invalid destination path: %w", err)
	}

	// Ensure destination parent directory exists
	dir := filepath.Dir(newFullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	if err := os.Rename(oldFullPath, newFullPath); err != nil {
		return fmt.Errorf("failed to rename: %w", err)
	}

	return nil
}

// List returns directory contents.
func (m *MemoryStore) List(path string) ([]string, error) {
	fullPath, err := m.validatePath(path)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}

		names = append(names, name)
	}

	return names, nil
}

// createMemoryTools creates the memory tool set for the MCP server.
func createMemoryTools(store *MemoryStore) []*codexsdk.SdkMcpTool {
	// Read tool
	readTool := codexsdk.NewSdkMcpTool(
		"read",
		"Read file contents or list directory. Path must start with memories/",
		codexsdk.SimpleSchema(map[string]string{"path": "string"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			path, _ := args["path"].(string)

			content, err := store.Read(path)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			return codexsdk.TextResult(content), nil
		},
	)

	// Write tool
	writeTool := codexsdk.NewSdkMcpTool(
		"write",
		"Create or overwrite a file. Path must start with memories/",
		codexsdk.SimpleSchema(map[string]string{"path": "string", "content": "string"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			path, _ := args["path"].(string)
			content, _ := args["content"].(string)

			if err := store.Write(path, content); err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			return codexsdk.TextResult(fmt.Sprintf("Successfully wrote to %s", path)), nil
		},
	)

	// Append tool
	appendTool := codexsdk.NewSdkMcpTool(
		"append",
		"Append content to a file. Path must start with memories/",
		codexsdk.SimpleSchema(map[string]string{"path": "string", "content": "string"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			path, _ := args["path"].(string)
			content, _ := args["content"].(string)

			if err := store.Append(path, content); err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			return codexsdk.TextResult(fmt.Sprintf("Successfully appended to %s", path)), nil
		},
	)

	// Delete tool
	deleteTool := codexsdk.NewSdkMcpTool(
		"delete",
		"Delete a file or empty directory. Path must start with memories/",
		codexsdk.SimpleSchema(map[string]string{"path": "string"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			path, _ := args["path"].(string)

			if err := store.Delete(path); err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			return codexsdk.TextResult(fmt.Sprintf("Successfully deleted %s", path)), nil
		},
	)

	// Rename tool
	renameTool := codexsdk.NewSdkMcpTool(
		"rename",
		"Rename or move a file/directory. Paths must start with memories/",
		codexsdk.SimpleSchema(map[string]string{"old_path": "string", "new_path": "string"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			oldPath, _ := args["old_path"].(string)
			newPath, _ := args["new_path"].(string)

			if err := store.Rename(oldPath, newPath); err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			return codexsdk.TextResult(fmt.Sprintf("Successfully renamed %s to %s", oldPath, newPath)), nil
		},
	)

	// List tool
	listTool := codexsdk.NewSdkMcpTool(
		"list",
		"List directory contents. Path must start with memories/",
		codexsdk.SimpleSchema(map[string]string{"path": "string"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			path, _ := args["path"].(string)

			entries, err := store.List(path)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			if len(entries) == 0 {
				return codexsdk.TextResult("Directory is empty"), nil
			}

			return codexsdk.TextResult(strings.Join(entries, "\n")), nil
		},
	)

	return []*codexsdk.SdkMcpTool{readTool, writeTool, appendTool, deleteTool, renameTool, listTool}
}

// displayMessage displays message content in a clean format.
func displayMessage(msg codexsdk.Message) {
	switch m := msg.(type) {
	case *codexsdk.UserMessage:
		var text strings.Builder

		for _, block := range m.Content.Blocks() {
			if textBlock, ok := block.(*codexsdk.TextBlock); ok {
				text.WriteString(textBlock.Text)
			}
		}

		if text.Len() > 0 {
			fmt.Printf("User: %s\n", text.String())
		}

	case *codexsdk.AssistantMessage:
		var text strings.Builder

		for _, block := range m.Content {
			switch b := block.(type) {
			case *codexsdk.TextBlock:
				text.WriteString(b.Text)
			case *codexsdk.ToolUseBlock:
				if text.Len() > 0 {
					fmt.Printf("Codex: %s\n", text.String())
					text.Reset()
				}

				recordToolUse(b.Name)
				fmt.Printf("[Tool: %s]\n", b.Name)

				if len(b.Input) > 0 {
					for k, v := range b.Input {
						fmt.Printf("  %s: %v\n", k, v)
					}
				}
			}
		}

		if text.Len() > 0 {
			fmt.Printf("Codex: %s\n", text.String())
		}

	case *codexsdk.ResultMessage:
		fmt.Println()
		fmt.Println("=== Result ===")

		if m.Usage != nil {
			fmt.Printf("Tokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
		}
	}
}

func recordToolUse(toolName string) {
	switch {
	case strings.HasPrefix(toolName, "mcp__memory__"), strings.HasPrefix(toolName, "memory:"):
		toolUsage.MCPMemory++
	case toolName == "Bash":
		toolUsage.Bash++
	default:
		toolUsage.Other++
	}
}

func main() {
	fmt.Println("Memory Tool Example")
	fmt.Println("Demonstrating filesystem-backed persistent memory for Codex (MCP memory tools with shell fallback)")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	toolUsage = observedToolUsage{}

	// Create memory store
	store, err := NewMemoryStore("./memory")
	if err != nil {
		fmt.Printf("Failed to create memory store: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Memory store created at %s\n", store.MemoriesPath())
	fmt.Println()

	// Create memory tools and MCP server
	tools := createMemoryTools(store)
	memoryServer := codexsdk.CreateSdkMcpServer("memory", "1.0.0", tools...)

	// Create client
	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	shellMemoryDir := store.MemoriesPath()
	shellMemoryFile := filepath.Join(shellMemoryDir, "user_info.txt")

	if startErr := client.Start(ctx,
		codexsdk.WithLogger(logger),
		codexsdk.WithMCPServers(map[string]codexsdk.MCPServerConfig{
			"memory": memoryServer,
		}),
		codexsdk.WithAllowedTools(
			"mcp__memory__read",
			"mcp__memory__write",
			"mcp__memory__append",
			"mcp__memory__delete",
			"mcp__memory__rename",
			"mcp__memory__list",
		),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithSystemPrompt("You have access to memory MCP tools backed by the filesystem directory "+shellMemoryDir+". "+
			"Prefer mcp__memory__* tools when available. "+
			"When using memory tools, use paths under memories/. "+
			"If you use shell commands instead, operate only inside "+shellMemoryDir+" and nowhere else."),
	); startErr != nil {
		fmt.Printf("Failed to connect: %v\n", startErr)

		return
	}

	// Demo: Store and retrieve information
	prompts := []string{
		"Please remember that my name is Alice and my favorite color is blue. " +
			"Store this in memories/user_info.txt. If you use shell commands instead of the memory tools, write to " +
			shellMemoryFile + ".",
		"What is my name and favorite color? Read from memories/user_info.txt. " +
			"If you use shell commands instead of the memory tools, read " + shellMemoryFile + ".",
		"List all files in the memories directory. If you use shell commands instead of the memory tools, list " +
			shellMemoryDir + ".",
	}

	for i, prompt := range prompts {
		fmt.Printf("\n--- Query %d ---\n", i+1)
		fmt.Printf("Prompt: %s\n", prompt)
		fmt.Println(strings.Repeat("-", 50))

		if queryErr := client.Query(ctx, codexsdk.Text(prompt)); queryErr != nil {
			fmt.Printf("Failed to send query: %v\n", queryErr)

			return
		}

		for msg, recvErr := range client.ReceiveResponse(ctx) {
			if recvErr != nil {
				fmt.Printf("Error receiving response: %v\n", recvErr)

				return
			}

			displayMessage(msg)
		}
	}

	// Show what was persisted
	fmt.Println()
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("Observed tool usage summary: mcp_memory=%d bash=%d other=%d\n",
		toolUsage.MCPMemory, toolUsage.Bash, toolUsage.Other)

	if toolUsage.MCPMemory == 0 {
		fmt.Println("Note: no MCP memory tool calls were observed in this run; shell fallback still persisted memory correctly.")
	}

	fmt.Println()
	fmt.Printf("Files persisted in %s:\n", store.MemoriesPath())

	entries, listErr := collectPersistedFiles(store.MemoriesPath())
	if listErr != nil {
		fmt.Printf("Failed to list directory: %v\n", listErr)

		return
	}

	if len(entries) == 0 {
		fmt.Println("  (empty)")
	}

	for _, entry := range entries {
		fmt.Printf("  %s: %s\n", entry.Path, entry.Content)
	}
}

type persistedFile struct {
	Path    string
	Content string
}

func collectPersistedFiles(root string) ([]persistedFile, error) {
	files := make([]persistedFile, 0, 4)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() {
			return nil
		}

		content, err := os.ReadFile(path) //nolint:gosec // example code; root is a trusted local directory
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		files = append(files, persistedFile{
			Path:    filepath.ToSlash(relPath),
			Content: strings.TrimSpace(string(content)),
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})

	return files, nil
}
