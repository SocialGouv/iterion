// Package delegate provides the Backend interface and types for executing
// agent/judge nodes via pluggable backends (CLI agents like claude-code/codex,
// or API-based backends like claw).
//
// When a node has `backend: "claude_code"`, the executor invokes the named
// Backend which handles execution (subprocess, API call, etc.).
package delegate

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Backend name constants used for registration and dispatch.
const (
	BackendClaw       = "claw"
	BackendClaudeCode = "claude_code"
	BackendCodex      = "codex"
)

// interactionSystemInstruction is appended to the system prompt when
// InteractionEnabled is true, instructing the delegate to signal user
// input needs via reserved output fields.
const interactionSystemInstruction = "\n\n[INTERACTION PROTOCOL]\n" +
	"If at any point you need input, clarification, or approval from a human user " +
	"to proceed with your task, you MUST include these fields in your JSON output:\n" +
	"  \"_needs_interaction\": true,\n" +
	"  \"_interaction_questions\": {\"question_key\": \"your question text\"}\n" +
	"Include as many question keys as needed. If you do NOT need human input, " +
	"do not include these fields and complete your task normally."

// Backend is the interface for delegation execution. Each backend wraps
// a CLI agent (e.g. claude, codex) and handles prompt delivery, tool
// forwarding, and output collection.
type Backend interface {
	// Execute runs the CLI agent with the given task and returns structured output.
	Execute(ctx context.Context, task Task) (Result, error)
}

// ContentBlock is a backend-agnostic representation of a single
// content block in a multimodal user message. Mirrors the relevant
// fields of api.ContentBlock without leaking the claw-code-go
// dependency to the rest of the codebase.
type ContentBlock struct {
	// Type is one of "text" or "image".
	Type string
	// Text carries the textual content for Type=="text" blocks.
	Text string
	// MediaType is the MIME type for image blocks (e.g. "image/png").
	MediaType string
	// Data is the base64-encoded payload for image blocks. Mutually
	// exclusive with URL — a backend should populate exactly one.
	Data string
	// URL is the direct image URL (when the backend prefers a URL
	// source over inline base64). Mutually exclusive with Data.
	URL string
	// Path is the host filesystem path to the image, retained as a
	// fallback for CLI-based backends that load via the read_image
	// tool when neither Data nor a working URL is available.
	Path string
	// Name carries the workflow-declared attachment name for
	// observability and prompt-fallback annotation.
	Name string
}

// ToolDef is a fully resolved tool definition for backends that execute tools
// internally (e.g. claw). CLI-based backends use AllowedTools (string names) instead.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Execute     func(ctx context.Context, input json.RawMessage) (string, error)
}

// MemorySpec opts the node into the iterion workspace memory
// tree (under ~/.iterion/projects/<encoded>/memory/<Scope>/).
// Honored by backends that maintain their own session history (claw).
type MemorySpec struct {
	Scope            string
	Autoload         []string
	Read             bool
	Write            bool
	PreCompactInject bool
	// ProjectRoot, when true, re-roots the scope under the run's
	// `RepoRoot` (passed alongside via Task.RepoRoot) instead of the
	// per-run workDir. Enables cross-worktree shared scopes such as
	// the dispatcher's `findings/` channel where bot runs from
	// `<repo>/.iterion/dispatcher/workspaces/<id>` worktrees write
	// to the same tree a whats-next run at the repo root reads.
	ProjectRoot bool
}

// Task describes the work to execute on a backend.
type Task struct {
	// NodeID is the IR node identifier, used for observability hooks.
	NodeID string

	// Iteration is the 0-based loop iteration counter for this
	// execution. Aligned with the loop_iteration field exposed in
	// events / ExecutionState. Zero for nodes outside any loop.
	// Backends use it to tag log lines as [NodeID#iter/...] so the
	// studio can filter run.log per (node, iteration).
	Iteration int

	// SystemPrompt is the fully resolved system prompt text.
	SystemPrompt string

	// UserPrompt is the fully resolved user message text.
	UserPrompt string

	// UserContent, when non-empty, replaces UserPrompt for backends
	// that support multimodal input (claw). The first text block is
	// expected to carry the resolved prompt; image blocks carry
	// multimodal attachments. CLI-based backends (claude_code, codex)
	// fall back to UserPrompt and rely on the read_image tool to
	// reach the bytes via Path.
	UserContent []ContentBlock

	// AllowedTools is the list of tool names the CLI agent may use.
	// Used by CLI-based backends; API-based backends use ToolDefs instead.
	AllowedTools []string

	// Capabilities are the host-side capability names granted to this node
	// (e.g. "board.create", "board.read"). Backends wire them through to
	// the internal MCP servers / in-process tools they expose: an unwanted
	// capability is not advertised, so the agent never sees it. Empty =
	// no capabilities granted.
	Capabilities []string

	// StoreDir is the absolute path to the dispatcher store root used by
	// capability-gated tools (currently: board operations). Backends pass
	// this to the __mcp-board subcommand via ITERION_STORE_DIR. Empty
	// means "fall back to the cwd default"; backends should set this
	// explicitly whenever they want a specific store binding.
	StoreDir string

	// BoardHTTPEndpoint is the URL of the iterion-host board MCP HTTP
	// endpoint, used for sandboxed runs that can't reach the host
	// `iterion __mcp-board` subprocess via stdio. When non-empty AND the
	// task is sandboxed AND has board capabilities, backends register an
	// HTTP MCP server pointing here, with BoardRunToken sent as the
	// X-Iterion-Run header. Empty disables the HTTP path (stdio path
	// still works for non-sandboxed runs).
	BoardHTTPEndpoint string

	// BoardRunToken is the ephemeral token registered with the iterion
	// server's BoardMCPTokens registry for this run. The runtime
	// generates it, registers grants, and revokes on run completion.
	BoardRunToken string

	// ToolDefs provides full tool definitions for backends that manage tool
	// loops internally (e.g. claw). CLI-based backends ignore this field.
	ToolDefs []ToolDef

	// OutputSchema is the JSON Schema for the expected structured output.
	// Nil means free-form text output.
	OutputSchema json.RawMessage

	// Model is the resolved model spec (e.g. "anthropic/claude-sonnet-4-6").
	// Required for API-based backends; ignored by CLI-based backends.
	Model string

	// HasTools indicates whether the node has tools, enabling backends to
	// choose between structured-output and text-with-tools generation strategies.
	HasTools bool

	// ToolMaxSteps is the maximum number of tool-use iterations (0 = default).
	ToolMaxSteps int

	// MaxTokens caps the LLM response length per call. Honored by API-based
	// backends (claw); CLI-based backends (claude_code, codex) ignore it.
	// Zero means "use the backend default" (typically 8192).
	MaxTokens int

	// WorkDir is the working directory for the CLI subprocess.
	WorkDir string

	// BaseDir is the allowed base directory for WorkDir validation.
	// If set, WorkDir must resolve to a path within BaseDir.
	BaseDir string

	// RepoRoot is the source-of-truth repository root for this run
	// (the directory persisted on the run record). When the runtime
	// uses a worktree (`worktree: auto`) or the dispatcher runs the
	// bot in a per-issue workspace, WorkDir points at the worktree
	// while RepoRoot still points at the operator's main checkout.
	// Memory specs that set `project_root: true` resolve their scope
	// under RepoRoot's encoded key so the resulting tree is shared
	// across all runs of the same project regardless of which
	// worktree they execute in.
	RepoRoot string

	// ReasoningEffort is the reasoning effort level.
	// Valid values: "low", "medium", "high", "xhigh", "max".
	ReasoningEffort string

	// CursorFragments are resolved prompt-engineering cursor fragments
	// to append to the system prompt under a "## Calibration" section.
	// Each entry is one cursor activation, pre-formatted as
	// "**<CursorName>:** <fragment>". The runtime resolves enum/numeric
	// invocations against the workflow's cursor declarations and feeds
	// the sorted, ready-to-render list here. Empty slice means "no
	// cursors active" (either none declared, none invoked, or
	// `enabled: false` on the cursors block) — backends should skip
	// the calibration section entirely. See docs/cursors.md.
	CursorFragments []string

	// CompactThresholdRatio is the resolved compaction trigger as a
	// fraction of the model's context window (0 = use backend default).
	// Backends that maintain their own session history (claw) honor this;
	// CLI-based backends ignore it (claude_code does its own compaction).
	CompactThresholdRatio float64

	// CompactPreserveRecent is the number of recent messages kept verbatim
	// during compaction (0 = use backend default of 4).
	CompactPreserveRecent int

	// Memory opts the node into the iterion workspace memory tree.
	// Honored only by backends that maintain their own session history (claw).
	Memory *MemorySpec

	// SessionID is an optional session ID to resume (empty = fresh session).
	SessionID string

	// ForkSession, when true, forks from the resumed session instead of
	// continuing it. Requires SessionID to be set. The forked session gets
	// a new ID and does not mutate the original session.
	ForkSession bool

	// SessionFingerprint carries the provider fingerprint that the
	// parent SessionID was created against (e.g. "anthropic-direct",
	// "facade:api.z.ai"). The backend uses it to detect a cross-provider
	// fork attempt — resuming or forking a session built by a different
	// provider triggers HTTP 400 "Invalid signature in thinking block"
	// because thinking blocks are provider-signed. On mismatch the
	// backend drops the resume/fork and starts a fresh session instead,
	// surfacing a warning so the operator sees the discontinuity.
	// Empty when the parent session never recorded a fingerprint
	// (legacy outputs, or first launch on a daemon without prior session
	// history).
	SessionFingerprint string

	// InteractionEnabled, when true, instructs the delegate to signal when
	// it needs user input by including _needs_interaction and
	// _interaction_questions fields in its output.
	InteractionEnabled bool

	// ResumeConversation, when non-nil, instructs the backend to skip
	// rendering the system+user prompts from scratch and instead replay
	// the persisted conversation history captured at the previous pause.
	// The backend appends a tool_result content block (tool_use_id =
	// ResumePendingToolUseID, content = ResumeAnswer) to answer the
	// pending ask_user call, then continues the agent loop. The opaque
	// json.RawMessage shape lets each backend choose its own message
	// representation (e.g. claw uses []api.Message).
	ResumeConversation json.RawMessage

	// ResumePendingToolUseID is the ID of the tool_use block waiting
	// for an answer in the persisted conversation. Required when
	// ResumeConversation is set.
	ResumePendingToolUseID string

	// ResumeAnswer is the human-supplied answer to the captured
	// ask_user call, sent back to the LLM as the tool_result content.
	ResumeAnswer string

	// Sandbox is the live sandbox handle for the run, or nil when the
	// workflow runs without isolation. Backends route their CLI
	// subprocess calls through it (via the SDK's CommandBuilder hook
	// for claude_code, or directly via Run.Command for shell-out
	// backends) so the agent's tools execute inside the container.
	//
	// In-process backends (claw) refuse to start when this is set —
	// see runtime.containsClawNode for the compile-time guard.
	Sandbox sandbox.Run

	// ProviderHint is the resolved per-node credential-routing hint
	// from the DSL `provider:` field (post env-expansion). When
	// non-empty, backends honour it to override the default process-env
	// precedence. Known values: "anthropic" (force Anthropic-direct,
	// skip z.ai even when ZAI_API_KEY is set), "zai" (force z.ai
	// facade), "openai" (force OpenAI-direct, skip OPENAI_BASE_URL
	// overrides). Empty string means "auto" — current
	// environment-driven precedence.
	ProviderHint string

	// Hooks lets the backend surface mid-execution events back to the
	// engine without returning. Currently used by the claude_code
	// delegate to emit `tool_started` and `tool_called` events as the
	// stream parser observes ToolUseBlock / ToolResultBlock content
	// blocks — the studio's Logs panel uses these to switch its footer
	// between the LLM "thinking" loader and an in-flight tool spinner.
	// All callbacks are optional.
	Hooks TaskHooks

	// InboxDrain is the operator-chatbox drain closure for this task.
	// Backends that can call hooks at tool-call / session-end boundaries
	// (currently claude_code via PostToolUse + Stop) invoke it to inject
	// any queued operator messages into the agent's next turn — claw
	// does the same via its own internal opts.Inbox plumbing, so this
	// field is the parity surface for CLI-based backends. The runtime's
	// executor populates it from its bound InboxBinder; nil means the
	// run opted out of the inbox (CLI manual runs, …).
	InboxDrain func() []string
}

// TaskHooks are optional callbacks a backend can fire during execution
// to stream observability events back to the engine. Each callback runs
// synchronously on the backend's stream-handling goroutine, so handlers
// must not block.
type TaskHooks struct {
	// OnToolStarted fires the moment the backend observes a tool is
	// about to run. For claude_code, that's when an AssistantMessage's
	// ToolUseBlock is decoded — the tool then executes inside the CLI
	// subprocess and the engine has no other way to know it has begun.
	// ToolUseID identifies the call so OnToolCalled can correlate.
	//
	// input carries the raw JSON arguments the LLM produced for the
	// tool. The engine uses it to log the tool target (URL, file path,
	// query…) and to persist a structured payload on the tool_started
	// event for select tools (TodoWrite, WebFetch, …) so the studio's
	// per-node Tools tab can render rich cards. May be nil for backends
	// that cannot surface the input (legacy path).
	OnToolStarted func(toolName string, toolUseID string, input json.RawMessage)

	// OnToolCalled fires when the matching ToolResultBlock arrives,
	// indicating the tool has returned (successfully or with an error).
	//
	// output carries the tool's result content as a string (flattened
	// from ToolResultBlock.Content, which the SDK exposes as `any` —
	// either a bare string or a slice of nested content blocks). The
	// engine persists it on the tool_called event so the studio's
	// per-node Tools tab can render in+out side-by-side the way Claude
	// Code does. May be empty for backends that cannot surface a result.
	OnToolCalled func(toolName string, toolUseID string, isError bool, output string)

	// OnTurnFinished fires once per successful delegate call boundary
	// (claude_code: one Result; claw: not used — claw fires per-step
	// hooks via GenerationOptions.OnTurnCapture instead). The runtime
	// uses it to persist a store.TurnCheckpoint anchored at
	// (run, node, iter, turn=0) carrying the CLI's SessionID, so the
	// Fork API can later relaunch claude with --resume <id>
	// --fork-session. All fields except SessionID are optional; the
	// runtime tolerates an empty SessionID (logs a warning, skips the
	// fork-readiness side of the turn).
	OnTurnFinished func(info TurnFinishedInfo)
}

// TurnFinishedInfo is the payload of the TaskHooks.OnTurnFinished
// callback. For claude_code, one of these fires per delegate-call
// boundary — coarse compared to the per-step claw firing, but that's
// the smallest unit the CLI exposes a session id at. The runtime
// promotes it into a store.TurnCheckpoint that anchors the fork-from-
// end-of-call UX. Phase 6 (intra-call SDK hooks) would refine the
// granularity; until then we accept the asymmetry.
type TurnFinishedInfo struct {
	// SessionID is the claude CLI session id captured from the
	// ResultMessage. Empty means the CLI didn't surface one (rare —
	// usually a hard error path that doesn't fire OnTurnFinished).
	SessionID string
	// FinishReason mirrors Result.Subtype on success ("success",
	// "max_turns", etc.). Empty when the SDK didn't surface one.
	FinishReason string
	// Text is the final assistant text block emitted by the CLI for
	// this delegate call. Used for the TextDigest fingerprint.
	Text string
	// InputTokens / OutputTokens come from the CLI's Result.Usage and
	// feed the per-turn store.TurnUsage.
	InputTokens  int
	OutputTokens int
}

// BuildSystemPrompt returns the task's SystemPrompt augmented with
// optional sub-sections: the interaction protocol (when
// InteractionEnabled), and a "## Calibration" section listing any
// resolved CursorFragments. Backends should call this instead of
// reading SystemPrompt directly so every consumer sees identical
// augmentations.
//
// Ordering is stable: base prompt, then interaction protocol, then
// calibration. CursorFragments are emitted verbatim in the order the
// caller provides them; the caller is responsible for sorting (the
// runtime sorts alphabetically by cursor name for prompt-cache
// stability).
func (t Task) BuildSystemPrompt() string {
	var b strings.Builder
	// Pre-grow to dodge 2-3 reallocations in the common path: base
	// prompt (~500B) + optional interaction instruction (~300B) +
	// calibration section (~80B per fragment).
	b.Grow(len(t.SystemPrompt) + 512)
	b.WriteString(t.SystemPrompt)
	if t.InteractionEnabled {
		b.WriteString(interactionSystemInstruction)
	}
	if len(t.CursorFragments) > 0 {
		b.WriteString("\n\n## Calibration\n\n")
		for i, frag := range t.CursorFragments {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(frag)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ErrAskUser is returned by the iterion-wired `ask_user` tool's handler
// when an LLM calls it during the agent loop. It propagates up through
// the generation layer to the backend, which converts it into a standard
// _needs_interaction Result so iterion's existing pause/resume flow
// surfaces the question to the dev's terminal and re-invokes the node
// with the answer.
//
// Conversation and PendingToolUseID enable mid-tool-loop resume: when set,
// they let the backend rehydrate the LLM's exact pre-pause state on the
// next turn (the persisted message history plus a tool_result block
// answering the captured tool_use). The opaque json.RawMessage type keeps
// the delegate package agnostic of any specific LLM SDK's message shape.
type ErrAskUser struct {
	Question         string
	PendingToolUseID string
	Conversation     json.RawMessage
}

func (e *ErrAskUser) Error() string {
	return "ask_user: " + e.Question
}

// ErrRateLimited is returned by a backend when the upstream provider
// signals a rate-limit / quota-exhausted condition during streaming —
// e.g. Anthropic forfait emitting "You've hit your limit · resets …"
// as an assistant text block before the result event. The runtime
// classifies this as a clean fail (not a schema-validation crash) so
// callers can surface "switch provider" guidance instead of a
// misleading "missing required field" parse error.
type ErrRateLimited struct {
	Provider string // "claude_code", "claw", "codex", etc.
	Detail   string // raw upstream message for diagnostics
}

func (e *ErrRateLimited) Error() string {
	if e.Provider != "" {
		return "rate_limited (" + e.Provider + "): " + e.Detail
	}
	return "rate_limited: " + e.Detail
}

// ErrTransient marks a backend failure the dispatcher should retry
// (subprocess killed by OOM, peer reset, network blip, …). CLI
// backends wrap stderr-matched indicators in this type so the executor's
// retry classifier doesn't have to keep regex-matching error strings.
//
// Distinct from ErrRateLimited: rate-limit cases get their own retry
// policy (longer backoff, provider-aware budgeting) and a separate
// user-facing message.
type ErrTransient struct {
	Provider string // "claude_code", "codex", "claw", …
	Reason   string // short human-readable category ("subprocess killed", "5xx upstream")
	Detail   string // raw upstream message for diagnostics
}

func (e *ErrTransient) Error() string {
	if e.Provider != "" {
		return "transient (" + e.Provider + ", " + e.Reason + "): " + e.Detail
	}
	return "transient (" + e.Reason + "): " + e.Detail
}

// AskUserQuestionKey is the canonical key under which iterion files an
// ask_user question in the Interaction record (and looks up the answer
// on resume). Stable across runs so workflow authors can reference
// {{input.ask_user_response}} in their prompts if they want explicit
// handling beyond the auto-prepended context block.
const AskUserQuestionKey = "ask_user_response"

// Reserved input keys used to relay ask_user pause/resume state across
// runtime → executor → backend. Owned by the delegate package because
// they are part of the ask_user contract and both pkg/runtime and
// pkg/backend/model already import delegate.
//
// PriorAskUser* keys carry the question/answer text for the prompt-side
// fallback (claude_code, codex). Resume* keys carry the persisted backend
// conversation, the pending tool_use ID, and the user's answer for
// in-process backends (claw) that can rehydrate the LLM mid-loop.
const (
	PriorAskUserQuestionKey   = "_prior_ask_user_question"
	PriorAskUserAnswerKey     = "_prior_ask_user_answer"
	ResumeConversationKey     = "_resume_conversation"
	ResumePendingToolUseIDKey = "_resume_pending_tool_use_id"
	ResumeAnswerKey           = "_resume_answer"
	// SessionIDKey carries the CLI session id consumed by SessionInherit
	// (and SessionFork) nodes through the input map. Set by the executor's
	// session-continuity wiring and by the engine's fork rehydration path
	// so a forked claude_code node picks up the parent's CLI session.
	SessionIDKey = "_session_id"
)

// QueuedOperatorMessagesKey is the reserved Interaction.Questions key
// under which the runtime stores operator-queued chatbox messages
// drained at pauseAtHuman time. The resume path reads it and folds
// the messages into the system prompt (or appends to the user prompt
// for prompt-only backends) so claude_code / codex — which cannot
// accept mid-session stdin — still surface the operator's intent on
// the post-resume LLM turn. Value shape: []string (FIFO).
const QueuedOperatorMessagesKey = "_queued_operator_messages"

// Result contains the output from a delegation backend.
type Result struct {
	// Output is the parsed structured output from the CLI agent.
	Output map[string]interface{}

	// Tokens is an estimate of total tokens consumed (if available from CLI metadata).
	Tokens int

	// Duration is the wall-clock time of the subprocess execution.
	Duration time.Duration

	// ExitCode is the process exit code (0 on success).
	ExitCode int

	// Stderr contains captured stderr output (warnings, progress info).
	Stderr string

	// BackendName identifies which backend produced this result (e.g. "claude_code", "codex").
	BackendName string

	// RawOutputLen is the byte length of raw stdout before parsing.
	RawOutputLen int

	// ParseFallback is true when structured output was expected (OutputSchema set)
	// but JSON parsing fell back to wrapping plain text as {"text": "..."}.
	ParseFallback bool

	// FormattingPassUsed is true when a two-pass execution was performed:
	// Pass 1 with tools (no output format), Pass 2 with WithOutputFormat
	// (no tools) to guarantee structured output conforming to the schema.
	FormattingPassUsed bool

	// SessionID is the session ID returned by the CLI agent (empty if unavailable).
	SessionID string

	// SessionFingerprint is the provider fingerprint that produced this
	// session. Stamped onto the node output alongside SessionID so
	// downstream forks can detect a cross-provider switch and fall back
	// to a fresh session instead of failing on signed thinking blocks.
	SessionFingerprint string

	// PendingConversation is the persisted LLM conversation captured at
	// the moment the agent loop was suspended by an ask_user call. The
	// runtime serializes this opaque blob into the checkpoint so that
	// resume can replay it via Task.ResumeConversation, preserving the
	// LLM's mid-tool-loop state across the pause. Backends that cannot
	// persist conversation state (CLI-based: claude_code, codex) leave
	// this nil and rely on the [PRIOR INTERACTION] prompt-side fallback.
	PendingConversation json.RawMessage

	// PendingToolUseID is the ID of the tool_use block awaiting an
	// answer in PendingConversation. Required when PendingConversation
	// is non-nil.
	PendingToolUseID string

	// EffectiveModel is the model the backend actually used, as reported
	// by the provider — distinct from the workflow-declared `model:`.
	// For claude_code this is captured from the CLI's init SystemMessage
	// after env vars and settings.json have been resolved, so it reflects
	// overrides like ANTHROPIC_MODEL or third-party proxies (GLM, Kimi,
	// DeepSeek via ANTHROPIC_BASE_URL). Empty when the backend doesn't
	// report it.
	EffectiveModel string

	// ContextWindow is the effective model's context window size in
	// tokens, as reported by the provider via its usage payload. Zero
	// when unknown (proxy didn't fill it, or backend doesn't expose it).
	ContextWindow int

	// MaxOutputTokens is the per-call output cap reported by the provider
	// for the effective model. Zero when unknown.
	MaxOutputTokens int

	// PeakInputTokens is the largest "context loaded" observed across
	// the backend session — the sum of input + cache_creation +
	// cache_read tokens on a single assistant turn. Combined with
	// ContextWindow it yields the peak usage ratio displayed on the
	// run-view node. Zero when unknown.
	PeakInputTokens int
}
