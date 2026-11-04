package model

import (
	"context"
	"encoding/json"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// GenerationTool describes a tool available during direct generation.
// It replaces sdk.Tool for the native generation engine.
type GenerationTool struct {
	// Name is the tool identifier.
	Name string

	// Description describes what the tool does.
	Description string

	// InputSchema is the JSON schema for the tool's input parameters.
	InputSchema json.RawMessage

	// Execute runs the tool with the given JSON input and returns the result text.
	Execute func(ctx context.Context, input json.RawMessage) (string, error)
}

// GenerationOptions configures a direct generation call.
type GenerationOptions struct {
	// Model is the routing spec the client was resolved from
	// ("anthropic/claude-sonnet-4-6"). buildRequest strips its routing prefix
	// exactly once: pass the spec, never an id already stripped — a model id
	// holding slashes of its own ("meta-llama/…") would lose a segment.
	Model string

	// Provider names the slot the client was resolved from — the
	// servingProvider word the delegate events carry (a `provider/` prefix's
	// pin, a hint that names the slot). The step hooks copy it onto every
	// llm_step_finished so a per-step observation joins the credential plan
	// on the loop's own answer instead of re-deriving one from the spec.
	// Empty when the loop builder stamped none.
	Provider string

	// System is the system prompt (plain string form).
	System string

	// SystemBlocks, when non-empty, takes precedence over System. Providers
	// supporting block-form system content receive these blocks unchanged;
	// buildRequest also mirrors their ordered text into System for transports
	// that only support the plain string form. Blocks remain the only way to
	// attach `cache_control` markers for prompt caching.
	SystemBlocks []api.ContentBlock

	// Messages is the initial conversation history.
	Messages []api.Message

	// Tools available for the model to call.
	Tools []GenerationTool

	// MaxSteps is the maximum number of tool-loop iterations (default 10).
	MaxSteps int

	// ForceInitialToolUse compels the model to call a tool before it may
	// emit a final answer: while no tool has been called yet, the tool loop
	// pins tool_choice to "any" (mapped to the provider's required/any
	// form), reverting to auto once the first tool call lands. This is the
	// agentic-parity lever for backends/models that, under the default
	// "auto" tool_choice, skip provided tools and answer from priors — the
	// exact failure that let a claw+gpt-5.5 reviewer produce an ungrounded
	// verdict (0 tool calls) while claude_code read the files. No-op when
	// Tools is empty. Set by the claw backend for tool-equipped agent/judge
	// nodes; harmless for models that already use their tools.
	ForceInitialToolUse bool

	// SkipUserPromptSubmit suppresses the UserPromptSubmit hook fire for
	// this call. Set by the claw backend on the harness's OWN re-ask
	// passes (the tool-use nudge and the schema-recovery formatting pass):
	// the operator's prompt was already screened on the first pass, the
	// reminder text those passes append is not the operator's, and a
	// screening Block landing on a recovery pass would be swallowed by
	// its fall-through — firing there is all cost, no signal.
	SkipUserPromptSubmit bool

	// MaxTokens is the maximum tokens per response (default 8192).
	MaxTokens int

	// MaxTokensWindow, when > 0, marks MaxTokens as a default raised past
	// claw's own (a gateway model's catalog max output) and is the context
	// window it must fit in: buildRequest lowers it, never below the 8192
	// default, so the request's estimated prompt plus max_tokens stays
	// inside this window. 0 sends MaxTokens as it is — always the case for
	// a max_tokens the node set.
	MaxTokensWindow int

	// AnswerFromReasoning lets a structured turn that showed no text answer
	// through a JSON object ending its reasoning (harvestStructured) — the
	// shape of a gateway model that answers in reasoning_content. Set by the
	// claw backend for a node's own output only, never for a caller whose
	// output is an action (an intervention, a command to run, a file to
	// write): a reasoning can end on an object it merely quotes.
	AnswerFromReasoning bool

	// Temperature controls randomness (nil = provider default).
	Temperature *float64

	// ExplicitSchema is the JSON schema for structured output (GenerateObjectDirect).
	ExplicitSchema json.RawMessage

	// SchemaName is the name for the synthetic structured-output tool (default "structured_output").
	SchemaName string

	// ProviderOptions are provider-specific options (e.g., reasoning_effort).
	ProviderOptions map[string]any

	// MaterializeSecrets, when non-nil, swaps secret placeholders for
	// their real values in agent-emitted tool input immediately before
	// execution (Layer 1). The placeholder form is what hooks/events
	// persist, so the real secret never reaches the store.
	MaterializeSecrets func(string) string

	// UnmaterializeSecrets, when non-nil, turns the known secret values an
	// echoing tool's result quotes (a fetched URL, an MCP tool's report of
	// its input) back into their placeholders before the result enters the
	// conversation — claude_code's PostToolUse twin.
	UnmaterializeSecrets func(string) string

	// ScreenModelText, when non-nil, is the model boundary's pass
	// (delegate.Task.ScreenModelText), applied by buildRequest to every
	// system text the request carries. The prompts arrive already passed;
	// the blocks the backend composed for ITSELF do not — the ambient
	// instruction files it read under the task's work dir, the
	// workspace-memory index and autoload — because no Task field holds
	// them. nil is the identity: placeholders off, or the in-container
	// runner, which carries no guard.
	ScreenModelText func(string) string

	// NamesSecret, when non-nil, reports whether a tool call's input carries
	// the placeholder of a secret that resolves at some sink — the proxy's
	// on the wire included — so a workspace reader's output is turned back
	// to placeholders when its call carried one, whatever materialised it.
	NamesSecret func(string) bool

	// CompactThresholdRatio overrides the default compaction trigger as a
	// fraction of the model's context window. 0 falls back to the built-in
	// default (0.85). Values outside (0, 1] fall back to the default.
	CompactThresholdRatio float64

	// CompactPreserveRecent overrides the default count of recent messages
	// kept verbatim during compaction. 0 falls back to the built-in default
	// (4). Values < 0 fall back to the default.
	CompactPreserveRecent int

	// --- Hook callbacks ---

	// OnRequest is called before each StreamResponse call.
	OnRequest func(RequestInfo)

	// OnResponse is called after each StreamResponse aggregation completes.
	OnResponse func(ResponseInfo)

	// OnStepFinish is called after each tool-loop step completes.
	OnStepFinish func(StepResult)

	// OnTurnCapture is called once per loop iteration after the
	// conversation has been augmented with the step's assistant message
	// (and tool_results, when the step contained tool calls). The
	// snapshot is a defensive copy the callback may persist or hand off
	// to a goroutine. Used by the runtime to write per-turn
	// TurnCheckpoint artifacts that anchor the fork-from-here UX
	// without disturbing the live message slice. Nil disables turn
	// capture.
	OnTurnCapture func(turn TurnCaptureInfo)

	// OnToolStarted is called immediately before each tool executes,
	// once the PreToolUse lifecycle hook (if any) has allowed the call.
	// Mirrors OnToolCall but carries no duration/error.
	OnToolStarted func(ToolCallInfo)

	// OnToolCall is called after each tool execution.
	OnToolCall func(ToolCallInfo)

	// OnBeforeCompact fires only when in-loop compaction is about to
	// shrink the history. The callback may return a modified slice
	// (e.g. with an injected session-memory user turn) that feeds the
	// summariser. Returning nil is a no-op. A turn the callback ADDS
	// carries IsInjected (the vendored compactor keeps it out of the
	// real-user-turn count). messages is a deep copy of the history, so
	// the live history keeps the originals whatever the callback writes,
	// in place or not, and whatever it returns. The loop passes every
	// turn of the returned slice that is not one of the history's own,
	// unchanged — the hook's additions, marked or not, a turn it rewrote
	// in place, and an earlier compaction's continuation turn alike —
	// through ScreenModelText before the summariser reads it
	// (screenedInjector): each text, tool name and tool input, nested
	// tool_result blocks included, so the text a hook composes for the
	// model crosses the boundary like every other. The history the
	// callback hands back as it was is handed to the summariser as it is.
	OnBeforeCompact func(messages []api.Message) []api.Message

	// OnCompact is called once after each in-loop compaction round
	// that actually shrunk the message history. No-op compactions
	// (transcript too short) do not fire the callback.
	OnCompact func(CompactInfo)

	// OnContextCompactRetry fires when a model call was REJECTED for
	// exceeding the backend's real context window and the loop reacted by
	// force-compacting the history and retrying. afterMessages is the
	// post-compaction message count; targetTokens the budget it shrank to.
	// Lets the backend surface the recovery as an llm_retry event.
	OnContextCompactRetry func(attempt int, err error, afterMessages, targetTokens int)

	// Hooks, when non-nil, is consulted around tool execution and at
	// session end. PreToolUse fires before each Execute and may Block
	// (the tool returns a synthetic refusal). PostToolUse fires after
	// successful Execute, PostToolUseFailure fires on error. Stop
	// fires once when the generation loop exits (success or failure).
	Hooks *hooks.Runner

	// Inbox, when non-nil, plumbs the run's operator-message inbox
	// into the tool loop. After each tool-results turn the engine
	// calls Inbox.Consume (transitioning the previous round's
	// delivered messages to "consumed") and then Inbox.Drain
	// (returning new operator texts to append as a synthetic user
	// turn). Delivery is cooperative — between iterations, never
	// mid-stream — mirroring Claude Code CLI's queued-message
	// semantics. Nil disables the inbox plumbing.
	Inbox InboxHook

	// Permission, when enabled, is the tool-permission gate evaluated
	// before each tool executes (the anti-prompt-injection boundary).
	// Allow → execute; Deny → a synthetic refusal tool_result; Ask →
	// the loop aborts with a *delegate.ErrAskUser so the run pauses and
	// the human approves (claw's half of cross-backend parity with
	// claude_code's PreToolUse permission hook). Nil/disabled = no gate.
	Permission *permission.Policy
}

// screenModelText returns the model-boundary pass these options carry, the
// identity when they carry none. Stated once so every text the request
// builder hands a model reads the same nil contract.
func (o GenerationOptions) screenModelText() func(string) string {
	if o.ScreenModelText == nil {
		return func(s string) string { return s }
	}
	return o.ScreenModelText
}
