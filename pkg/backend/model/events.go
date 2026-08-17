package model

import (
	"encoding/json"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
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
	// Iteration is the 0-based loop iteration the node is executing in,
	// threaded through by applyHooks. Drives the [node#iter/claw] console
	// tag so the studio's per-(node,iteration) Logs tab can associate the
	// line; without it every claw line stuck at #0 (the hook closures
	// captured a base context, not the live per-iteration one).
	Iteration int
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
	// ReasoningTokens is an approximate count of extended-thinking tokens
	// (re-encoded from the thinking text); ThinkingMs is the wall-clock spent
	// in thinking blocks. Both are 0 when the step produced no thinking.
	ReasoningTokens int
	ThinkingMs      int
	// Thinking is the extended-thinking text for this step (empty when the
	// step produced no thinking).
	Thinking string
	// Iteration is the 0-based loop iteration (see LLMRequestInfo.Iteration).
	Iteration int
}

// ToolCallEntry describes a single tool call within a step.
type ToolCallEntry struct {
	Name  string
	Input json.RawMessage
}

// AssistantTextInfo carries one chunk of assistant narration (mid-turn
// prose), passed to the OnAssistantText hook. Fired per assistant text
// block for claude_code and per tool-bearing step for claw.
type AssistantTextInfo struct {
	Text string
	// Iteration is the 0-based loop iteration (see LLMRequestInfo.Iteration).
	Iteration int
}

// UsageCapInfo describes a usage-cap crossing, passed to the OnUsageCap
// hook. Stopped distinguishes the two postures: a soft cap fires this event
// and lets the run finish, a hard cap fires it and ends the run.
type UsageCapInfo struct {
	Window   string
	Family   string
	Percent  float64
	Cap      float64
	Mode     string
	Stopped  bool
	ResetsAt time.Time
}

// LLMToolCallInfo describes a tool call execution, passed to the OnToolCall hook.
type LLMToolCallInfo struct {
	ToolName  string
	InputSize int
	// ToolUseID correlates this completion event with the matching
	// tool_started event. The studio merges the two by this id so the
	// post-execution event (duration + error) lines up with the
	// pre-execution event (structured input payload for whitelisted
	// tools). Empty when the path can't provide one — the merge then
	// falls through and the card renders with whatever data the
	// tool_called event alone carries.
	ToolUseID string
	// Output is the string the tool returned to the LLM. The hooks layer
	// truncates and persists it on the tool_called event so the studio's
	// per-node Tools tab can render in+out the way Claude Code does.
	// Empty when the backend can't surface a result (e.g. tool blocked
	// pre-execution by a lifecycle hook, unknown tool dispatch).
	Output   string
	Duration time.Duration
	Error    error
}

// LLMToolStartedInfo describes a tool call about to execute, passed to the
// OnToolStarted hook. Emitted immediately before the tool runs so observers
// can render an in-flight indicator (e.g. the studio's "Running <tool>"
// footer) while waiting for completion.
type LLMToolStartedInfo struct {
	ToolName  string
	InputSize int
	// ToolUseID correlates start↔completion when the same node fires
	// multiple parallel tool calls (e.g. claude_code's assistant message
	// with several tool_use blocks). Empty when the path can't provide
	// one (direct tool nodes, claw single tool loop).
	ToolUseID string
	// Input is the raw JSON arguments the LLM produced for this call.
	// May be empty when the backend cannot surface it. The hooks layer
	// persists it (truncated) on the tool_started event so the studio's
	// per-node Tools tab can render parameters (command, file_path,
	// todos, …) for every tool — symmetric with the post-execution
	// `output` field on LLMToolCallInfo.
	Input json.RawMessage
	// Iteration is the 0-based loop iteration of the node firing this
	// tool call (see LLMRequestInfo.Iteration). Populated so the plan-
	// capture hook can tag each TodoWrite snapshot with the pass it came
	// from; 0 for direct tool nodes / paths that don't carry one.
	Iteration int
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
	// Iteration is the 0-based loop iteration (see LLMRequestInfo.Iteration).
	Iteration int
}

// LLMTurnCaptureInfo describes one captured tool-loop turn, passed to
// the OnLLMTurnCapture hook. Carries the per-step metadata
// (finish_reason, tool calls, usage) plus the JSON-encoded
// conversation snapshot the runtime persists as a
// store.TurnCheckpoint.MessagesRef for the Fork API rehydration path.
//
// Conversation is the JSON-encoded []api.Message slice taken at the
// natural end-of-iteration boundary — the very state the next LLM
// call would observe if the loop continued. Treat as immutable.
type LLMTurnCaptureInfo struct {
	Step             int
	Text             string
	ToolCalls        []ToolCallEntry
	FinishReason     string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	// Iteration is the 0-based loop iteration (see LLMRequestInfo.Iteration).
	// Threaded so TurnCheckpoint.LoopIter is correct — it previously came
	// from a stale captured context and was always 0.
	Iteration int
	// Backend names the executor that produced this turn ("claw" or
	// "claude_code"). Used by the store-backed hook to set the
	// matching TurnCheckpoint.Backend field. Empty defaults to "claw"
	// for back-compat with paths that don't fill it explicitly.
	Backend string
	// SessionID is the claude_code CLI session id captured at the end
	// of a delegate call. Empty for claw (no session-id concept) and
	// for claude_code paths that didn't surface one. The Fork API
	// passes it to `claude --resume <id> --fork-session` for the
	// claude_code rehydration path.
	SessionID string
	// conversation holds the unmarshalled message slice the runtime
	// captures for the fork rehydration path. Kept unexported so
	// observers must call MarshalConversation to materialise the JSON
	// bytes — this defers the O(N) marshal cost to consumers that
	// actually persist the snapshot (the store-backed hook), keeping
	// observability-only consumers cheap. claude_code leaves it nil
	// (the CLI owns its session jsonl).
	conversation []api.Message
}

// MarshalConversation encodes the captured message slice as JSON, on
// demand. Returns nil when the turn carried no conversation (the
// claude_code path, or an SDK that didn't surface one). Errors are
// folded into a nil return so the store-backed hook treats them as
// "snapshot unavailable" rather than aborting the run.
func (i LLMTurnCaptureInfo) MarshalConversation() json.RawMessage {
	if len(i.conversation) == 0 {
		return nil
	}
	b, err := json.Marshal(i.conversation)
	if err != nil {
		return nil
	}
	return b
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
		ReasoningTokens:  step.Usage.ReasoningTokens,
		ThinkingMs:       step.Usage.ThinkingMs,
		Thinking:         step.Thinking,
	}
}

func toLLMToolCallInfo(info ToolCallInfo) LLMToolCallInfo {
	return LLMToolCallInfo{
		ToolName:  info.ToolName,
		InputSize: info.InputSize,
		ToolUseID: info.ToolUseID,
		Output:    info.Output,
		Duration:  info.Duration,
		Error:     info.Error,
	}
}

func toLLMToolStartedInfo(info ToolCallInfo) LLMToolStartedInfo {
	return LLMToolStartedInfo{
		ToolName:  info.ToolName,
		InputSize: info.InputSize,
		ToolUseID: info.ToolUseID,
		Input:     info.Input,
	}
}

func toLLMCompactInfo(info CompactInfo) LLMCompactInfo {
	return LLMCompactInfo{
		BeforeMessages:      info.BeforeMessages,
		AfterMessages:       info.AfterMessages,
		RemovedMessageCount: info.RemovedMessageCount,
	}
}

// toLLMTurnCaptureInfo converts the local TurnCaptureInfo into the
// iterion-owned LLMTurnCaptureInfo. The conversation snapshot is
// intentionally NOT marshalled here — the marshal is O(N) in
// conversation length and only the runtime hook that writes to a
// TurnStore actually needs the bytes. Callers that need them call
// MarshalConversation on the info; callers that don't (stores without a
// TurnStore, observability-only consumers) skip the cost.
func toLLMTurnCaptureInfo(info TurnCaptureInfo) LLMTurnCaptureInfo {
	calls := make([]ToolCallEntry, len(info.Result.ToolCalls))
	for i, tc := range info.Result.ToolCalls {
		calls[i] = ToolCallEntry{Name: tc.Name, Input: tc.Input}
	}
	return LLMTurnCaptureInfo{
		Step:             info.Step,
		Text:             info.Result.Text,
		ToolCalls:        calls,
		FinishReason:     string(info.Result.FinishReason),
		InputTokens:      info.Result.Usage.InputTokens,
		OutputTokens:     info.Result.Usage.OutputTokens,
		CacheReadTokens:  info.Result.Usage.CacheReadTokens,
		CacheWriteTokens: info.Result.Usage.CacheWriteTokens,
		conversation:     info.Conversation,
	}
}

// ---------------------------------------------------------------------------
// Hook wiring (shared between claw_backend.go and executor.go)
// ---------------------------------------------------------------------------

// applyHooks populates the GenerationOptions hook callbacks that bridge the
// generation engine's types (RequestInfo, ResponseInfo, StepResult, ToolCallInfo)
// to the iterion event types (LLMRequestInfo, etc.) and dispatch to the
// EventHooks callbacks. Only non-nil hooks are wired.
func applyHooks(nodeID string, iteration int, h EventHooks, opts *GenerationOptions) {
	if h.OnLLMRequest != nil {
		fn := h.OnLLMRequest
		opts.OnRequest = func(info RequestInfo) {
			li := toLLMRequestInfo(info)
			li.Iteration = iteration
			fn(nodeID, li)
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
			li := toLLMStepInfo(step)
			li.Iteration = iteration
			fn(nodeID, li)
		}
	}
	if h.OnLLMTurnCapture != nil {
		fn := h.OnLLMTurnCapture
		opts.OnTurnCapture = func(info TurnCaptureInfo) {
			li := toLLMTurnCaptureInfo(info)
			li.Iteration = iteration
			fn(nodeID, li)
		}
	}
	if h.OnToolStarted != nil {
		fn := h.OnToolStarted
		opts.OnToolStarted = func(info ToolCallInfo) {
			li := toLLMToolStartedInfo(info)
			li.Iteration = iteration
			fn(nodeID, li)
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
			li := toLLMCompactInfo(info)
			li.Iteration = iteration
			fn(nodeID, li)
		}
	}
	// Surface a reactive context-window force-compaction (the loop hit the
	// backend's real window and recovered) as an llm_retry, reusing the
	// same event the network-retry path emits.
	if h.OnLLMRetry != nil {
		fn := h.OnLLMRetry
		opts.OnContextCompactRetry = func(attempt int, err error, _, _ int) {
			fn(nodeID, RetryInfo{Attempt: attempt, Error: err})
		}
	}
}
