package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// maxOutputSize is the maximum allowed stdout size from a delegate subprocess (50 MB).
const maxOutputSize = 50 * 1024 * 1024

// ClaudeCodeBackend delegates work to the `claude` CLI (claude-code).
// It spawns a subprocess with the task prompt and collects structured output.
type ClaudeCodeBackend struct {
	// Command overrides the CLI binary name (default: "claude").
	Command string
}

func (b *ClaudeCodeBackend) command() string {
	if b.Command != "" {
		return b.Command
	}
	return "claude"
}

// Execute runs the claude CLI with the given task.
func (b *ClaudeCodeBackend) Execute(ctx context.Context, task Task) (Result, error) {
	args := []string{
		"--print",
		"--output-format", "json",
	}

	if task.SystemPrompt != "" {
		args = append(args, "--system-prompt", task.SystemPrompt)
	}

	if len(task.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(task.AllowedTools, ","))
	}

	// Build the user prompt; if an output schema is provided, append instructions.
	prompt := task.UserPrompt
	if len(task.OutputSchema) > 0 {
		prompt += "\n\nYou MUST respond with a JSON object matching this schema:\n" + string(task.OutputSchema)
	}

	args = append(args, "--prompt", prompt)

	cmd := exec.CommandContext(ctx, b.command(), args...)
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
		cmd.Dir = task.WorkDir
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("delegate: claude-code stdout pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("delegate: claude-code failed to start: %w", err)
	}

	limited := io.LimitReader(stdoutPipe, maxOutputSize+1)
	output, err := io.ReadAll(limited)
	if err != nil {
		return Result{}, fmt.Errorf("delegate: claude-code reading stdout: %w", err)
	}

	if len(output) > maxOutputSize {
		// Kill the process since we're not going to use the output.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Result{}, fmt.Errorf("delegate: claude-code output exceeded limit of %d bytes", maxOutputSize)
	}

	if err := cmd.Wait(); err != nil {
		return Result{}, fmt.Errorf("delegate: claude-code failed: %w\nstderr: %s", err, stderr.String())
	}

	return parseJSONOutput(output)
}

// validateWorkDir checks that workDir resolves to a path within baseDir.
// If baseDir is empty, no validation is performed.
func validateWorkDir(workDir, baseDir string) error {
	if baseDir == "" {
		return nil
	}

	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("delegate: invalid WorkDir %q: %w", workDir, err)
	}
	absWork = filepath.Clean(absWork)

	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return fmt.Errorf("delegate: invalid BaseDir %q: %w", baseDir, err)
	}
	absBase = filepath.Clean(absBase)

	// Ensure absWork is within absBase by checking the prefix with a trailing separator.
	if absWork != absBase && !strings.HasPrefix(absWork, absBase+string(filepath.Separator)) {
		return fmt.Errorf("delegate: WorkDir %q is outside allowed BaseDir %q", workDir, baseDir)
	}

	return nil
}

// parseJSONOutput tries to parse the CLI output as a JSON object.
// Falls back to wrapping raw text in {"text": "..."}.
func parseJSONOutput(data []byte) (Result, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return Result{Output: map[string]interface{}{}}, nil
	}

	// Try direct JSON object parse.
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err == nil {
		tokens := extractTokens(obj)
		return Result{Output: obj, Tokens: tokens}, nil
	}

	// Try JSON array — take the last element if it's the result.
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 {
		// Claude --output-format json may emit an array of messages.
		// The last message with role "assistant" contains the result text.
		return parseClaudeJSONArray(arr)
	}

	// Fallback: wrap raw text.
	return Result{
		Output: map[string]interface{}{"text": string(data)},
	}, nil
}

// parseClaudeJSONArray handles claude's JSON output format which is an array
// of message objects. Extracts the assistant's response content.
func parseClaudeJSONArray(arr []json.RawMessage) (Result, error) {
	// Walk backwards to find the last assistant text block.
	for i := len(arr) - 1; i >= 0; i-- {
		var msg struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(arr[i], &msg); err != nil {
			continue
		}
		if msg.Role != "assistant" || len(msg.Content) == 0 {
			continue
		}
		for _, c := range msg.Content {
			if c.Type == "text" && c.Text != "" {
				// Try parsing the text content as JSON.
				var obj map[string]interface{}
				if err := json.Unmarshal([]byte(c.Text), &obj); err == nil {
					return Result{Output: obj}, nil
				}
				return Result{Output: map[string]interface{}{"text": c.Text}}, nil
			}
		}
	}
	return Result{Output: map[string]interface{}{}}, nil
}

// extractTokens attempts to find token usage metadata in the response.
func extractTokens(obj map[string]interface{}) int {
	if usage, ok := obj["usage"].(map[string]interface{}); ok {
		input, _ := usage["input_tokens"].(float64)
		output, _ := usage["output_tokens"].(float64)
		return int(input + output)
	}
	return 0
}
