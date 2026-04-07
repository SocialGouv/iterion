package model

import (
	"encoding/json"
	"time"

	goai "github.com/zendev-sh/goai"
)

// ---------------------------------------------------------------------------
// Iterion-owned event types (decoupled from goai)
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
// Conversion functions (goai → iterion types)
// ---------------------------------------------------------------------------

func fromGoaiRequestInfo(info goai.RequestInfo) LLMRequestInfo {
	return LLMRequestInfo{
		Model:        info.Model,
		MessageCount: info.MessageCount,
		ToolCount:    info.ToolCount,
		Timestamp:    info.Timestamp,
	}
}

func fromGoaiResponseInfo(info goai.ResponseInfo) LLMResponseInfo {
	return LLMResponseInfo{
		Latency:      info.Latency,
		InputTokens:  info.Usage.InputTokens,
		OutputTokens: info.Usage.OutputTokens,
		FinishReason: string(info.FinishReason),
		Error:        info.Error,
		StatusCode:   info.StatusCode,
	}
}

func fromGoaiStepResult(step goai.StepResult) LLMStepInfo {
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

func fromGoaiToolCallInfo(info goai.ToolCallInfo) LLMToolCallInfo {
	return LLMToolCallInfo{
		ToolName:  info.ToolName,
		InputSize: info.InputSize,
		Duration:  info.Duration,
		Error:     info.Error,
	}
}
