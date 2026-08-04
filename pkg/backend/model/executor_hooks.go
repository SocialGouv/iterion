package model

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// ---------------------------------------------------------------------------
// Observability hook payloads + composition
//
// Extracted from executor.go to keep that file focused on the executor
// flow (Execute / resolveBackend / executeBackend). Same package, so no
// API change.
// ---------------------------------------------------------------------------

// RetryInfo describes a retry attempt, passed to the OnLLMRetry hook.
type RetryInfo struct {
	Attempt    int           // 1-based retry number (attempt 1 = first retry)
	Error      error         // the error that triggered this retry
	StatusCode int           // HTTP status code if available
	Delay      time.Duration // backoff delay before this retry
}

// DelegateInfo describes a backend execution attempt, passed to backend hooks.
type DelegateInfo struct {
	BackendName        string        // e.g. "claude_code", "codex"
	Duration           time.Duration // subprocess wall-clock time
	Tokens             int           // estimated total tokens consumed
	ExitCode           int           // process exit code
	Stderr             string        // captured stderr output
	RawOutputLen       int           // byte length of raw stdout
	ParseFallback      bool          // true if structured output fell back to text wrapper
	FormattingPassUsed bool          // true if two-pass execution was used (tools + schema)
	Error              error         // non-nil for OnDelegateError
	Attempt            int           // 1-based retry number (for OnDelegateRetry)
	Delay              time.Duration // backoff delay (for OnDelegateRetry)
	// CostUSD is the delegation's LLM spend, read back from the `_cost_usd`
	// the backend annotated onto its output — the CLI's own figure when it
	// reports one, else the token estimate. Zero means the price table did
	// not know the model, NOT a free call: observers must skip rather than
	// record a $0 sample.
	CostUSD float64
}

// delegateInfoFromResult fills the result-derived fields of a DelegateInfo —
// the eight fields every post-call hook (OnDelegateFinished / OnDelegateError /
// the schema-fallback OnDelegateRetry) copies off delegate.Result. Callers pass
// BackendName explicitly (it varies: result.BackendName, a fallback, or the
// requested name) and set Error / Attempt afterward as the hook needs.
func delegateInfoFromResult(backendName string, result delegate.Result) DelegateInfo {
	return DelegateInfo{
		BackendName:        backendName,
		Duration:           result.Duration,
		Tokens:             result.Tokens,
		ExitCode:           result.ExitCode,
		Stderr:             result.Stderr,
		RawOutputLen:       result.RawOutputLen,
		ParseFallback:      result.ParseFallback,
		FormattingPassUsed: result.FormattingPassUsed,
		CostUSD:            cost.USDFromOutput(result.Output),
	}
}

// ProviderFallbackInfo describes a single fall-through within a node's
// provider fallback chain, passed to the OnProviderFallback hook.
type ProviderFallbackInfo struct {
	BackendName string // backend that ran the chain (e.g. "claude_code")
	From        string // provider hint that just failed ("" = auto)
	To          string // provider hint about to be tried next
	FromModel   string // effective model the failed provider ran (per-element override or node baseline)
	ToModel     string // effective model the next provider will run
	Attempts    int    // retry attempts spent on the failed provider
	Err         error  // the hard failure that triggered the fall-through
}

// EventHooks allows the executor to emit observability events back to the caller.
type EventHooks struct {
	OnLLMRequest    func(nodeID string, info LLMRequestInfo)
	OnLLMPrompt     func(nodeID string, systemPrompt string, userMessage string)
	OnLLMResponse   func(nodeID string, info LLMResponseInfo)
	OnLLMRetry      func(nodeID string, info RetryInfo)
	OnLLMStepFinish func(nodeID string, step LLMStepInfo)
	// OnAssistantText fires with each chunk of assistant narration —
	// mid-turn prose the conversation views render as the agent
	// "talking" while it works. claude_code fires it per streamed
	// assistant text block; claw derives it from tool-bearing steps in
	// the store hooks layer.
	OnAssistantText func(nodeID string, info AssistantTextInfo)
	// OnLLMTurnCapture fires once per claw tool-loop iteration after
	// the conversation has been augmented with this step's
	// assistant + tool_results blocks. The runtime persists the
	// snapshot as a store.TurnCheckpoint anchored at (run, node,
	// loop_iter, turn) — the load-bearing primitive for the
	// fork-from-here UX and the per-node timeline. Conversation is
	// an opaque []byte (JSON-encoded []api.Message) so EventHooks
	// stays neutral to the wire format.
	OnLLMTurnCapture func(nodeID string, info LLMTurnCaptureInfo)
	OnLLMCompacted   func(nodeID string, info LLMCompactInfo)
	OnToolStarted    func(nodeID string, info LLMToolStartedInfo)
	OnToolCall       func(nodeID string, info LLMToolCallInfo)
	// OnToolNodeResult is called for direct tool nodes (not LLM tool loops)
	// with full input/output content for detailed logging.
	OnToolNodeResult func(nodeID string, toolName string, input []byte, output string, elapsed time.Duration, err error)

	// Delegation lifecycle hooks.
	OnDelegateStarted  func(nodeID string, backendName string)
	OnDelegateFinished func(nodeID string, info DelegateInfo)
	OnDelegateError    func(nodeID string, info DelegateInfo)
	OnDelegateRetry    func(nodeID string, info DelegateInfo)
	// OnProviderFallback fires once each time a node's provider
	// fallback chain falls through from a failed provider to the next
	// one (see the DSL `provider: "a,b,c"` chain). It is purely
	// observational — the run continues transparently against the next
	// provider — and lets the studio / Prometheus exporter surface that
	// a credential route was exhausted without the run itself failing.
	OnProviderFallback func(nodeID string, info ProviderFallbackInfo)

	// OnNodeFinished fires after a node's executor returns successfully.
	// The output map carries iterion's conventional usage keys (`_tokens`,
	// `_cost_usd`, `_model`) so observers (e.g. the Prometheus exporter)
	// can attribute cost and tokens per-node without re-parsing the event
	// log.
	OnNodeFinished func(nodeID string, output map[string]any)
}

// chainCb2 composes two 2-argument callbacks: if either is nil, returns
// the non-nil one; otherwise returns a wrapper that calls a then b.
func chainCb2[A, B any](a, b func(A, B)) func(A, B) {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return func(x A, y B) { a(x, y); b(x, y) }
}

// chainCb3 is the 3-argument variant of chainCb2 (used by OnLLMPrompt).
func chainCb3[A, B, C any](a, b func(A, B, C)) func(A, B, C) {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return func(x A, y B, z C) { a(x, y, z); b(x, y, z) }
}

// chainCb6 is the 6-argument variant (used by OnToolNodeResult).
func chainCb6[A, B, C, D, E, F any](a, b func(A, B, C, D, E, F)) func(A, B, C, D, E, F) {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return func(p1 A, p2 B, p3 C, p4 D, p5 E, p6 F) {
		a(p1, p2, p3, p4, p5, p6)
		b(p1, p2, p3, p4, p5, p6)
	}
}

// ChainHooks composes two EventHooks so callbacks registered on either
// side run in order (a then b) for every event. Either side may leave
// any callback nil; the result keeps the non-nil one without an extra
// closure.
func ChainHooks(a, b EventHooks) EventHooks {
	return EventHooks{
		OnLLMRequest:       chainCb2(a.OnLLMRequest, b.OnLLMRequest),
		OnLLMPrompt:        chainCb3(a.OnLLMPrompt, b.OnLLMPrompt),
		OnLLMResponse:      chainCb2(a.OnLLMResponse, b.OnLLMResponse),
		OnLLMRetry:         chainCb2(a.OnLLMRetry, b.OnLLMRetry),
		OnLLMStepFinish:    chainCb2(a.OnLLMStepFinish, b.OnLLMStepFinish),
		OnLLMTurnCapture:   chainCb2(a.OnLLMTurnCapture, b.OnLLMTurnCapture),
		OnLLMCompacted:     chainCb2(a.OnLLMCompacted, b.OnLLMCompacted),
		OnToolStarted:      chainCb2(a.OnToolStarted, b.OnToolStarted),
		OnToolCall:         chainCb2(a.OnToolCall, b.OnToolCall),
		OnToolNodeResult:   chainCb6(a.OnToolNodeResult, b.OnToolNodeResult),
		OnDelegateStarted:  chainCb2(a.OnDelegateStarted, b.OnDelegateStarted),
		OnDelegateFinished: chainCb2(a.OnDelegateFinished, b.OnDelegateFinished),
		OnDelegateError:    chainCb2(a.OnDelegateError, b.OnDelegateError),
		OnDelegateRetry:    chainCb2(a.OnDelegateRetry, b.OnDelegateRetry),
		OnProviderFallback: chainCb2(a.OnProviderFallback, b.OnProviderFallback),
		OnNodeFinished:     chainCb2(a.OnNodeFinished, b.OnNodeFinished),
	}
}
