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

	// Structured output handling:
	// - When schema is set and NO tools: use native WithOutputFormat (single pass).
	// - When schema is set and tools are present: two-pass execution (see below).
	prompt := task.UserPrompt
	needsTwoPass := len(task.OutputSchema) > 0 && len(task.AllowedTools) > 0
	if len(task.OutputSchema) > 0 && !needsTwoPass {
		var schema map[string]any
		if json.Unmarshal(task.OutputSchema, &schema) == nil {
			opts = append(opts, claude.WithOutputFormat(schema))
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

	// Two-pass execution: when tools + schema are both present, Pass 1 output
	// is free-form text. We always run Pass 2 with WithOutputFormat to guarantee
	// structured output conforming to the schema.
	if needsTwoPass {
		pass1Text := ""
		if rm.Result != nil {
			pass1Text = *rm.Result
		}
		fmtRM, fmtErr := b.formatOutput(ctx, task, pass1Text)
		if fmtErr != nil {
			return result, fmt.Errorf("delegate: claude-code formatting pass failed: %w", fmtErr)
		}
		if fmtRM.Usage != nil {
			result.Tokens += fmtRM.Usage.InputTokens + fmtRM.Usage.OutputTokens
		}
		result.FormattingPassUsed = true

		output, rawLen, _ := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
		result.Output = output
		result.RawOutputLen = rawLen
		result.ParseFallback = false
		return result, nil
	}

	output, rawLen, fallback := parseSDKOutput(rm.Result, rm.StructuredOutput, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	return result, nil
}

// formatOutput performs the second pass of two-pass execution: a standalone
// call with WithOutputFormat (no tools) that guarantees structured JSON output
// conforming to the schema. The Pass 1 text output is included in the prompt
// so the model has the full context of the work that was done.
func (b *ClaudeCodeBackend) formatOutput(ctx context.Context, task Task, pass1Text string) (*claude.ResultMessage, error) {
	fmtCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var schema map[string]any
	if err := json.Unmarshal(task.OutputSchema, &schema); err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}

	opts := []claude.Option{
		claude.WithOutputFormat(schema),
		claude.WithPermissionMode("bypassPermissions"),
		claude.WithVerbose(true),
	}
	if task.WorkDir != "" {
		opts = append(opts, claude.WithCwd(task.WorkDir))
	}
	if b.Command != "" {
		opts = append(opts, claude.WithCLIPath(b.Command))
	}

	prompt := "You completed the following work:\n\n" + pass1Text +
		"\n\nNow format your complete findings as JSON matching the required output schema."

	return claude.Prompt(fmtCtx, prompt, opts...)
}
