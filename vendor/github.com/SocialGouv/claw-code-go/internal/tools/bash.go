package tools

import (
	"bytes"
	"context"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/permissions"
	"io"
	"os"
	"os/exec"
	"time"
)

const (
	bashTimeout   = 30 * time.Second
	maxOutputSize = 10000
)

// bashWarnWriter is the writer for bash validation warnings.
// Defaults to os.Stderr; tests can replace it to capture output.
var bashWarnWriter io.Writer

func bashStderr() io.Writer {
	if bashWarnWriter != nil {
		return bashWarnWriter
	}
	return os.Stderr
}

// BashTool returns the tool definition for the bash tool.
func BashTool() api.Tool {
	return api.Tool{
		Name:        "bash",
		Description: "Execute a bash command and return the output. Use this for running shell commands, scripts, and system operations.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"command": {
					Type:        "string",
					Description: "The bash command to execute",
				},
			},
			Required: []string{"command"},
		},
	}
}

// ExecuteBash runs a bash command and returns combined stdout+stderr.
// It validates the command against the current permission mode and workspace
// path before execution. Pass permissions.ModeAllow and "" to skip validation.
func ExecuteBash(input map[string]any, mode permissions.PermissionMode, workspace string) (string, error) {
	command, ok := input["command"].(string)
	if !ok || command == "" {
		return "", fmt.Errorf("bash: 'command' input is required and must be a string")
	}

	// Validate command before execution.
	if workspace == "" {
		workspace = "."
	}
	result := ValidateCommand(command, mode, workspace)
	switch result.Kind {
	case ValidationBlock:
		return "", fmt.Errorf("bash: command blocked: %s", result.Reason)
	case ValidationWarn:
		// Log warning but proceed.
		fmt.Fprintf(bashStderr(), "bash warning: %s\n", result.Message)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bashTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", command)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()

	output := buf.String()

	// Truncate output if too long
	if len(output) > maxOutputSize {
		output = output[:maxOutputSize] + "\n... [output truncated]"
	}

	if err != nil {
		// Return output + error description; the caller decides if it's a hard error
		if ctx.Err() == context.DeadlineExceeded {
			return output, fmt.Errorf("command timed out after %s", bashTimeout)
		}
		// For non-zero exit codes, return output with error appended
		return output, fmt.Errorf("command exited with error: %v", err)
	}

	return output, nil
}
