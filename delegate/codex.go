package delegate

import (
	"bytes"
	"context"
	"fmt"
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
		"--quiet",
		"--approval-mode", "full-auto",
		"--output-format", "json",
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
	if task.WorkDir != "" {
		cmd.Dir = task.WorkDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("delegate: codex failed: %w\nstderr: %s", err, stderr.String())
	}

	return parseJSONOutput(stdout.Bytes())
}
