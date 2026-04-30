package delegate

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

//go:embed codex_output_discipline.txt
var codexOutputDisciplinePreamble string

// CodexBackend delegates work to the `codex` CLI (OpenAI Codex)
// via the Codex Agent SDK.
type CodexBackend struct {
	// Command overrides the CLI binary path (default: "codex").
	Command string
	// Logger is the leveled logger for diagnostic output.
	Logger *iterlog.Logger
}

// Execute runs the codex CLI with the given task using the Codex Agent SDK.
func (b *CodexBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}

	var opts []codexsdk.Option

	// Preamble teaches frugal tool usage; task prompt follows and may override.
	systemPrompt := codexOutputDisciplinePreamble + task.SystemPromptWithInteraction()
	if systemPrompt != "" {
		opts = append(opts, codexsdk.WithSystemPrompt(systemPrompt))
	}
	if task.WorkDir != "" {
		opts = append(opts, codexsdk.WithCwd(task.WorkDir))
	}
	// Codex executes its built-in shell without routing tool_use through any
	// SDK callback, so per-name allow/deny at the SDK level has no effect on
	// the shell. Our only real lever is codex's sandbox mode. Translate the
	// AllowedTools intent into the least-privilege sandbox that still lets the
	// node do its job. bypassPermissions skips user-escalation prompts so
	// non-interactive runs don't hang; the explicit Sandbox wins over the
	// permission-mode default via session.go:187.
	opts = append(opts, codexsdk.WithSandbox(codexSandboxForAllowedTools(task.AllowedTools)))
	opts = append(opts, codexsdk.WithPermissionMode("bypassPermissions"))

	if b.Command != "" {
		opts = append(opts, codexsdk.WithCliPath(b.Command))
	}

	// Structured output: when tools are present we skip WithOutputSchema on the
	// work pass and rely on a dedicated formatting pass (formatOutput) to
	// guarantee schema-conforming JSON via session resume. Without tools the
	// single-pass WithOutputSchema is enough.
	needsTwoPass := len(task.OutputSchema) > 0 && len(task.AllowedTools) > 0
	if len(task.OutputSchema) > 0 && !needsTwoPass {
		opts = append(opts, codexsdk.WithOutputSchema(string(task.OutputSchema)))
	}

	if task.ReasoningEffort != "" {
		opts = append(opts, codexsdk.WithEffort(mapReasoningEffort(task.ReasoningEffort)))
	}

	if task.SessionID != "" {
		opts = append(opts, codexsdk.WithResume(task.SessionID))
		if task.ForkSession {
			opts = append(opts, codexsdk.WithForkSession(true))
		}
	}

	// Stream stderr for live observability and capture for diagnostics.
	var stderrBuf strings.Builder
	opts = append(opts, codexsdk.WithStderr(func(line string) {
		stderrBuf.WriteString(line)
		stderrBuf.WriteString("\n")
		if line != "" {
			b.Logger.Info("[%s] %s", task.NodeID, line)
		}
	}))

	resultMsg, totalDuration, lastThreadID, err := b.runQueryWithRetry(ctx, task, task.UserPrompt, opts)
	if err != nil {
		return Result{
			Duration:    totalDuration,
			ExitCode:    -1,
			Stderr:      stderrBuf.String(),
			BackendName: BackendCodex,
		}, err
	}

	if resultMsg == nil {
		diag := inspectCodexRollout(lastThreadID)
		errMsg := fmt.Sprintf("delegate: codex: no result message received after %d attempts", maxCodexRetries)
		if diag != "" {
			errMsg += " (" + diag + ")"
		}
		return Result{
			Duration:    totalDuration,
			ExitCode:    -1,
			Stderr:      stderrBuf.String(),
			BackendName: BackendCodex,
		}, fmt.Errorf("%s", errMsg)
	}

	result := Result{
		Duration:    totalDuration,
		ExitCode:    0,
		Stderr:      stderrBuf.String(),
		BackendName: BackendCodex,
		SessionID:   resultMsg.SessionID,
	}

	var totalIn, totalOut int
	if resultMsg.Usage != nil {
		totalIn += resultMsg.Usage.InputTokens
		totalOut += resultMsg.Usage.OutputTokens
	}
	result.Tokens = totalIn + totalOut

	if resultMsg.IsError && resultMsg.Subtype != "success" {
		return result, fmt.Errorf("delegate: codex error: subtype=%s", resultMsg.Subtype)
	}

	// Two-pass execution: when tools + schema are both present, Pass 1 output
	// is free-form text. Run Pass 2 with WithOutputSchema + read-only sandbox
	// (no writes during formatting) to guarantee structured output via session
	// resume.
	if needsTwoPass && resultMsg.SessionID != "" {
		const maxFmtAttempts = 2
		for attempt := 1; attempt <= maxFmtAttempts; attempt++ {
			b.Logger.Debug("codex [formatting pass %d/%d] starting structured output extraction (session=%s)", attempt, maxFmtAttempts, resultMsg.SessionID)
			fmtRM, fmtDuration, fmtErr := b.formatOutput(ctx, task, resultMsg.SessionID)
			result.Duration += fmtDuration
			if fmtErr != nil {
				if attempt < maxFmtAttempts {
					b.Logger.Warn("codex [formatting pass %d/%d] failed, retrying: %v", attempt, maxFmtAttempts, fmtErr)
					continue
				}
				return result, fmt.Errorf("delegate: codex formatting pass failed: %w", fmtErr)
			}
			if fmtRM.Usage != nil {
				totalIn += fmtRM.Usage.InputTokens
				totalOut += fmtRM.Usage.OutputTokens
				result.Tokens = totalIn + totalOut
			}
			result.FormattingPassUsed = true

			output, rawLen, fallback := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
			if fallback && attempt < maxFmtAttempts {
				b.Logger.Warn("codex [formatting pass %d/%d] produced fallback text, retrying", attempt, maxFmtAttempts)
				continue
			}
			result.Output = output
			result.RawOutputLen = rawLen
			result.ParseFallback = fallback
			cost.Annotate(result.Output, task.Model, totalIn, totalOut)
			return result, nil
		}
	}

	output, rawLen, fallback := parseSDKOutput(resultMsg.Result, resultMsg.StructuredOutput, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	// Recovery pass: if schema is set but we got empty/fallback output, retry
	// via session resume with WithOutputSchema. Mirrors claude_code.go:219-238.
	if (len(output) == 0 || fallback) && len(task.OutputSchema) > 0 && resultMsg.SessionID != "" {
		b.Logger.Debug("codex: empty output with schema — attempting recovery formatting pass (session=%s)", resultMsg.SessionID)
		fmtRM, fmtDuration, fmtErr := b.formatOutput(ctx, task, resultMsg.SessionID)
		result.Duration += fmtDuration
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
				b.Logger.Warn("codex: recovery formatting pass also produced empty output")
			}
		} else {
			b.Logger.Warn("codex: recovery formatting pass failed: %v", fmtErr)
		}
	}

	cost.Annotate(result.Output, task.Model, totalIn, totalOut)
	return result, nil
}

const maxCodexRetries = 3

// runQueryWithRetry drives codex Query to completion, retrying up to
// maxCodexRetries when the process exits without producing a ResultMessage
// (a known transient failure mode).
func (b *CodexBackend) runQueryWithRetry(ctx context.Context, task Task, prompt string, opts []codexsdk.Option) (*codexsdk.ResultMessage, time.Duration, string, error) {
	var totalDuration time.Duration
	var lastThreadID string

	for attempt := 1; attempt <= maxCodexRetries; attempt++ {
		startTime := time.Now()
		var resultMsg *codexsdk.ResultMessage
		var queryErr error

		for msg, err := range codexsdk.Query(ctx, codexsdk.Text(prompt), opts...) {
			if err != nil {
				queryErr = err
				break
			}
			switch m := msg.(type) {
			case *codexsdk.AssistantMessage:
				b.logAssistantActivity(task.NodeID, m)
			case *codexsdk.ResultMessage:
				resultMsg = m
			case *codexsdk.SystemMessage:
				switch m.Subtype {
				case "thread.started":
					if tid, ok := m.Data["thread_id"].(string); ok && tid != "" {
						lastThreadID = tid
					}
				case "thread.token_usage.updated":
					b.logTokenUsage(task.NodeID, m.Data)
				}
			}
		}

		totalDuration += time.Since(startTime)

		if queryErr != nil {
			return nil, totalDuration, lastThreadID, fmt.Errorf("delegate: codex failed: %w", queryErr)
		}

		if resultMsg != nil {
			return resultMsg, totalDuration, lastThreadID, nil
		}

		// No ResultMessage: inspect the rollout log to classify the failure.
		// Overflow is not retryable — same prompt will overflow again and
		// burn tokens. Break with a clear error so the caller fails fast.
		diag := inspectCodexRollout(lastThreadID)
		if strings.Contains(diag, "context window") {
			return nil, totalDuration, lastThreadID, fmt.Errorf("delegate: codex: %s", diag)
		}

		if attempt < maxCodexRetries {
			select {
			case <-ctx.Done():
				return nil, totalDuration, lastThreadID, fmt.Errorf("delegate: codex: context cancelled during retry: %w", ctx.Err())
			default:
			}
			if diag != "" {
				b.Logger.Warn("[%s] codex returned no result (attempt %d/%d, %s), retrying", task.NodeID, attempt, maxCodexRetries, diag)
			} else {
				b.Logger.Warn("[%s] codex returned no result (attempt %d/%d), retrying", task.NodeID, attempt, maxCodexRetries)
			}
		}
	}

	return nil, totalDuration, lastThreadID, nil
}

// formatOutput performs a second pass: resumes the work-pass session with
// WithOutputSchema and a tight formatting prompt. Sandbox is forced to
// read-only so the pass cannot mutate state while rendering the final JSON.
func (b *CodexBackend) formatOutput(ctx context.Context, task Task, sessionID string) (*codexsdk.ResultMessage, time.Duration, error) {
	opts := []codexsdk.Option{
		codexsdk.WithResume(sessionID),
		codexsdk.WithOutputSchema(string(task.OutputSchema)),
		codexsdk.WithSandbox("read-only"),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithStderr(func(line string) {
			if line != "" {
				b.Logger.Info("[%s/fmt] %s", task.NodeID, line)
			}
		}),
	}
	if task.WorkDir != "" {
		opts = append(opts, codexsdk.WithCwd(task.WorkDir))
	}
	if b.Command != "" {
		opts = append(opts, codexsdk.WithCliPath(b.Command))
	}
	if task.ReasoningEffort != "" {
		opts = append(opts, codexsdk.WithEffort(mapReasoningEffort(task.ReasoningEffort)))
	}

	prompt := "Format your complete findings as JSON matching the required output schema. Do not call any tools; just return the JSON."

	rm, duration, _, err := b.runQueryWithRetry(ctx, task, prompt, opts)
	if err != nil {
		return nil, duration, err
	}
	if rm == nil {
		return nil, duration, fmt.Errorf("codex formatting pass: no result message")
	}
	if rm.IsError && rm.Subtype != "success" {
		return rm, duration, fmt.Errorf("codex formatting pass error: subtype=%s", rm.Subtype)
	}
	return rm, duration, nil
}

// logTokenUsage extracts totals from a thread.token_usage.updated event and
// logs them live. Codex emits this a few times per turn; surfacing it lets
// operators see context growth before a silent overflow (inspectCodexRollout
// remains the post-mortem safety net). Data shape: tokenUsage.last.{total,input,cached,output,reasoning}Tokens.
func (b *CodexBackend) logTokenUsage(nodeID string, data map[string]any) {
	tu, ok := data["tokenUsage"].(map[string]any)
	if !ok {
		return
	}
	last, ok := tu["last"].(map[string]any)
	if !ok {
		return
	}
	total := asInt(last["totalTokens"])
	input := asInt(last["inputTokens"])
	cached := asInt(last["cachedInputTokens"])
	output := asInt(last["outputTokens"])
	reasoning := asInt(last["reasoningOutputTokens"])
	if total == 0 && input == 0 && output == 0 {
		return
	}
	b.Logger.Info("[%s/codex] 📊 tokens total=%d (input=%d cached=%d output=%d reasoning=%d)",
		nodeID, total, input, cached, output, reasoning)
}

func asInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}

// logAssistantActivity logs tool calls, text output, and tool errors from an
// AssistantMessage in real-time, mirroring the claude_code backend's activity
// streaming.
func (b *CodexBackend) logAssistantActivity(nodeID string, msg *codexsdk.AssistantMessage) {
	for _, block := range msg.Content {
		switch blk := block.(type) {
		case *codexsdk.ToolUseBlock:
			detail := toolUseDetail(blk.Name, blk.Input)
			b.Logger.Info("[%s/codex] 🔧 %s %s", nodeID, blk.Name, detail)
		case *codexsdk.ToolResultBlock:
			if blk.IsError {
				b.Logger.Info("[%s/codex] ❌ tool error: %s", nodeID, contentBlocksText(blk.Content))
			}
		case *codexsdk.TextBlock:
			if blk.Text != "" {
				b.Logger.Info("[%s/codex] 💬 %s", nodeID, truncate(blk.Text, 300))
			}
		}
	}
}

// inspectCodexRollout returns a short diagnostic pulled from the last event
// of ~/.codex/sessions/.../rollout-*-<threadID>.jsonl. Used when codex exits
// without sending turn.completed/turn.failed (e.g. context-window overflow).
// Returns "" when nothing useful can be extracted.
func inspectCodexRollout(threadID string) string {
	if threadID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Codex writes rollouts to ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<thread_id>.jsonl.
	pattern := filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return ""
	}
	// With a unique thread_id there should be exactly one match; pick the
	// first defensively.
	path := matches[0]

	// Read the last non-empty JSONL event. Small files — full scan is fine.
	f, err := os.Open(path) // #nosec G304 — path is built from a thread_id we just saw come out of the SDK and a fixed ~/.codex/sessions prefix.
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var last map[string]any
	scanner := bufio.NewScanner(f)
	// Allow large lines (tool outputs can be big).
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err == nil {
			last = ev
		}
	}
	if last == nil {
		return ""
	}

	// Pull out the payload type and check for a context-window overflow.
	payload, _ := last["payload"].(map[string]any)
	pType, _ := payload["type"].(string)
	evType, _ := last["type"].(string)

	if evType == "event_msg" && pType == "token_count" {
		info, _ := payload["info"].(map[string]any)
		tot, _ := info["total_token_usage"].(map[string]any)
		total, _ := tot["total_tokens"].(float64)
		window, _ := info["model_context_window"].(float64)
		if total > 0 && window > 0 && total > window {
			return fmt.Sprintf("codex likely hit context window: total_tokens=%d > model_context_window=%d; reduce prompt size or use a larger-context model", int(total), int(window))
		}
		return fmt.Sprintf("codex exited without completion; last event was token_count (total_tokens=%d, window=%d)", int(total), int(window))
	}
	if evType != "" || pType != "" {
		return fmt.Sprintf("codex exited without completion; last rollout event was %s/%s", evType, pType)
	}
	return ""
}

// contentBlocksText flattens a ContentBlock slice for logging; truncates to 500 chars.
func contentBlocksText(blocks []codexsdk.ContentBlock) string {
	if len(blocks) == 0 {
		return "<empty>"
	}
	var sb strings.Builder
	for i, blk := range blocks {
		if i > 0 {
			sb.WriteString(" | ")
		}
		switch b := blk.(type) {
		case *codexsdk.TextBlock:
			sb.WriteString(b.Text)
		default:
			fmt.Fprintf(&sb, "<%s>", blk.BlockType())
		}
	}
	return truncate(sb.String(), 500)
}

// codexSandboxForAllowedTools picks the least-privilege codex sandbox mode
// compatible with the intent expressed by AllowedTools:
//   - empty allowlist or read-only tools only → "read-only"
//   - any mutating tool (Bash/Edit/Write/NotebookEdit) → "workspace-write"
//
// "danger-full-access" is intentionally never chosen here — workflows that
// truly need unrestricted network or out-of-workspace writes must request it
// via an explicit codex config override, not by listing a mutating tool.
func codexSandboxForAllowedTools(allowed []string) string {
	for _, t := range allowed {
		switch t {
		case "Bash", "Edit", "Write", "NotebookEdit":
			return "workspace-write"
		}
	}
	return "read-only"
}

// mapReasoningEffort converts iterion reasoning effort strings to Codex SDK Effort constants.
func mapReasoningEffort(s string) codexsdk.Effort {
	switch s {
	case "low":
		return codexsdk.EffortLow
	case "medium":
		return codexsdk.EffortMedium
	case "high":
		return codexsdk.EffortHigh
	case "extra_high":
		return codexsdk.EffortMax
	default:
		return codexsdk.EffortMedium
	}
}
