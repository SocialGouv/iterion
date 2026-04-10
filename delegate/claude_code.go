package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	rm, err := promptWithTimeout(ctx, prompt, opts...)
	duration := time.Since(startTime)

	if err != nil {
		return Result{
			Duration:    duration,
			ExitCode:    -1,
			Stderr:      stderrBuf.String(),
			BackendName: BackendClaudeCode,
		}, fmt.Errorf("delegate: claude-code failed: %w", err)
	}

	result := Result{
		Duration:    duration,
		ExitCode:    0,
		Stderr:      stderrBuf.String(),
		BackendName: BackendClaudeCode,
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
	// structured output conforming to the schema via session resume.
	if needsTwoPass && rm.SessionID != "" {
		const maxFmtAttempts = 2
		for attempt := 1; attempt <= maxFmtAttempts; attempt++ {
			log.Printf("delegate: claude-code [formatting pass %d/%d] starting structured output extraction (session=%s)", attempt, maxFmtAttempts, rm.SessionID)
			fmtRM, fmtErr := b.formatOutput(ctx, task, rm.SessionID)
			if fmtErr != nil {
				if attempt < maxFmtAttempts {
					log.Printf("delegate: claude-code [formatting pass %d/%d] failed, retrying: %v", attempt, maxFmtAttempts, fmtErr)
					continue
				}
				return result, fmt.Errorf("delegate: claude-code formatting pass failed: %w", fmtErr)
			}
			if fmtRM.Usage != nil {
				result.Tokens += fmtRM.Usage.InputTokens + fmtRM.Usage.OutputTokens
			}
			result.FormattingPassUsed = true

			output, rawLen, fallback := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
			if fallback && attempt < maxFmtAttempts {
				log.Printf("delegate: claude-code [formatting pass %d/%d] produced fallback text, retrying", attempt, maxFmtAttempts)
				continue
			}
			result.Output = output
			result.RawOutputLen = rawLen
			result.ParseFallback = fallback
			return result, nil
		}
	}

	output, rawLen, fallback := parseSDKOutput(rm.Result, rm.StructuredOutput, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	// Safety net: if we have a schema but got empty/nil output or only a
	// fallback text wrapper, attempt a formatting pass via session resume.
	// This catches cases where the agent did real work (tools, code changes)
	// but the SDK didn't capture structured output — e.g., backend agents
	// where tools are implicit.
	if (len(output) == 0 || fallback) && len(task.OutputSchema) > 0 && rm.SessionID != "" {
		log.Printf("delegate: claude-code: empty output with schema — attempting recovery formatting pass (session=%s)", rm.SessionID)
		fmtRM, fmtErr := b.formatOutput(ctx, task, rm.SessionID)
		if fmtErr == nil {
			if fmtRM.Usage != nil {
				result.Tokens += fmtRM.Usage.InputTokens + fmtRM.Usage.OutputTokens
			}
			result.FormattingPassUsed = true
			fmtOutput, fmtRawLen, fmtFallback := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
			if len(fmtOutput) > 0 {
				result.Output = fmtOutput
				result.RawOutputLen = fmtRawLen
				result.ParseFallback = fmtFallback
			} else {
				log.Printf("delegate: claude-code: recovery formatting pass also produced empty output")
			}
		} else {
			log.Printf("delegate: claude-code: recovery formatting pass failed: %v", fmtErr)
		}
	}

	return result, nil
}

// formatOutput performs the second pass of two-pass execution: resumes the
// Pass 1 session with WithOutputFormat (no tools) to guarantee structured JSON
// output conforming to the schema. The model already has full context from the
// session, so only a short formatting instruction is needed.
func (b *ClaudeCodeBackend) formatOutput(ctx context.Context, task Task, sessionID string) (*claude.ResultMessage, error) {
	// Use the parent context directly — the runtime already enforces budget
	// timeouts. Adding a short artificial timeout here risks cancelling the
	// formatting pass while the CLI is still loading the resumed session.
	fmtCtx := ctx

	var schema map[string]any
	if err := json.Unmarshal(task.OutputSchema, &schema); err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}

	opts := []claude.Option{
		claude.WithResume(sessionID),
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

	prompt := "Format your complete findings as JSON matching the required output schema."

	return promptWithTimeout(fmtCtx, prompt, opts...)
}

// promptWithTimeout wraps claude.Prompt in a goroutine with context-aware
// cancellation. The Claude Agent SDK's Prompt() function does not check
// ctx.Done() in its internal ReadLine() loop, so it can block indefinitely
// even after context cancellation. This wrapper ensures the call returns
// promptly when the context is cancelled or times out.
func promptWithTimeout(ctx context.Context, prompt string, opts ...claude.Option) (*claude.ResultMessage, error) {
	type result struct {
		rm  *claude.ResultMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		rm, err := claude.Prompt(ctx, prompt, opts...)
		ch <- result{rm, err}
	}()

	select {
	case res := <-ch:
		return res.rm, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("claude prompt cancelled: %w", ctx.Err())
	}
}
