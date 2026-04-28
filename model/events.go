package model

import (
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------------------
// Iterion-owned event types (decoupled from SDK)
// ---------------------------------------------------------------------------

// LLMRequestInfo describes an LLM request, passed to the OnLLMRequest hook.
type LLMRequestInfo struct {
	Model        string
	MessageCount int
	ToolCount    int
	Timestamp    time.Time
}

// LLMResponseInfo describes an LLM response, passed to the OnLLMResponse hook.
type LLMResponseInfo struct {
	Latency      time.Duration
	InputTokens  int
	OutputTokens int
	FinishReason string
	Error        error
	StatusCode   int
}

// LLMStepInfo describes a single step in a multi-step LLM generation,
// passed to the OnLLMStepFinish hook.
type LLMStepInfo struct {
	Number       int
	Text         string
	ToolCalls    []ToolCallEntry
	FinishReason string
	InputTokens  int
	OutputTokens int
}

// ToolCallEntry describes a single tool call within a step.
type ToolCallEntry struct {
	Name  string
	Input json.RawMessage
}

// LLMToolCallInfo describes a tool call execution, passed to the OnToolCall hook.
type LLMToolCallInfo struct {
	ToolName  string
	InputSize int
	Duration  time.Duration
	Error     error
}

// ---------------------------------------------------------------------------
// Conversion functions (local model types → iterion event types)
// ---------------------------------------------------------------------------

func toLLMRequestInfo(info RequestInfo) LLMRequestInfo {
	return LLMRequestInfo{
		Model:        info.Model,
		MessageCount: info.MessageCount,
		ToolCount:    info.ToolCount,
		Timestamp:    info.Timestamp,
	}
}

func toLLMResponseInfo(info ResponseInfo) LLMResponseInfo {
	return LLMResponseInfo{
		Latency:      info.Latency,
		InputTokens:  info.Usage.InputTokens,
		OutputTokens: info.Usage.OutputTokens,
		FinishReason: string(info.FinishReason),
		Error:        info.Error,
		StatusCode:   info.StatusCode,
	}
}

func toLLMStepInfo(step StepResult) LLMStepInfo {
	calls := make([]ToolCallEntry, len(step.ToolCalls))
	for i, tc := range step.ToolCalls {
		calls[i] = ToolCallEntry{
			Name:  tc.Name,
			Input: tc.Input,
		}
	}
	return LLMStepInfo{
		Number:       step.Number,
		Text:         step.Text,
		ToolCalls:    calls,
		FinishReason: string(step.FinishReason),
		InputTokens:  step.Usage.InputTokens,
		OutputTokens: step.Usage.OutputTokens,
	}
}

func toLLMToolCallInfo(info ToolCallInfo) LLMToolCallInfo {
	return LLMToolCallInfo{
		ToolName:  info.ToolName,
		InputSize: info.InputSize,
		Duration:  info.Duration,
		Error:     info.Error,
	}
}

// ---------------------------------------------------------------------------
// Hook wiring (shared between claw_backend.go and executor.go)
// ---------------------------------------------------------------------------

// applyHooks populates the GenerationOptions hook callbacks that bridge the
// generation engine's types (RequestInfo, ResponseInfo, StepResult, ToolCallInfo)
// to the iterion event types (LLMRequestInfo, etc.) and dispatch to the
// EventHooks callbacks. Only non-nil hooks are wired.
func applyHooks(nodeID string, h EventHooks, opts *GenerationOptions) {
	if h.OnLLMRequest != nil {
		fn := h.OnLLMRequest
		opts.OnRequest = func(info RequestInfo) {
			fn(nodeID, toLLMRequestInfo(info))
		}
	}
	if h.OnLLMResponse != nil {
		fn := h.OnLLMResponse
		opts.OnResponse = func(info ResponseInfo) {
			fn(nodeID, toLLMResponseInfo(info))
		}
	}
	if h.OnLLMStepFinish != nil {
		fn := h.OnLLMStepFinish
		opts.OnStepFinish = func(step StepResult) {
			fn(nodeID, toLLMStepInfo(step))
		}
	}
	if h.OnToolCall != nil {
		fn := h.OnToolCall
		opts.OnToolCall = func(info ToolCallInfo) {
			fn(nodeID, toLLMToolCallInfo(info))
		}
	}
}
