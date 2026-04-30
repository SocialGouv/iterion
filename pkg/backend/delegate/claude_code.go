package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// askUserMCPServerName is the name under which iterion registers itself as an
// MCP server exposing the ask_user tool. The CLI prefixes MCP tool names as
// "mcp__<server>__<tool>", so the LLM sees the tool as "mcp__iterion__ask_user".
const askUserMCPServerName = "iterion"

// askUserMCPToolName is the fully-qualified name of the ask_user tool as the
// CLI exposes it to the LLM.
const askUserMCPToolName = "mcp__iterion__ask_user"

// askUserMCPSubcommand is the hidden iterion subcommand that runs an MCP stdio
// server exposing only the ask_user tool. See cmd/iterion/mcp_ask_user.go.
const askUserMCPSubcommand = "__mcp-ask-user"

// defaultClaudeCodeModel is the model iterion forces on the claude_code
// backend when the workflow doesn't specify one. Mirrors the official
// Claude Code CLI default — Opus 4.7 (1M context window). Workflows can
// always override via the node's `model:` field.
const defaultClaudeCodeModel = "claude-opus-4-7"

// defaultClaudeCodeEffort is the reasoning effort iterion forces on the
// claude_code backend when the workflow doesn't specify one. Mirrors the
// official Claude Code CLI default ("extra_high" thinking budget for
// Opus). Workflows can always override via `reasoning_effort:`.
const defaultClaudeCodeEffort = "extra_high"

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

	systemPrompt := task.SystemPromptWithInteraction()
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

	model := task.Model
	if model == "" {
		model = defaultClaudeCodeModel
	}
	opts = append(opts, claudesdk.WithModel(model))

	if b.Command != "" {
		opts = append(opts, claudesdk.WithCLIPath(b.Command))
	}

	effort := task.ReasoningEffort
	if effort == "" {
		effort = defaultClaudeCodeEffort
	}
	opts = append(opts, claudesdk.WithEnv("CLAUDE_CODE_EFFORT_LEVEL", effort))

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

	// Capture stderr for post-session diagnostics AND surface every
	// line live so the user can see what the CLI is doing during long
	// reasoning intervals. Without live stderr, the SDK is a black box
	// while it streams thinking tokens or reads files: the runtime
	// emits nothing between "Delegation started" and the final
	// AssistantMessage, which can be many minutes for Opus extra_high.
	var stderrBuf strings.Builder
	opts = append(opts, claudesdk.WithStderrCallback(func(line string) {
		stderrBuf.WriteString(line)
		stderrBuf.WriteString("\n")
		if line != "" {
			b.Logger.Info("[%s/claude-code:err] %s", task.NodeID, line)
		}
	}))

	// Native ask_user interception. When the workflow enables interaction, we
	// register iterion itself as an MCP server exposing the ask_user tool, and
	// install a PreToolUse hook that captures the question and short-circuits
	// the session as soon as the LLM calls it. This mirrors the claw backend's
	// in-process ask_user path. The system prompt's [INTERACTION PROTOCOL]
	// suffix is preserved so the existing JSON-output fallback still works if
	// the LLM bypasses the native tool.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	var pendingQuestion atomic.Value // string

	if task.InteractionEnabled {
		if selfPath, err := os.Executable(); err == nil {
			opts = append(opts, claudesdk.WithMCPServer(askUserMCPServerName, &claudesdk.MCPStdioServer{
				Command: selfPath,
				Args:    []string{askUserMCPSubcommand},
			}))
			// Only extend the allowlist when the workflow already restricts tools.
			// An empty AllowedTools means "no restriction", and the MCP tool will
			// be discoverable without explicit listing.
			if len(task.AllowedTools) > 0 {
				combined := make([]string, 0, len(task.AllowedTools)+1)
				combined = append(combined, task.AllowedTools...)
				combined = append(combined, askUserMCPToolName)
				opts = append(opts, claudesdk.WithAllowedTools(combined...))
			}
			matcher := "^" + askUserMCPToolName + "$"
			noContinue := false
			opts = append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
				Matcher: &matcher,
				Handler: func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
					if q, ok := in.ToolInput["question"].(string); ok && q != "" {
						pendingQuestion.Store(q)
						cancelStream()
					}
					return claudesdk.HookOutput{
						Decision:      "deny",
						Continue:      &noContinue,
						SystemMessage: "ask_user has been escalated to the iterion runtime; stop generating.",
					}, nil
				},
			}))
		} else {
			b.Logger.Warn("[%s/claude-code] could not resolve iterion binary path; native ask_user MCP server disabled (falling back to JSON _needs_interaction protocol): %v", task.NodeID, err)
		}
	}

	startTime := time.Now()
	rm, streamErr := b.runSession(streamCtx, prompt, task, opts)
	duration := time.Since(startTime)

	// Native ask_user capture takes precedence over any error: if the hook
	// fired, the resulting context cancellation surfaces here as ctx.Err(),
	// which we must not treat as a failure.
	if q, ok := pendingQuestion.Load().(string); ok && q != "" {
		b.Logger.Info("[%s/claude-code] 🛑 ask_user escalated via native MCP tool", task.NodeID)
		sessID := ""
		if rm != nil {
			sessID = rm.SessionID
		}
		return Result{
			Output: map[string]interface{}{
				"_needs_interaction": true,
				"_interaction_questions": map[string]interface{}{
					AskUserQuestionKey: q,
				},
			},
			Duration:    duration,
			ExitCode:    0,
			Stderr:      stderrBuf.String(),
			BackendName: BackendClaudeCode,
			SessionID:   sessID,
		}, nil
	}

	if streamErr != nil {
		return Result{
			Duration:    duration,
			ExitCode:    -1,
			Stderr:      stderrBuf.String(),
			BackendName: BackendClaudeCode,
		}, fmt.Errorf("delegate: claude-code failed: %w", streamErr)
	}

	result := Result{
		Duration:    duration,
		ExitCode:    0,
		Stderr:      stderrBuf.String(),
		BackendName: BackendClaudeCode,
		SessionID:   rm.SessionID,
	}

	var totalIn, totalOut int
	if rm.Usage != nil {
		totalIn += rm.Usage.InputTokens
		totalOut += rm.Usage.OutputTokens
	}
	result.Tokens = totalIn + totalOut

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
				totalIn += fmtRM.Usage.InputTokens
				totalOut += fmtRM.Usage.OutputTokens
				result.Tokens = totalIn + totalOut
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
			cost.Annotate(result.Output, task.Model, totalIn, totalOut)
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
				totalIn += fmtRM.Usage.InputTokens
				totalOut += fmtRM.Usage.OutputTokens
				result.Tokens = totalIn + totalOut
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

	cost.Annotate(result.Output, task.Model, totalIn, totalOut)
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
	model := task.Model
	if model == "" {
		model = defaultClaudeCodeModel
	}
	opts = append(opts, claudesdk.WithModel(model))
	if b.Command != "" {
		opts = append(opts, claudesdk.WithCLIPath(b.Command))
	}

	prompt := "Format your complete findings as JSON matching the required output schema."

	return promptWithTimeout(fmtCtx, prompt, opts...)
}

// promptWithTimeout wraps claudesdk.Prompt in a goroutine with context-aware
// cancellation. The Claude Agent SDK's Prompt() function may not check
// ctx.Done() in its internal ReadLine() loop on every read, so this wrapper
// ensures the call returns promptly when the context is cancelled. Used only
// by formatOutput (no hooks needed); the main Execute path uses Session.
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

// Stream-timeout tiers calibrated for the two failure shapes we
// see in practice:
//
//   - **Cold timeout** (no message yet, session never produced a
//     SystemMessage/AssistantMessage): an SDK or process deadlock
//     manifests immediately. We want to fail fast so the recovery
//     dispatcher can retry without burning minutes on a corpse.
//
//   - **Hot timeout** (at least one message received): claude is
//     genuinely working — possibly waiting on a sub-agent or a
//     long-running tool call. We give it significantly more leeway
//     before declaring the session stuck. Sub-agent runs commonly
//     take 5–10 min before producing the next visible message.
//
// Override either via env (Go duration strings):
//   - ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT
//   - ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT (the hot timeout —
//     name kept for back-compat with earlier behavior).
//
// Set either to "0" to disable that tier.
const (
	defaultStreamColdTimeout = 90 * time.Second
	defaultStreamHotTimeout  = 15 * time.Minute
)

func resolveStreamColdTimeout() time.Duration {
	if v := os.Getenv("ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultStreamColdTimeout
}

func resolveStreamHotTimeout() time.Duration {
	if v := os.Getenv("ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultStreamHotTimeout
}

// runSession opens an interactive Session with the Claude CLI, sends the
// prompt, and consumes the message stream until a ResultMessage arrives. It
// streams agent activity (tool_use, tool_result, text) directly from the typed
// content blocks to the iterion logger — this replaces the previous raw-JSON
// WithMessageCallback path. Hooks (PreToolUse, etc.) only fire when configured
// via Session, which is why we use this mode rather than one-shot Prompt().
//
// An idle-timeout watchdog aborts the session when no message arrives for
// `streamIdleTimeout` — protecting against hung Claude CLI processes that
// otherwise block indefinitely (we observed the SDK occasionally getting
// stuck in ep_poll without any propagated error). The aborted session
// returns an error the runtime classifies as resumable, so the recovery
// dispatcher retries automatically.
func (b *ClaudeCodeBackend) runSession(ctx context.Context, prompt string, task Task, opts []claudesdk.Option) (*claudesdk.ResultMessage, error) {
	sess := claudesdk.NewSession(opts...)
	defer func() { _ = sess.Close() }()

	if err := sess.Send(ctx, prompt); err != nil {
		return nil, err
	}

	coldTimeout := resolveStreamColdTimeout()
	hotTimeout := resolveStreamHotTimeout()

	// Forward messages from the SDK iterator into a channel so we can
	// select on (msg, idle-timer, ctx.Done) and abort cleanly when
	// the session falls silent. Using range-over-func directly would
	// block the goroutine until ctx is cancelled, which the runtime
	// only does at the workflow's max_duration (way too late).
	type streamItem struct {
		msg claudesdk.Message
		err error
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	items := make(chan streamItem, 1)
	go func() {
		defer close(items)
		for msg, err := range sess.Stream(streamCtx) {
			select {
			case items <- streamItem{msg: msg, err: err}:
			case <-streamCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// receivedAny tracks whether the session has emitted at least one
	// message. While false we apply the tighter cold timeout (hung
	// SDK / deadlocked subprocess); once any progress is observed we
	// switch to the hot timeout (give claude room for sub-agent runs
	// or other long tool calls).
	receivedAny := false
	var result *claudesdk.ResultMessage
	currentTimeout := coldTimeout
	idle := time.NewTimer(currentTimeout)
	defer idle.Stop()

	for {
		// Pick the timeout that matches the current phase and reset
		// the timer for this iteration. Any progress (assistant
		// tokens, tool calls, tool results) flips us into hot mode
		// and grants the longer budget on every subsequent wait.
		if receivedAny {
			currentTimeout = hotTimeout
		} else {
			currentTimeout = coldTimeout
		}
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		if currentTimeout > 0 {
			idle.Reset(currentTimeout)
		}

		select {
		case it, ok := <-items:
			if !ok {
				// Stream closed without surfacing an error.
				if result == nil {
					return nil, fmt.Errorf("claude session ended without result message")
				}
				return result, nil
			}
			if it.err != nil {
				return result, it.err
			}
			// Any incoming item proves the SDK is alive — flip into
			// hot-timeout mode for the rest of the session.
			receivedAny = true
			switch m := it.msg.(type) {
			case *claudesdk.SystemMessage:
				// `init` is the canonical session start: model + tool list +
				// MCP server count are interesting for debugging at info
				// level. Hook lifecycle subtypes (hook_started, hook_response,
				// hook_progress) fire repeatedly during a session and would
				// flood the log; route them to debug.
				if m.Subtype == "init" {
					b.Logger.Info("[%s/claude-code] ⚙️  system/init session=%s model=%s tools=%d mcp=%d",
						task.NodeID, m.SessionID, m.Model, m.ToolCount(), m.MCPServerCount())
				} else {
					b.Logger.Debug("[%s/claude-code] ⚙️  system/%s session=%s",
						task.NodeID, m.Subtype, m.SessionID)
				}
			case *claudesdk.AssistantMessage:
				if m.Message != nil {
					logAssistantContent(b.Logger, task.NodeID, m.Message.Content)
				}
			case *claudesdk.UserMessage:
				b.Logger.Debug("[%s/claude-code] 👤 user message echoed back", task.NodeID)
			case *claudesdk.ResultMessage:
				result = m
			default:
				if it.msg != nil {
					b.Logger.Debug("[%s/claude-code] 📨 %T message", task.NodeID, it.msg)
				}
			}
		case <-idle.C:
			if currentTimeout <= 0 {
				continue
			}
			cancelStream()
			phase := "cold"
			envHint := "ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT"
			if receivedAny {
				phase = "hot"
				envHint = "ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT"
			}
			b.Logger.Warn("[%s/claude-code] no SDK message for %s (%s phase) — aborting",
				task.NodeID, currentTimeout, phase)
			return result, fmt.Errorf("claude session idle for %s (%s phase) — aborting (set %s to extend, or 0 to disable)", currentTimeout, phase, envHint)
		case <-ctx.Done():
			cancelStream()
			return result, ctx.Err()
		}
	}
}

// logAssistantContent emits human-readable info logs for tool calls, tool
// errors, and text deltas from a single assistant message.
func logAssistantContent(logger *iterlog.Logger, nodeID string, blocks []claudesdk.ContentBlock) {
	for _, block := range blocks {
		switch bl := block.(type) {
		case *claudesdk.ToolUseBlock:
			displayName := bl.Name
			for _, prefix := range []string{"mcp__claude_code__", "mcp__plugin_claude-mem_mcp-search__", "mcp__iterion__"} {
				if strings.HasPrefix(displayName, prefix) {
					displayName = displayName[len(prefix):]
					break
				}
			}
			logger.Info("[%s/claude-code] 🔧 %s %s", nodeID, displayName, toolUseDetail(bl.Name, bl.Input))
		case *claudesdk.ToolResultBlock:
			if bl.IsError {
				logger.Info("[%s/claude-code] ❌ tool error: %v", nodeID, bl.Content)
			}
		case *claudesdk.TextBlock:
			if bl.Text != "" {
				logger.Info("[%s/claude-code] 💬 %s", nodeID, truncate(bl.Text, 300))
			}
		}
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
		return truncate(p, 80)
	}
	// Bash: show the command (truncated)
	if c, ok := input["command"].(string); ok {
		return truncate(c, 100)
	}
	return ""
}
