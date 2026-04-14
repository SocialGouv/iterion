package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/delegate/claudesdk"

	iterlog "github.com/SocialGouv/iterion/log"
)

// ClaudeCodeBackend delegates work to the `claude` CLI (claude-code)
// via the Claude Agent SDK.
type ClaudeCodeBackend struct {
	// Command overrides the CLI binary path (default: "claude").
	Command string
	// Logger is the leveled logger for diagnostic output.
	Logger *iterlog.Logger
}

// Execute runs the claude CLI with the given task using the Claude Agent SDK.
func (b *ClaudeCodeBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}

	var opts []claudesdk.Option

	// Build system prompt, optionally augmented with interaction instructions.
	systemPrompt := task.SystemPrompt
	if task.InteractionEnabled {
		systemPrompt += interactionSystemInstruction
	}
	if systemPrompt != "" {
		opts = append(opts, claudesdk.WithSystemPrompt(systemPrompt))
	}
	if task.WorkDir != "" {
		opts = append(opts, claudesdk.WithCwd(task.WorkDir))
	}
	if len(task.AllowedTools) > 0 {
		opts = append(opts, claudesdk.WithAllowedTools(task.AllowedTools...))
	}
	// Bypass interactive permission prompts: the runtime enforces safety via
	// workspace isolation and allowed-tool lists, so the delegate subprocess
	// does not need its own permission gate.
	opts = append(opts, claudesdk.WithPermissionMode("bypassPermissions"))

	// The CLI requires --verbose when using --output-format=stream-json in
	// --print mode. The SDK always uses stream-json, so we must enable verbose.
	opts = append(opts, claudesdk.WithVerbose(true))

	if b.Command != "" {
		opts = append(opts, claudesdk.WithCLIPath(b.Command))
	}

	if task.ReasoningEffort != "" {
		opts = append(opts, claudesdk.WithEnv("CLAUDE_CODE_EFFORT_LEVEL", task.ReasoningEffort))
	}

	if task.SessionID != "" {
		opts = append(opts, claudesdk.WithResume(task.SessionID))
		if task.ForkSession {
			opts = append(opts, claudesdk.WithForkSession(true))
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
			opts = append(opts, claudesdk.WithOutputFormat(schema))
		}
	}

	// Capture stderr for diagnostics.
	var stderrBuf strings.Builder
	opts = append(opts, claudesdk.WithStderrCallback(func(line string) {
		stderrBuf.WriteString(line)
		stderrBuf.WriteString("\n")
	}))

	// Stream agent activity in real-time via NDJSON message callback.
	opts = append(opts, claudesdk.WithMessageCallback(func(msgType string, data json.RawMessage) {
		switch msgType {
		case "assistant":
			var msg struct {
				Message struct {
					Content []json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(data, &msg) != nil {
				return
			}
			for _, raw := range msg.Message.Content {
				var probe struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(raw, &probe) != nil {
					continue
				}
				switch probe.Type {
				case "tool_use":
					var tu struct {
						Name  string         `json:"name"`
						Input map[string]any `json:"input"`
					}
					if json.Unmarshal(raw, &tu) == nil {
						detail := toolUseDetail(tu.Name, tu.Input)
						b.Logger.Info("[%s] 🔧 %s %s", task.NodeID, tu.Name, detail)
					}
				case "tool_result":
					var tr struct {
						Content any  `json:"content"`
						IsError bool `json:"is_error"`
					}
					if json.Unmarshal(raw, &tr) == nil && tr.IsError {
						b.Logger.Info("[%s] ❌ tool error: %v", task.NodeID, tr.Content)
					}
				case "text":
					var tb struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(raw, &tb) == nil && tb.Text != "" {
						text := tb.Text
						if len(text) > 300 {
							text = text[:300] + "..."
						}
						b.Logger.Info("[%s] 💬 %s", task.NodeID, text)
					}
				}
			}
		}
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

	if rm.IsError && rm.Subtype != claudesdk.ResultSuccess {
		return result, fmt.Errorf("delegate: claude-code error: subtype=%s", rm.Subtype)
	}

	// Two-pass execution: when tools + schema are both present, Pass 1 output
	// is free-form text. We always run Pass 2 with WithOutputFormat to guarantee
	// structured output conforming to the schema via session resume.
	if needsTwoPass && rm.SessionID != "" {
		const maxFmtAttempts = 2
		for attempt := 1; attempt <= maxFmtAttempts; attempt++ {
			b.Logger.Debug("claude-code [formatting pass %d/%d] starting structured output extraction (session=%s)", attempt, maxFmtAttempts, rm.SessionID)
			fmtRM, fmtErr := b.formatOutput(ctx, task, rm.SessionID)
			if fmtErr != nil {
				if attempt < maxFmtAttempts {
					b.Logger.Warn("claude-code [formatting pass %d/%d] failed, retrying: %v", attempt, maxFmtAttempts, fmtErr)
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
				b.Logger.Warn("claude-code [formatting pass %d/%d] produced fallback text, retrying", attempt, maxFmtAttempts)
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
		b.Logger.Debug("claude-code: empty output with schema — attempting recovery formatting pass (session=%s)", rm.SessionID)
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
				b.Logger.Warn("claude-code: recovery formatting pass also produced empty output")
			}
		} else {
			b.Logger.Warn("claude-code: recovery formatting pass failed: %v", fmtErr)
		}
	}

	return result, nil
}

// formatOutput performs the second pass of two-pass execution: resumes the
// Pass 1 session with WithOutputFormat (no tools) to guarantee structured JSON
// output conforming to the schema. The model already has full context from the
// session, so only a short formatting instruction is needed.
func (b *ClaudeCodeBackend) formatOutput(ctx context.Context, task Task, sessionID string) (*claudesdk.ResultMessage, error) {
	// Use the parent context directly — the runtime already enforces budget
	// timeouts. Adding a short artificial timeout here risks cancelling the
	// formatting pass while the CLI is still loading the resumed session.
	fmtCtx := ctx

	var schema map[string]any
	if err := json.Unmarshal(task.OutputSchema, &schema); err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}

	opts := []claudesdk.Option{
		claudesdk.WithResume(sessionID),
		claudesdk.WithOutputFormat(schema),
		claudesdk.WithPermissionMode("bypassPermissions"),
		claudesdk.WithVerbose(true),
		claudesdk.WithStderrCallback(func(line string) {
			if line != "" {
				b.Logger.Info("[%s/fmt] %s", task.NodeID, line)
			}
		}),
	}
	if task.WorkDir != "" {
		opts = append(opts, claudesdk.WithCwd(task.WorkDir))
	}
	if b.Command != "" {
		opts = append(opts, claudesdk.WithCLIPath(b.Command))
	}

	prompt := "Format your complete findings as JSON matching the required output schema."

	return promptWithTimeout(fmtCtx, prompt, opts...)
}

// promptWithTimeout wraps claudesdk.Prompt in a goroutine with context-aware
// cancellation. The Claude Agent SDK's Prompt() function does not check
// ctx.Done() in its internal ReadLine() loop, so it can block indefinitely
// even after context cancellation. This wrapper ensures the call returns
// promptly when the context is cancelled or times out.
func promptWithTimeout(ctx context.Context, prompt string, opts ...claudesdk.Option) (*claudesdk.ResultMessage, error) {
	type result struct {
		rm  *claudesdk.ResultMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		rm, err := claudesdk.Prompt(ctx, prompt, opts...)
		ch <- result{rm, err}
	}()

	select {
	case res := <-ch:
		return res.rm, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("claude prompt cancelled: %w", ctx.Err())
	}
}

// toolUseDetail extracts a human-readable detail from tool input.
func toolUseDetail(name string, input map[string]any) string {
	// File-related tools: show the path
	if p, ok := input["file_path"].(string); ok {
		return p
	}
	if p, ok := input["path"].(string); ok {
		return p
	}
	// Search/grep: show the pattern
	if p, ok := input["pattern"].(string); ok {
		if len(p) > 80 {
			return p[:80] + "..."
		}
		return p
	}
	// Bash: show the command (truncated)
	if c, ok := input["command"].(string); ok {
		if len(c) > 100 {
			return c[:100] + "..."
		}
		return c
	}
	return ""
}
