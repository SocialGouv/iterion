package model

import (
	"fmt"
	"time"

	iterlog "github.com/SocialGouv/iterion/log"
	"github.com/SocialGouv/iterion/store"
)

// maxFieldSize is the maximum byte length for a single content field in an event.
// Fields exceeding this limit are truncated to stay within the 10 MB event line limit.
const maxFieldSize = 1 << 20 // 1 MB

// EventEmitter is the subset of store.RunStore used by the event bridge.
type EventEmitter interface {
	AppendEvent(runID string, evt store.Event) (*store.Event, error)
}

// NewStoreEventHooks returns EventHooks that emit store events for a given run
// and log emoji-rich console output via the provided logger.
// The logger controls which content fields are included in events:
//   - debug+: prompts, response text
//   - trace:  tool call inputs/outputs, tool call details
func NewStoreEventHooks(emitter EventEmitter, runID string, logger *iterlog.Logger) EventHooks {
	return EventHooks{
		OnLLMPrompt: func(nodeID string, systemPrompt string, userMessage string) {
			data := map[string]interface{}{
				"system_prompt": iterlog.Truncate(systemPrompt, maxFieldSize),
				"user_message":  iterlog.Truncate(userMessage, maxFieldSize),
			}
			_, _ = emitter.AppendEvent(runID, store.Event{
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
			_, _ = emitter.AppendEvent(runID, store.Event{
				Type:   store.EventLLMRequest,
				RunID:  runID,
				NodeID: nodeID,
				Data: map[string]interface{}{
					"model":         info.Model,
					"message_count": info.MessageCount,
					"tool_count":    info.ToolCount,
				},
			})

			toolInfo := ""
			if info.ToolCount > 0 {
				toolInfo = fmt.Sprintf(", %d tools", info.ToolCount)
			}
			logger.Logf(iterlog.LevelInfo, "🤖", "LLM call [%s]: %s (%d msgs%s)",
				nodeID, info.Model, info.MessageCount, toolInfo)
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
			_, _ = emitter.AppendEvent(runID, store.Event{
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

			_, _ = emitter.AppendEvent(runID, store.Event{
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
					logger.Logf(iterlog.LevelInfo, "🔧", "Tool call [%s]: %s", nodeID, tc.Name)
					logger.Logf(iterlog.LevelDebug, "🔧", "  input: %s", preview(string(tc.Input), 400))
				}
			}
			logger.Logf(iterlog.LevelInfo, "📊", "Step %d [%s]: %d in / %d out tokens",
				step.Number, nodeID, step.InputTokens, step.OutputTokens)
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
			_, _ = emitter.AppendEvent(runID, store.Event{
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
			_, _ = emitter.AppendEvent(runID, store.Event{
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
			_, _ = emitter.AppendEvent(runID, store.Event{
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
			_, _ = emitter.AppendEvent(runID, store.Event{
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
			_, _ = emitter.AppendEvent(runID, store.Event{
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
			_, _ = emitter.AppendEvent(runID, store.Event{
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
