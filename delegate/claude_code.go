package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
		"--dangerously-skip-permissions",
	}

	if task.SystemPrompt != "" {
		args = append(args, "--system-prompt", task.SystemPrompt)
	}

	if task.ReasoningEffort != "" {
		args = append(args, "--reasoning-effort", task.ReasoningEffort)
	}

	if len(task.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(task.AllowedTools, ","))
	}

	// Build the user prompt; if an output schema is provided, embed the schema
	// in the prompt text. Note: --json-schema disables tool use in claude CLI,
	// so we pass the schema as instructions instead.
	prompt := task.UserPrompt
	if len(task.OutputSchema) > 0 {
		prompt += "\n\nAfter completing all actions, you MUST respond with a JSON object matching this schema:\n" + string(task.OutputSchema)
	}

	// Pass prompt via stdin to avoid OS argument length limits (ARG_MAX).
	// The claude CLI reads from stdin when no positional argument is provided
	// and stdin is not a terminal.
	cmd := exec.CommandContext(ctx, b.command(), args...)
	cmd.Stdin = strings.NewReader(prompt)
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

	startTime := time.Now()

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
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return Result{
			Duration:    time.Since(startTime),
			ExitCode:    exitCode,
			Stderr:      stderr.String(),
			BackendName: "claude_code",
		}, fmt.Errorf("delegate: claude-code failed: %w\nstderr: %s", err, stderr.String())
	}

	result, parseErr := parseClaudeResult(output)
	if parseErr != nil {
		return Result{}, parseErr
	}
	result.Duration = time.Since(startTime)
	result.ExitCode = 0
	result.Stderr = stderr.String()
	result.BackendName = "claude_code"
	result.RawOutputLen = len(output)

	// Detect parse fallback: structured output was expected but parsing
	// fell back to wrapping plain text as {"text": "..."}.
	if len(task.OutputSchema) > 0 {
		if _, hasText := result.Output["text"]; hasText && len(result.Output) == 1 {
			result.ParseFallback = true
		}
	}

	return result, nil
}

// parseClaudeResult parses the JSON output from `claude --print --output-format json`.
// The output is a single JSON object with fields:
//
//	{type: "result", result: "<text or JSON string>", usage: {input_tokens, output_tokens, ...}}
//
// The actual content is in the "result" field as a string (which may itself be JSON).
func parseClaudeResult(data []byte) (Result, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return Result{Output: map[string]interface{}{}}, nil
	}

	// Parse the envelope.
	var envelope struct {
		Type   string                 `json:"type"`
		Result string                 `json:"result"`
		Usage  map[string]interface{} `json:"usage"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Type == "result" {
		tokens := 0
		if envelope.Usage != nil {
			input, _ := envelope.Usage["input_tokens"].(float64)
			output, _ := envelope.Usage["output_tokens"].(float64)
			tokens = int(input + output)
		}

		// Try parsing the result string as a JSON object.
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(envelope.Result), &obj); err == nil {
			return Result{Output: obj, Tokens: tokens}, nil
		}

		// Try extracting JSON from markdown code blocks (```json ... ```).
		if extracted := extractJSONFromMarkdown(envelope.Result); extracted != "" {
			if err := json.Unmarshal([]byte(extracted), &obj); err == nil {
				return Result{Output: obj, Tokens: tokens}, nil
			}
		}

		// Result is plain text.
		return Result{
			Output: map[string]interface{}{"text": envelope.Result},
			Tokens: tokens,
		}, nil
	}

	// Fallback to generic JSON parsing for backwards compatibility.
	return parseJSONOutput(data)
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

// extractJSONFromMarkdown extracts the last JSON object from markdown code blocks.
// It looks for ```json ... ``` or ``` ... ``` blocks and returns the last one
// that contains valid JSON.
func extractJSONFromMarkdown(text string) string {
	const fence = "```"
	result := ""
	for {
		start := strings.Index(text, fence)
		if start == -1 {
			break
		}
		// Skip the opening fence and optional language tag.
		inner := text[start+len(fence):]
		// Skip language tag (e.g., "json").
		if nl := strings.IndexByte(inner, '\n'); nl != -1 {
			inner = inner[nl+1:]
		}
		end := strings.Index(inner, fence)
		if end == -1 {
			break
		}
		block := strings.TrimSpace(inner[:end])
		// Check if it looks like a JSON object.
		if len(block) > 0 && block[0] == '{' {
			result = block
		}
		text = inner[end+len(fence):]
	}
	return result
}
