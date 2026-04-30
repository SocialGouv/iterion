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
//
// The spawned bash inherits os.Environ() of the calling process. Use
// ExecuteBashWithEnv when the caller needs to surface a project-managed
// toolchain (devbox, nix, asdf) whose bin path is not in the parent
// shell's PATH.
func ExecuteBash(input map[string]any, mode permissions.PermissionMode, workspace string) (string, error) {
	return ExecuteBashWithEnv(input, mode, workspace, nil)
}

// ExecuteBashWithEnv runs a bash command with extra environment
// variables appended to the inherited process environment. Each
// extraEnv entry uses the standard "KEY=value" format; later entries
// for the same key win (Go's exec.Cmd convention).
//
// The use case driving this entry point: when iterion is launched
// without the project's devbox/nix/asdf toolchain in PATH, the bash
// tool can't find go/gofmt/etc. and the LLM-driven fixer can't run
// `go test` to validate its patch. Passing devbox's bin path via
// extraEnv from the iterion side restores autonomy without forcing
// every operator to remember `devbox run --` at launch.
//
// Pass nil to inherit only the parent process's environment (same
// behaviour as the legacy ExecuteBash entry point).
func ExecuteBashWithEnv(input map[string]any, mode permissions.PermissionMode, workspace string, extraEnv []string) (string, error) {
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
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}

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
