package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

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
		cmd.Dir = task.WorkDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("delegate: claude-code failed: %w\nstderr: %s", err, stderr.String())
	}

	return parseJSONOutput(stdout.Bytes())
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
