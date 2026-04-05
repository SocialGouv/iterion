package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	claude "github.com/partio-io/claude-agent-sdk-go"
)

// ClaudeCodeBackend delegates work to the `claude` CLI (claude-code)
// via the Claude Agent SDK.
type ClaudeCodeBackend struct {
	// Command overrides the CLI binary path (default: "claude").
	Command string
}

// Execute runs the claude CLI with the given task using the Claude Agent SDK.
func (b *ClaudeCodeBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}

	var opts []claude.Option

	// Build system prompt, optionally augmented with interaction instructions.
	systemPrompt := task.SystemPrompt
	if task.InteractionEnabled {
		systemPrompt += interactionSystemInstruction
	}
	if systemPrompt != "" {
		opts = append(opts, claude.WithSystemPrompt(systemPrompt))
	}
	if task.WorkDir != "" {
		opts = append(opts, claude.WithCwd(task.WorkDir))
	}
	if len(task.AllowedTools) > 0 {
		opts = append(opts, claude.WithAllowedTools(task.AllowedTools...))
	}
	// Bypass interactive permission prompts: the runtime enforces safety via
	// workspace isolation and allowed-tool lists, so the delegate subprocess
	// does not need its own permission gate.
	opts = append(opts, claude.WithPermissionMode("bypassPermissions"))

	// The CLI requires --verbose when using --output-format=stream-json in
	// --print mode. The SDK always uses stream-json, so we must enable verbose.
	opts = append(opts, claude.WithVerbose(true))

	if b.Command != "" {
		opts = append(opts, claude.WithCLIPath(b.Command))
	}

	if task.ReasoningEffort != "" {
		opts = append(opts, claude.WithEnv("CLAUDE_CODE_EFFORT_LEVEL", task.ReasoningEffort))
	}

	if task.SessionID != "" {
		opts = append(opts, claude.WithResume(task.SessionID))
		if task.ForkSession {
			opts = append(opts, claude.WithForkSession(true))
		}
	}

	// Build user prompt. When an output schema is provided and tools are also
	// in use, embed the schema in the prompt text (WithOutputFormat disables
	// tool use in the claude CLI). When no tools are involved, use the native
	// structured output flag.
	prompt := task.UserPrompt
	if len(task.OutputSchema) > 0 {
		if len(task.AllowedTools) == 0 {
			var schema map[string]any
			if json.Unmarshal(task.OutputSchema, &schema) == nil {
				opts = append(opts, claude.WithOutputFormat(schema))
			}
		} else {
			prompt += "\n\nAfter completing all actions, you MUST respond with a JSON object matching this schema:\n" + string(task.OutputSchema)
		}
	}

	// Capture stderr for observability.
	var stderrBuf strings.Builder
	opts = append(opts, claude.WithStderrCallback(func(line string) {
		stderrBuf.WriteString(line)
		stderrBuf.WriteString("\n")
	}))

	startTime := time.Now()
	rm, err := claude.Prompt(ctx, prompt, opts...)
	duration := time.Since(startTime)

	if err != nil {
		return Result{
			Duration:    duration,
			ExitCode:    -1,
			Stderr:      stderrBuf.String(),
			BackendName: "claude_code",
		}, fmt.Errorf("delegate: claude-code failed: %w", err)
	}

	result := Result{
		Duration:    duration,
		ExitCode:    0,
		Stderr:      stderrBuf.String(),
		BackendName: "claude_code",
		SessionID:   rm.SessionID,
	}

	if rm.Usage != nil {
		result.Tokens = rm.Usage.InputTokens + rm.Usage.OutputTokens
	}

	if rm.IsError && rm.Subtype != claude.ResultSuccess {
		return result, fmt.Errorf("delegate: claude-code error: subtype=%s", rm.Subtype)
	}

	output, rawLen, fallback := parseSDKOutput(rm.Result, rm.StructuredOutput, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	return result, nil
}
