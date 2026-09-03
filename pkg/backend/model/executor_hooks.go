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
	BackendName string // e.g. "claude_code", "codex"
	// DeclaredModel is the workflow-declared `model:` (or the launch
	// override that replaced it) — what the node ASKED for. Empty when
	// the node left model unset (auto-detect / backend default).
	DeclaredModel string
	// EffectiveModel is the model the backend actually used, as
	// reported by the provider. Distinct from DeclaredModel when a
	// fallback, env override (ANTHROPIC_MODEL), or proxy rewrote it.
	// Empty when the backend does not report it.
	EffectiveModel string
	// ContextWindow is the effective model's context window in tokens.
	// Zero when unknown.
	ContextWindow int
	// MaxOutputTokens is the per-call output cap reported for the
	// effective model. Zero when unknown.
	MaxOutputTokens int
	// PeakInputTokens is the largest context load observed across the
	// session. Zero when unknown.
	PeakInputTokens    int
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
	// Skipped marks a node completed by an `action: skip` terminal route:
	// NOTHING served it. BackendName then names the LAST route that
	// actually EXECUTED and spent (chainOutcome.BackendName = lastBackend;
	// the node's requested backend only when no route executed at all, in
	// which case the spend is zero) — deliberately, because the runner's
	// cost accumulator keys its claw double-count exclusion on that name.
	// Consumers must not read it as "what served": recordServed is
	// suppressed and the event carries skipped:true.
	Skipped bool
}

// delegateInfoFromResult fills the result-derived fields of a DelegateInfo —
// duration, tokens, exit, stderr, raw length, parse/formatting flags, cost,
// plus the model/window the backend actually used. Callers pass BackendName
// explicitly (it varies: result.BackendName, a fallback, or the requested
// name) and set Error / Attempt / DeclaredModel afterward as the hook needs.
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
		EffectiveModel:     result.EffectiveModel,
		ContextWindow:      result.ContextWindow,
		MaxOutputTokens:    result.MaxOutputTokens,
		PeakInputTokens:    result.PeakInputTokens,
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
	// FromBackend / ToBackend are the backends on each side of the
	// fall-through. A legacy `provider:` chain never changes backend, so
	// both equal BackendName; a `fallbacks:` chain (ADR-087) varies them.
	// They are the ONLY honest answer to "what actually served this
	// node" once a chain can cross backends, which is why the emitted
	// event carries them rather than the requested name.
	FromBackend string
	ToBackend   string
	// Reason is the delegate.FallbackCategory the failure classified as
	// (usage_window / auth / unavailable / transient_exhausted /
	// unclassified). It is what makes a fall-through readable in a
	// report: "fell through because the forfait window shut" reads very
	// differently from "fell through because the credential is dead".
	Reason   string
	Attempts int   // retry attempts spent on the failed provider; 0 for a cooldown skip
	Err      error // the hard failure that triggered the fall-through
	// FallbackIndex is the zero-based destination stage for a launch-time
	// run fallback. Nil for authored and legacy provider chains.
	FallbackIndex *int
	// Cooldown is true when dispatch skipped an attempt using a refusal a
	// previous node already observed. CooldownUntil is that refusal's reset.
	Cooldown      bool
	CooldownUntil time.Time
	// ToSkip marks a fall-through INTO an `action: skip` terminal route:
	// nothing will serve the node — it completes with a zero-value output.
	// Without this flag the event would read as a bascule to ToBackend,
	// which for a skip is the backend that just failed.
	ToSkip bool
}

// SessionDegradedInfo describes a best-effort session that could not be
// resumed, passed to the OnSessionDegraded hook.
type SessionDegradedInfo struct {
	BackendName string // backend whose session failed to load
	SessionID   string // the id that failed to serve
	// Reason is the delegate.FallbackCategory the failure classified as.
	// Always "unclassified" today — the degrade fires on nothing else,
	// deliberately (a typed auth/usage-window/transient failure names a
	// cause the session had no part in).
	Reason string
	Err    error // the failure the dropped session is being blamed for
}

// MCPServerDegradedInfo describes an ambient MCP server dropped from a
// node's tool set because it failed to boot, passed to the
// OnMCPServerDegraded hook.
type MCPServerDegradedInfo struct {
	Server string // MCP server name that failed to boot
	Source string // where the server came from — "ambient" (repo .mcp.json / plugin catalog)
	Err    error  // the boot failure the dropped tools are blamed for
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
	// OnUsageCap fires when a reading of the provider's subscription
	// telemetry crosses an operator-configured cap (pkg/usagecap). Purely
	// observational — the executor has already decided whether to stop the
	// run by the time it fires — but it is the only place the timeline
	// learns that iterion stopped itself rather than being refused.
	OnUsageCap     func(nodeID string, info UsageCapInfo)
	OnLLMCompacted func(nodeID string, info LLMCompactInfo)
	OnToolStarted  func(nodeID string, info LLMToolStartedInfo)
	OnToolCall     func(nodeID string, info LLMToolCallInfo)
	// OnToolNodeResult is called for direct tool nodes (not LLM tool loops)
	// with full input/output content for detailed logging.
	OnToolNodeResult func(nodeID string, toolName string, input []byte, output string, elapsed time.Duration, err error)

	// Delegation lifecycle hooks.
	// OnDelegateStarted fires before the backend is invoked. The payload
	// carries the requested BackendName and DeclaredModel; EffectiveModel
	// is filled later on Finished/Error once the provider reports it.
	OnDelegateStarted  func(nodeID string, info DelegateInfo)
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

	// OnSessionDegraded fires when a best-effort session
	// (`session: inherit_if_available` / `persist`) failed to serve and
	// the call was re-run with the session dropped. Purely observational
	// — the node goes on to succeed — but it is the ONLY thing that puts
	// "this node ran without the conversation it asked for" in the run
	// record; the process log alone leaves a downstream gate blind.
	OnSessionDegraded func(nodeID string, info SessionDegradedInfo)

	// OnMCPServerDegraded fires when an AMBIENT MCP server (repo
	// .mcp.json / plugin catalog — never named by the node) fails to
	// boot and is dropped from the node's tool set. Purely observational
	// — the node runs on without that server's tools — but it is the
	// only thing that puts "this node ran without an inherited server"
	// in the run record.
	OnMCPServerDegraded func(nodeID string, info MCPServerDegradedInfo)

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
		OnLLMRequest:        chainCb2(a.OnLLMRequest, b.OnLLMRequest),
		OnLLMPrompt:         chainCb3(a.OnLLMPrompt, b.OnLLMPrompt),
		OnLLMResponse:       chainCb2(a.OnLLMResponse, b.OnLLMResponse),
		OnLLMRetry:          chainCb2(a.OnLLMRetry, b.OnLLMRetry),
		OnLLMStepFinish:     chainCb2(a.OnLLMStepFinish, b.OnLLMStepFinish),
		OnLLMTurnCapture:    chainCb2(a.OnLLMTurnCapture, b.OnLLMTurnCapture),
		OnLLMCompacted:      chainCb2(a.OnLLMCompacted, b.OnLLMCompacted),
		OnToolStarted:       chainCb2(a.OnToolStarted, b.OnToolStarted),
		OnToolCall:          chainCb2(a.OnToolCall, b.OnToolCall),
		OnToolNodeResult:    chainCb6(a.OnToolNodeResult, b.OnToolNodeResult),
		OnDelegateStarted:   chainCb2(a.OnDelegateStarted, b.OnDelegateStarted),
		OnDelegateFinished:  chainCb2(a.OnDelegateFinished, b.OnDelegateFinished),
		OnDelegateError:     chainCb2(a.OnDelegateError, b.OnDelegateError),
		OnDelegateRetry:     chainCb2(a.OnDelegateRetry, b.OnDelegateRetry),
		OnProviderFallback:  chainCb2(a.OnProviderFallback, b.OnProviderFallback),
		OnSessionDegraded:   chainCb2(a.OnSessionDegraded, b.OnSessionDegraded),
		OnMCPServerDegraded: chainCb2(a.OnMCPServerDegraded, b.OnMCPServerDegraded),
		OnNodeFinished:      chainCb2(a.OnNodeFinished, b.OnNodeFinished),
	}
}
