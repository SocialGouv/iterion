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
	Model           string
	MessageCount    int
	ToolCount       int
	ReasoningEffort string
	Timestamp       time.Time
}

// LLMResponseInfo describes an LLM response, passed to the OnLLMResponse hook.
type LLMResponseInfo struct {
	Latency          time.Duration
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	FinishReason     string
	Error            error
	StatusCode       int
}

// LLMStepInfo describes a single step in a multi-step LLM generation,
// passed to the OnLLMStepFinish hook.
type LLMStepInfo struct {
	Number           int
	Text             string
	ToolCalls        []ToolCallEntry
	FinishReason     string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
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

// LLMCompactInfo describes a mid-tool-loop compaction round, passed to the
// OnLLMCompacted hook. Emitted when claw's pure-function compactor shrinks
// the running message history before the next StreamResponse call so
// long agentic loops on small-context models do not silently hit
// context_length_exceeded.
type LLMCompactInfo struct {
	BeforeMessages      int
	AfterMessages       int
	RemovedMessageCount int
}

// ---------------------------------------------------------------------------
// Conversion functions (local model types → iterion event types)
// ---------------------------------------------------------------------------

func toLLMRequestInfo(info RequestInfo) LLMRequestInfo {
	return LLMRequestInfo{
		Model:           info.Model,
		MessageCount:    info.MessageCount,
		ToolCount:       info.ToolCount,
		ReasoningEffort: info.ReasoningEffort,
		Timestamp:       info.Timestamp,
	}
}

func toLLMResponseInfo(info ResponseInfo) LLMResponseInfo {
	return LLMResponseInfo{
		Latency:          info.Latency,
		InputTokens:      info.Usage.InputTokens,
		OutputTokens:     info.Usage.OutputTokens,
		CacheReadTokens:  info.Usage.CacheReadTokens,
		CacheWriteTokens: info.Usage.CacheWriteTokens,
		FinishReason:     string(info.FinishReason),
		Error:            info.Error,
		StatusCode:       info.StatusCode,
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
		Number:           step.Number,
		Text:             step.Text,
		ToolCalls:        calls,
		FinishReason:     string(step.FinishReason),
		InputTokens:      step.Usage.InputTokens,
		OutputTokens:     step.Usage.OutputTokens,
		CacheReadTokens:  step.Usage.CacheReadTokens,
		CacheWriteTokens: step.Usage.CacheWriteTokens,
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

func toLLMCompactInfo(info CompactInfo) LLMCompactInfo {
	return LLMCompactInfo{
		BeforeMessages:      info.BeforeMessages,
		AfterMessages:       info.AfterMessages,
		RemovedMessageCount: info.RemovedMessageCount,
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
	if h.OnLLMCompacted != nil {
		fn := h.OnLLMCompacted
		opts.OnCompact = func(info CompactInfo) {
			fn(nodeID, toLLMCompactInfo(info))
		}
	}
}
