package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// ToolUsageLog tracks tool usage for demonstration.
type ToolUsageLog struct {
	Tool        string
	Input       map[string]any
	Suggestions []*codexsdk.PermissionUpdate
}

var toolUsageLog []ToolUsageLog

// myPermissionCallback controls tool permissions based on tool type and input.
func myPermissionCallback(
	ctx context.Context,
	toolName string,
	inputData map[string]any,
	permCtx *codexsdk.ToolPermissionContext,
) (codexsdk.PermissionResult, error) {
	// Log the tool request
	toolUsageLog = append(toolUsageLog, ToolUsageLog{
		Tool:        toolName,
		Input:       inputData,
		Suggestions: permCtx.Suggestions,
	})

	inputJSON, _ := json.MarshalIndent(inputData, "   ", "  ")

	fmt.Printf("\n🔧 Tool Permission Request: %s\n", toolName)
	fmt.Printf("   Input: %s\n", string(inputJSON))

	// Always allow read operations
	if toolName == "Read" || toolName == "Glob" || toolName == "Grep" {
		fmt.Printf("   ✅ Automatically allowing %s (read-only operation)\n", toolName)

		return &codexsdk.PermissionResultAllow{
			Behavior: "allow",
		}, nil
	}

	// Deny write operations to system directories
	if toolName == "Write" || toolName == "Edit" || toolName == "MultiEdit" {
		filePath, ok := inputData["file_path"].(string)
		if !ok {
			filePath = ""
		}

		if strings.HasPrefix(filePath, "/etc/") || strings.HasPrefix(filePath, "/usr/") {
			fmt.Printf("   ❌ Denying write to system directory: %s\n", filePath)

			return &codexsdk.PermissionResultDeny{
				Behavior: "deny",
				Message:  fmt.Sprintf("Cannot write to system directory: %s", filePath),
			}, nil
		}

		// Redirect writes to a safe directory
		if !strings.HasPrefix(filePath, "/tmp/") && !strings.HasPrefix(filePath, "./") {
			pathParts := strings.Split(filePath, "/")
			fileName := pathParts[len(pathParts)-1]
			safePath := fmt.Sprintf("./safe_output/%s", fileName)

			fmt.Printf("   ⚠️  Redirecting write from %s to %s\n", filePath, safePath)

			modifiedInput := make(map[string]any)
			maps.Copy(modifiedInput, inputData)

			modifiedInput["file_path"] = safePath

			return &codexsdk.PermissionResultAllow{
				Behavior:     "allow",
				UpdatedInput: modifiedInput,
			}, nil
		}
	}

	// Check dangerous bash commands
	if toolName == "Bash" {
		command, ok := inputData["command"].(string)
		if !ok {
			command = ""
		}

		dangerousCommands := []string{"rm -rf", "sudo", "chmod 777", "dd if=", "mkfs"}

		for _, dangerous := range dangerousCommands {
			if strings.Contains(command, dangerous) {
				fmt.Printf("   ❌ Denying dangerous command: %s\n", command)

				return &codexsdk.PermissionResultDeny{
					Behavior: "deny",
					Message:  fmt.Sprintf("Dangerous command pattern detected: %s", dangerous),
				}, nil
			}
		}

		// Allow but log the command
		fmt.Printf("   ✅ Allowing bash command: %s\n", command)

		return &codexsdk.PermissionResultAllow{
			Behavior: "allow",
		}, nil
	}

	// For all other tools, prompt the user (in Golang this would be interactive stdin).
	// Note: Go cannot do synchronous stdin prompting in this callback context,
	// so we deny by default. In an interactive scenario, you would prompt the user
	// via a separate channel or UI mechanism.
	fmt.Printf("   ❓ Unknown tool: %s (would prompt user in interactive mode)\n", toolName)

	return &codexsdk.PermissionResultDeny{
		Behavior: "deny",
		Message:  "Tool requires user approval - not available in non-interactive mode",
	}, nil
}

func simulatePermissionRequest(
	ctx context.Context,
	toolName string,
	input map[string]any,
) {
	result, err := myPermissionCallback(ctx, toolName, input, &codexsdk.ToolPermissionContext{})
	if err != nil {
		fmt.Printf("   Callback error: %v\n", err)

		return
	}

	switch r := result.(type) {
	case *codexsdk.PermissionResultAllow:
		fmt.Printf("   Result: allow")

		if len(r.UpdatedInput) > 0 {
			updatedJSON, _ := json.MarshalIndent(r.UpdatedInput, "   ", "  ")
			fmt.Printf(" with updated input %s", string(updatedJSON))
		}

		fmt.Println()
	case *codexsdk.PermissionResultDeny:
		fmt.Printf("   Result: deny")

		if r.Message != "" {
			fmt.Printf(" (%s)", r.Message)
		}

		fmt.Println()
	default:
		fmt.Printf("   Result: unexpected type %T\n", result)
	}
}

func main() {
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("Tool Permission Callback Example")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("\nThis example demonstrates how to:")
	fmt.Println("1. Implement a custom permission callback")
	fmt.Println("2. Allow/deny approval-gated tools based on type")
	fmt.Println("3. Modify tool inputs for safety")
	fmt.Println("4. Log tool usage for later inspection")
	fmt.Println("5. Drive the callback directly so the demo is deterministic")
	fmt.Println(strings.Repeat("=", 60))

	ctx := context.Background()

	fmt.Println("\n🧪 Simulating permission requests...")
	simulatePermissionRequest(ctx, "Read", map[string]any{"file_path": "./README.md"})
	simulatePermissionRequest(ctx, "Write", map[string]any{"file_path": "/etc/hosts"})
	simulatePermissionRequest(ctx, "Write", map[string]any{"file_path": "notes.txt"})
	simulatePermissionRequest(ctx, "Bash", map[string]any{"command": "ls -1"})
	simulatePermissionRequest(ctx, "Bash", map[string]any{"command": "sudo rm -rf /"})
	simulatePermissionRequest(ctx, "Browser", map[string]any{"url": "https://example.com"})

	// Print tool usage summary
	fmt.Println("\n" + strings.Repeat("=", 60))

	fmt.Println("Tool Usage Summary")
	fmt.Println(strings.Repeat("=", 60))

	for i, usage := range toolUsageLog {
		fmt.Printf("\n%d. Tool: %s\n", i+1, usage.Tool)

		inputJSON, _ := json.MarshalIndent(usage.Input, "   ", "  ")
		fmt.Printf("   Input: %s\n", string(inputJSON))

		if len(usage.Suggestions) > 0 {
			fmt.Printf("   Suggestions: %d permission updates suggested\n", len(usage.Suggestions))
		}
	}
}
