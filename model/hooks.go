package model

import (
	goai "github.com/zendev-sh/goai"

	"github.com/iterion-ai/iterion/store"
)

// EventEmitter is the subset of store.RunStore used by the event bridge.
type EventEmitter interface {
	AppendEvent(runID string, evt store.Event) error
}

// NewStoreEventHooks returns EventHooks that emit store events for a given run.
// This bridges the goai callback model to iterion's persisted event stream.
func NewStoreEventHooks(emitter EventEmitter, runID string) EventHooks {
	return EventHooks{
		OnLLMRequest: func(nodeID string, info goai.RequestInfo) {
			_ = emitter.AppendEvent(runID, store.Event{
				Type:   store.EventLLMRequest,
				RunID:  runID,
				NodeID: nodeID,
				Data: map[string]interface{}{
					"model":         info.Model,
					"message_count": info.MessageCount,
					"tool_count":    info.ToolCount,
				},
			})
		},

		// OnLLMResponse is intentionally nil: the V4 event model does not
		// define a separate llm_response event. Response-level data
		// (latency, usage, finish reason) surfaces through llm_step_finished.

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
			_ = emitter.AppendEvent(runID, store.Event{
				Type:   store.EventLLMRetry,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})
		},

		OnLLMStepFinish: func(nodeID string, step goai.StepResult) {
			data := map[string]interface{}{
				"step":          step.Number,
				"input_tokens":  step.Usage.InputTokens,
				"output_tokens": step.Usage.OutputTokens,
				"finish_reason": string(step.FinishReason),
				"tool_calls":    len(step.ToolCalls),
			}
			_ = emitter.AppendEvent(runID, store.Event{
				Type:   store.EventLLMStepFinished,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})
		},

		OnToolCall: func(nodeID string, info goai.ToolCallInfo) {
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
			_ = emitter.AppendEvent(runID, store.Event{
				Type:   evtType,
				RunID:  runID,
				NodeID: nodeID,
				Data:   data,
			})
		},
	}
}
