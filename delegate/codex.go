package delegate

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"

	iterlog "github.com/SocialGouv/iterion/log"
)

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

	// Build system prompt, optionally augmented with interaction instructions.
	systemPrompt := task.SystemPrompt
	if task.InteractionEnabled {
		systemPrompt += interactionSystemInstruction
	}
	if systemPrompt != "" {
		opts = append(opts, codexsdk.WithSystemPrompt(systemPrompt))
	}
	if task.WorkDir != "" {
		opts = append(opts, codexsdk.WithCwd(task.WorkDir))
	}
	if len(task.AllowedTools) > 0 {
		opts = append(opts, codexsdk.WithAllowedTools(task.AllowedTools...))
	}
	// Bypass interactive permission prompts: the runtime enforces safety via
	// workspace isolation and allowed-tool lists, so the delegate subprocess
	// does not need its own permission gate.
	opts = append(opts, codexsdk.WithPermissionMode("bypassPermissions"))

	if b.Command != "" {
		opts = append(opts, codexsdk.WithCliPath(b.Command))
	}

	if len(task.OutputSchema) > 0 {
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

	prompt := task.UserPrompt

	const maxRetries = 3
	var resultMsg *codexsdk.ResultMessage
	var queryErr error
	var totalDuration time.Duration
	var lastThreadID string

	for attempt := 1; attempt <= maxRetries; attempt++ {
		startTime := time.Now()
		resultMsg = nil
		queryErr = nil

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
				if m.Subtype == "thread.started" {
					if tid, ok := m.Data["thread_id"].(string); ok && tid != "" {
						lastThreadID = tid
					}
				}
			}
		}

		totalDuration += time.Since(startTime)

		if queryErr != nil {
			return Result{
				Duration:    totalDuration,
				ExitCode:    -1,
				Stderr:      stderrBuf.String(),
				BackendName: BackendCodex,
			}, fmt.Errorf("delegate: codex failed: %w", queryErr)
		}

		if resultMsg != nil {
			break // success
		}

		// Codex process exited without producing a ResultMessage.
		// This is a known transient failure — retry unless context is done.
		if attempt < maxRetries {
			select {
			case <-ctx.Done():
				return Result{
					Duration:    totalDuration,
					ExitCode:    -1,
					Stderr:      stderrBuf.String(),
					BackendName: BackendCodex,
				}, fmt.Errorf("delegate: codex: context cancelled during retry: %w", ctx.Err())
			default:
			}
			b.Logger.Warn("[%s] codex returned no result (attempt %d/%d), retrying", task.NodeID, attempt, maxRetries)
		}
	}

	if resultMsg == nil {
		diag := inspectCodexRollout(lastThreadID)
		errMsg := fmt.Sprintf("delegate: codex: no result message received after %d attempts", maxRetries)
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

	if resultMsg.Usage != nil {
		result.Tokens = resultMsg.Usage.InputTokens + resultMsg.Usage.OutputTokens
	}

	if resultMsg.IsError && resultMsg.Subtype != "success" {
		return result, fmt.Errorf("delegate: codex error: subtype=%s", resultMsg.Subtype)
	}

	output, rawLen, fallback := parseSDKOutput(resultMsg.Result, resultMsg.StructuredOutput, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	return result, nil
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
				text := blk.Text
				if len(text) > 300 {
					text = text[:300] + "..."
				}
				b.Logger.Info("[%s/codex] 💬 %s", nodeID, text)
			}
		}
	}
}

// inspectCodexRollout locates the session rollout file for the given thread_id
// under ~/.codex/sessions/ and returns a short diagnostic extracted from its
// last event. Used when the SDK Query iterator returns without a ResultMessage
// — which typically means the codex CLI exited without sending turn.completed
// or turn.failed (e.g. on context-window overflow). Returns "" if nothing
// useful can be extracted; callers should treat that as "no extra info".
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

// contentBlocksText flattens a ContentBlock slice (as used by ToolResultBlock.Content)
// into a single readable string, extracting text from TextBlocks and a short type
// tag for unrecognized blocks. Truncates to 500 chars to keep logs bounded.
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
	out := sb.String()
	if len(out) > 500 {
		out = out[:500] + "..."
	}
	return out
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
