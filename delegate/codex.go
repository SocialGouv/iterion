package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
)

// CodexBackend delegates work to the `codex` CLI (OpenAI Codex).
// It spawns a subprocess with the task prompt and collects structured output.
type CodexBackend struct {
	// Command overrides the CLI binary name (default: "codex").
	Command string
}

func (b *CodexBackend) command() string {
	if b.Command != "" {
		return b.Command
	}
	return "codex"
}

// Execute runs the codex CLI with the given task.
func (b *CodexBackend) Execute(ctx context.Context, task Task) (Result, error) {
	args := []string{
		"exec",
		"--dangerously-bypass-approvals-and-sandbox",
		"--json",
	}

	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
		args = append(args, "-C", task.WorkDir)
	}

	// Build the full prompt combining system and user messages.
	prompt := task.UserPrompt
	if task.SystemPrompt != "" {
		prompt = task.SystemPrompt + "\n\n" + prompt
	}

	if len(task.OutputSchema) > 0 {
		prompt += "\n\nYou MUST respond with a JSON object matching this schema:\n" + string(task.OutputSchema)
	}

	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, b.command(), args...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("delegate: codex stdout pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("delegate: codex failed to start: %w", err)
	}

	limited := io.LimitReader(stdoutPipe, maxOutputSize+1)
	output, err := io.ReadAll(limited)
	if err != nil {
		return Result{}, fmt.Errorf("delegate: codex reading stdout: %w", err)
	}

	if len(output) > maxOutputSize {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Result{}, fmt.Errorf("delegate: codex output exceeded limit of %d bytes", maxOutputSize)
	}

	if err := cmd.Wait(); err != nil {
		return Result{}, fmt.Errorf("delegate: codex failed: %w\nstderr: %s", err, stderr.String())
	}

	return parseCodexJSONL(output)
}

// parseCodexJSONL parses the JSONL output from `codex exec --json`.
// It extracts the last agent message text from "item.completed" events
// and token usage from "turn.completed" events.
func parseCodexJSONL(data []byte) (Result, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return Result{Output: map[string]interface{}{}}, nil
	}

	var lastText string
	var tokens int

	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var evt struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}

		switch evt.Type {
		case "item.completed":
			if evt.Item.Type == "agent_message" && evt.Item.Text != "" {
				lastText = evt.Item.Text
			}
		case "turn.completed":
			tokens = evt.Usage.InputTokens + evt.Usage.OutputTokens
		}
	}

	if lastText == "" {
		return Result{Output: map[string]interface{}{}}, nil
	}

	// Try parsing the agent's text as a JSON object.
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(lastText), &obj); err == nil {
		return Result{Output: obj, Tokens: tokens}, nil
	}

	// Try extracting JSON from markdown code blocks.
	if extracted := extractJSONFromMarkdown(lastText); extracted != "" {
		if err := json.Unmarshal([]byte(extracted), &obj); err == nil {
			return Result{Output: obj, Tokens: tokens}, nil
		}
	}

	// Fallback: wrap raw text.
	return Result{
		Output: map[string]interface{}{"text": lastText},
		Tokens: tokens,
	}, nil
}
