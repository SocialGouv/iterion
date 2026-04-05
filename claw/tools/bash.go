// Package tools provides coding-oriented tool implementations as goai.Tool.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/zendev-sh/goai"
)

const (
	bashTimeout   = 120 * time.Second
	maxOutputSize = 10000
)

var bashSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"command": {
			"type": "string",
			"description": "The bash command to execute"
		},
		"timeout": {
			"type": "integer",
			"description": "Timeout in seconds (default 120)"
		}
	},
	"required": ["command"]
}`)

// Bash returns a goai.Tool that executes bash commands.
// Security note: commands are passed directly to "bash -c" with no sanitization.
// Callers must enforce safety through the agent's OnPermissionCheck hook or by
// restricting which tools are available via CodingAgentConfig.AllowedTools.
func Bash(workDir string) goai.Tool {
	return goai.Tool{
		Name:        "bash",
		Description: "Execute a bash command and return the output. Use this for running shell commands, scripts, and system operations.",
		InputSchema: bashSchema,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("bash: parse input: %w", err)
			}
			if params.Command == "" {
				return "", fmt.Errorf("bash: 'command' is required")
			}

			timeout := bashTimeout
			if params.Timeout > 0 {
				timeout = time.Duration(params.Timeout) * time.Second
			}

			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			cmd := exec.CommandContext(ctx, "bash", "-c", params.Command)
			if workDir != "" {
				cmd.Dir = workDir
			}

			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf

			err := cmd.Run()
			output := buf.String()

			if len(output) > maxOutputSize {
				output = output[:maxOutputSize] + "\n... [output truncated]"
			}

			if err != nil {
				if ctx.Err() == context.DeadlineExceeded {
					return output, fmt.Errorf("command timed out after %s", timeout)
				}
				// Non-zero exit is normal for many commands (grep, diff, test).
				// Return output with exit info as a string, not a Go error.
				if exitErr, ok := err.(*exec.ExitError); ok {
					return fmt.Sprintf("%s\n[exit code: %d]", output, exitErr.ExitCode()), nil
				}
				return output, fmt.Errorf("bash: %w", err)
			}

			return output, nil
		},
	}
}
