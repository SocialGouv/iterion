package model

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// maxFieldSize is the maximum byte length for a single content field in an event.
// Fields exceeding this limit are truncated to stay within the 10 MB event line limit.
const maxFieldSize = 1 << 20 // 1 MB

// EventEmitter is the subset of store.RunStore used by the event bridge.
type EventEmitter interface {
	AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error)
}

// NewStoreEventHooks returns EventHooks that emit store events for a given run
// and log emoji-rich console output via the provided logger.
// The logger controls which content fields are included in events:
//   - debug+: prompts, response text
//   - trace:  tool call inputs/outputs, tool call details
//
// ctx is captured by the returned hook closures: filesystem stores ignore
// it but cloud (Mongo) stores honor cancellation/timeout. The hook lifetime
// is bounded by the engine.Run call that constructed it.
func NewStoreEventHooks(ctx context.Context, emitter EventEmitter, runID string, logger *iterlog.Logger) EventHooks {
	return EventHooks{
		OnLLMPrompt: func(nodeID string, systemPrompt string, userMessage string) {
			data := map[string]interface{}{
				"system_prompt": iterlog.Truncate(systemPrompt, maxFieldSize),
				"user_message":  iterlog.Truncate(userMessage, maxFieldSize),
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventLLMPrompt,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			if userMessage != "" {
				logger.Logf(iterlog.LevelInfo, "💬", "Prompt [%s]: %s", nodeID, preview(userMessage, 300))
			}
			if systemPrompt != "" {
				logger.Logf(iterlog.LevelDebug, "📝", "System prompt [%s]: %s", nodeID, preview(systemPrompt, 500))
			}
		},

		OnLLMRequest: func(nodeID string, info LLMRequestInfo) {
			data := map[string]interface{}{
				"model":         info.Model,
				"message_count": info.MessageCount,
				"tool_count":    info.ToolCount,
			}
			if info.ReasoningEffort != "" {
				data["reasoning_effort"] = info.ReasoningEffort
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventLLMRequest,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			toolInfo := ""
			if info.ToolCount > 0 {
				toolInfo = fmt.Sprintf(", %d tools", info.ToolCount)
			}
			reasoningInfo := ""
			if info.ReasoningEffort != "" {
				reasoningInfo = fmt.Sprintf(", reasoning=%s", info.ReasoningEffort)
			}
			logger.Logf(iterlog.LevelInfo, "🤖", "LLM call [%s]: %s (%d msgs%s%s)",
				nodeID, info.Model, info.MessageCount, toolInfo, reasoningInfo)
		},

		// OnLLMResponse is intentionally nil: response data surfaces through
		// llm_step_finished events with richer per-step detail.

		OnLLMRetry: func(nodeID string, info RetryInfo) {
			data := map[string]interface{}{
				"attempt":  info.Attempt,
				"delay_ms": info.Delay.Milliseconds(),
			}
			if info.Error != nil {
				data["error"] = info.Error.Error()
			}
			if info.StatusCode != 0 {
				data["status_code"] = info.StatusCode
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventLLMRetry,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			errMsg := ""
			if info.Error != nil {
				errMsg = info.Error.Error()
			}
			logger.Warn("LLM retry [%s]: attempt %d, delay %dms: %s",
				nodeID, info.Attempt, info.Delay.Milliseconds(), errMsg)
		},

		OnLLMStepFinish: func(nodeID string, step LLMStepInfo) {
			data := map[string]interface{}{
				"step":          step.Number,
				"input_tokens":  step.InputTokens,
				"output_tokens": step.OutputTokens,
				"finish_reason": step.FinishReason,
				"tool_calls":    len(step.ToolCalls),
			}
			if step.CacheReadTokens > 0 {
				data["cache_read_tokens"] = step.CacheReadTokens
			}
			if step.CacheWriteTokens > 0 {
				data["cache_write_tokens"] = step.CacheWriteTokens
			}

			// Always include response text in persisted events.
			if step.Text != "" {
				data["response_text"] = iterlog.Truncate(step.Text, maxFieldSize)
			}

			// At trace, include tool call details.
			if logger.IsEnabled(iterlog.LevelTrace) && len(step.ToolCalls) > 0 {
				calls := make([]map[string]interface{}, len(step.ToolCalls))
				for i, tc := range step.ToolCalls {
					calls[i] = map[string]interface{}{
						"tool_name": tc.Name,
						"input":     iterlog.Truncate(string(tc.Input), maxFieldSize),
					}
				}
				data["tool_call_details"] = calls
			}

			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventLLMStepFinished,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			if step.Text != "" {
				logger.LogBlock(iterlog.LevelInfo, "💬",
					fmt.Sprintf("LLM response [%s] step %d:", nodeID, step.Number),
					iterlog.BlockPreview(step.Text, 2000))
			}
			if len(step.ToolCalls) > 0 {
				for _, tc := range step.ToolCalls {
					if detail := summarizeToolCallInput(tc.Name, tc.Input); detail != "" {
						logger.Logf(iterlog.LevelInfo, "🔧", "Tool call [%s]: %s %s", nodeID, tc.Name, detail)
					} else {
						logger.Logf(iterlog.LevelInfo, "🔧", "Tool call [%s]: %s", nodeID, tc.Name)
					}
					logger.Logf(iterlog.LevelDebug, "🔧", "  input: %s", preview(string(tc.Input), 400))
				}
			}
			if step.CacheReadTokens > 0 || step.CacheWriteTokens > 0 {
				logger.Logf(iterlog.LevelInfo, "📊", "Step %d [%s]: %d in / %d out tokens (cache: %d read, %d write)",
					step.Number, nodeID, step.InputTokens, step.OutputTokens,
					step.CacheReadTokens, step.CacheWriteTokens)
			} else {
				logger.Logf(iterlog.LevelInfo, "📊", "Step %d [%s]: %d in / %d out tokens",
					step.Number, nodeID, step.InputTokens, step.OutputTokens)
			}
		},

		OnLLMCompacted: func(nodeID string, info LLMCompactInfo) {
			data := map[string]interface{}{
				"before_messages":       info.BeforeMessages,
				"after_messages":        info.AfterMessages,
				"removed_message_count": info.RemovedMessageCount,
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventLLMCompacted,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			logger.Logf(iterlog.LevelInfo, "📦", "Compacted [%s]: %d → %d msgs (%d removed)",
				nodeID, info.BeforeMessages, info.AfterMessages, info.RemovedMessageCount)
		},

		OnToolCall: func(nodeID string, info LLMToolCallInfo) {
			data := map[string]interface{}{
				"tool":        info.ToolName,
				"input_size":  info.InputSize,
				"duration_ms": info.Duration.Milliseconds(),
			}

			evtType := store.EventToolCalled
			if info.Error != nil {
				evtType = store.EventToolError
				data["error"] = info.Error.Error()
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   evtType,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			// Console output.
			if info.Error != nil {
				logger.Error("Tool error [%s]: %s — %v (%dms)",
					nodeID, info.ToolName, info.Error, info.Duration.Milliseconds())
			} else {
				logger.Logf(iterlog.LevelInfo, "🔧", "Tool done [%s]: %s (%dms)",
					nodeID, info.ToolName, info.Duration.Milliseconds())
			}
		},

		OnDelegateStarted: func(nodeID string, backendName string) {
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventDelegateStarted,
				RunID:  runID,
				NodeID: nodeID,
				Data:   map[string]interface{}{"backend": backendName},
			})
			logger.Logf(iterlog.LevelInfo, "🚀", "Delegation started [%s]: backend=%s", nodeID, backendName)
		},

		OnDelegateFinished: func(nodeID string, info DelegateInfo) {
			data := map[string]interface{}{
				"backend":              info.BackendName,
				"duration_ms":          info.Duration.Milliseconds(),
				"tokens":               info.Tokens,
				"exit_code":            info.ExitCode,
				"raw_output_len":       info.RawOutputLen,
				"parse_fallback":       info.ParseFallback,
				"formatting_pass_used": info.FormattingPassUsed,
			}
			if logger.IsEnabled(iterlog.LevelTrace) && info.Stderr != "" {
				data["stderr"] = iterlog.Truncate(info.Stderr, maxFieldSize)
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventDelegateFinished,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			logger.Logf(iterlog.LevelInfo, "✅", "Delegation finished [%s]: %s (%dms, %d tokens)",
				nodeID, info.BackendName, info.Duration.Milliseconds(), info.Tokens)
			if info.FormattingPassUsed {
				logger.Logf(iterlog.LevelDebug, "📐", "Delegation [%s]: two-pass execution used for structured output", nodeID)
			} else if info.ParseFallback {
				logger.Warn("Delegation [%s]: structured output parsing fell back to text wrapper", nodeID)
			}
			if info.Stderr != "" {
				logger.Logf(iterlog.LevelDebug, "⚠️", "Delegation stderr [%s]: %s", nodeID, preview(info.Stderr, 500))
			}
		},

		OnDelegateError: func(nodeID string, info DelegateInfo) {
			data := map[string]interface{}{
				"backend":     info.BackendName,
				"duration_ms": info.Duration.Milliseconds(),
				"tokens":      info.Tokens,
				"exit_code":   info.ExitCode,
			}
			if info.Error != nil {
				data["error"] = info.Error.Error()
			}
			if logger.IsEnabled(iterlog.LevelTrace) && info.Stderr != "" {
				data["stderr"] = iterlog.Truncate(info.Stderr, maxFieldSize)
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventDelegateError,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			errMsg := ""
			if info.Error != nil {
				errMsg = info.Error.Error()
			}
			logger.Error("Delegation failed [%s]: %s — %s", nodeID, info.BackendName, errMsg)
		},

		OnDelegateRetry: func(nodeID string, info DelegateInfo) {
			data := map[string]interface{}{
				"backend":  info.BackendName,
				"attempt":  info.Attempt,
				"delay_ms": info.Delay.Milliseconds(),
			}
			if info.Error != nil {
				data["error"] = info.Error.Error()
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   store.EventDelegateRetry,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			errMsg := ""
			if info.Error != nil {
				errMsg = info.Error.Error()
			}
			logger.Warn("Delegation retry [%s]: %s attempt %d, delay %dms: %s",
				nodeID, info.BackendName, info.Attempt, info.Delay.Milliseconds(), errMsg)
		},

		// OnToolNodeResult handles direct tool nodes with full I/O content.
		OnToolNodeResult: func(nodeID string, toolName string, input []byte, output string, elapsed time.Duration, err error) {
			data := map[string]interface{}{
				"tool":        toolName,
				"input_size":  len(input),
				"duration_ms": elapsed.Milliseconds(),
			}

			if logger.IsEnabled(iterlog.LevelTrace) {
				if len(input) > 0 {
					data["input"] = iterlog.Truncate(string(input), maxFieldSize)
				}
				if output != "" {
					data["output"] = iterlog.Truncate(output, maxFieldSize)
				}
			}

			evtType := store.EventToolCalled
			if err != nil {
				evtType = store.EventToolError
				data["error"] = err.Error()
			}
			_, _ = emitter.AppendEvent(ctx, runID, store.Event{
				Type:   evtType,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})

			if err != nil {
				logger.Error("Tool error [%s]: %s — %v (%dms)",
					nodeID, toolName, err, elapsed.Milliseconds())
			} else {
				logger.Logf(iterlog.LevelInfo, "🔧", "Tool result [%s]: %s → %s (%dms)",
					nodeID, toolName, humanSize(len(output)), elapsed.Milliseconds())
				if output != "" {
					logger.LogBlock(iterlog.LevelDebug, "🔬",
						fmt.Sprintf("Tool output [%s/%s]:", nodeID, toolName),
						iterlog.BlockPreview(output, 1500))
				}
			}
		},
	}
}

// preview returns the first n bytes of s, adding "..." if truncated.
// It also replaces newlines with spaces for single-line display.
func preview(s string, n int) string {
	if len(s) == 0 {
		return "(empty)"
	}
	truncated := len(s) > n
	if truncated {
		s = s[:n]
	}
	// Replace newlines for compact display.
	cleaned := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			cleaned = append(cleaned, ' ')
		} else {
			cleaned = append(cleaned, s[i])
		}
	}
	if truncated {
		return string(cleaned) + "..."
	}
	return string(cleaned)
}

// toolDetailKeys maps a tool name to the input fields whose value best
// identifies the call at a glance (file path, command, pattern, url).
// Order matters: the first non-empty string match wins. Tools not in
// the map fall back to logging just the name.
var toolDetailKeys = map[string][]string{
	"read_file":     {"path", "file_path"},
	"file_edit":     {"path", "file_path"},
	"write_file":    {"path", "file_path"},
	"notebook_edit": {"path", "file_path", "notebook_path"},
	"bash":          {"command"},
	"grep":          {"pattern"},
	"glob":          {"pattern"},
	"web_fetch":     {"url"},
	"web_search":    {"query"},
	"skill":         {"skill", "name"},
	"agent":         {"description"},
	"ask_user":      {"question"},
	"task_create":   {"description"},
	"tool_search":   {"query"},
	"sleep":         {"seconds", "duration"},
}

// summarizeToolCallInput returns a one-line, truncated detail string
// describing the tool's primary argument for inclusion in the per-step
// log line. Returns "" when the tool name is unknown or the input has
// no usable primary field — log call falls back to bare tool name.
//
// This brings the claw-side log output to parity with the claude_code
// delegate, which already emits "🔧 Read /path/to/file" lines.
func summarizeToolCallInput(toolName string, input json.RawMessage) string {
	keys := toolDetailKeys[toolName]
	if len(keys) == 0 || len(input) == 0 {
		return ""
	}
	var raw map[string]any
	if err := json.Unmarshal(input, &raw); err != nil {
		return ""
	}
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		switch s := v.(type) {
		case string:
			if s != "" {
				return preview(s, 200)
			}
		case float64:
			return preview(fmt.Sprintf("%g", s), 200)
		case bool:
			return preview(fmt.Sprintf("%v", s), 200)
		}
	}
	return ""
}

// humanSize formats a byte count as a human-readable string.
func humanSize(bytes int) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%d B", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	}
}
