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

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/plugin"
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

// asyncInteractionSystemInstruction is appended (after the base
// interaction protocol) for interaction: async nodes (ADR-081): the agent
// may post non-blocking questions and keep working; answers arrive in its
// message queue; await_answers is the explicit sync point.
const asyncInteractionSystemInstruction = "\n\n[ASYNC QUESTIONS]\n" +
	"You can ask the human operator questions WITHOUT stopping your work:\n" +
	"- ask_user_async(question, options?, allow_free_text?) posts a question and " +
	"returns immediately. Keep working on everything that does not depend on the " +
	"answer. The operator's answer will arrive later in your conversation as an " +
	"operator message tagged with the question id — incorporate it when it shows up.\n" +
	"- Post questions as EARLY as possible (front-load them), so the operator has " +
	"time to answer while you work.\n" +
	"- await_answers() is the sync point: call it ONLY when you truly cannot " +
	"proceed without the pending answers. If everything is already answered it " +
	"returns the answers immediately; otherwise the run pauses until the operator " +
	"replies.\n" +
	"- For a question that must block right now (a destructive or irreversible " +
	"decision), use the blocking ask_user tool instead."

// ultracodeOrchestrationInstruction is the standing-consent prompt appended
// when Task.Ultracode is set. It mirrors Anthropic's documented
// orchestration-mode recipe (xhigh effort + standing permission to launch
// multi-agent workflows): the model is told it may decompose substantial work
// across parallel subagents and lean toward adversarial verification, without
// asking first. The orchestration capability is the `agent` subagent tool,
// which the runtime makes available on the node when ultracode is active.
// See platform.claude.com/docs/en/build-with-claude/mid-conversation-effort-example.
const ultracodeOrchestrationInstruction = "\n\n## Workflow Orchestration\n\n" +
	"Ultracode mode is on: optimize for the most exhaustive, correct result, " +
	"not the fastest or cheapest. You have standing consent to orchestrate " +
	"multi-agent workflows — when a task has independent sub-parts, decompose " +
	"it and dispatch parallel subagents via the `agent` tool rather than doing " +
	"everything in one thread, and lean toward verifying findings adversarially " +
	"before acting on them. Work solo only on trivial or inherently sequential " +
	"steps. This consent stands for the whole task; you need not ask before " +
	"spawning a subagent.\n\n" +
	"Orchestration mechanics:\n" +
	"- Prefer pipelines to barriers: let each item flow through its stages " +
	"independently; synchronize all branches only when a stage genuinely needs " +
	"every prior result at once (dedup/merge across the set, early-exit on zero " +
	"findings, cross-item comparison).\n" +
	"- Subagents are stateless context-compressors: give each ONE self-contained " +
	"brief (goal, exact scope and paths, expected report shape) and work from its " +
	"summary instead of pulling raw exploration into your own context.\n\n" +
	"Quality patterns (pick per task, compose freely):\n" +
	"- Adversarial verify: for each finding, spawn independent skeptics prompted " +
	"to REFUTE it; keep only what survives a majority.\n" +
	"- Perspective-diverse verify: when something can fail in several ways, give " +
	"each verifier a distinct lens (correctness, security, performance, " +
	"does-it-reproduce) instead of N identical passes.\n" +
	"- Judge panel: generate N independent attempts from different angles, score " +
	"them, and synthesize from the winner while grafting the runners-up's best ideas.\n" +
	"- Loop-until-dry: for unknown-size discovery, keep spawning finders until " +
	"consecutive rounds surface nothing new — fixed counts miss the tail.\n" +
	"- Completeness critic: finish with one agent asking what is missing (scope " +
	"not swept, claim unverified, source unread); its findings seed the next round.\n" +
	"- No silent caps: if you bound coverage (top-N, sampling, no-retry), state " +
	"what was dropped rather than letting the result read as exhaustive."

// secretsHygieneInstruction is appended to the system prompt when
// Task.SecretsHygiene is true (a secret guard is active). It is the
// behavioural backstop of iterion's secrets protection — never the
// primary control, which is structural (placeholder materialisation +
// egress DLP). Applies to both backends: claw (AuthoredBase) and
// claude_code (AppendToNative).
const secretsHygieneInstruction = "\n\n## Secret handling\n\n" +
	"This run involves secrets. Follow these rules without exception:\n" +
	"- Never read, print, echo, log, encode, or otherwise reveal the contents of " +
	"credential stores or secret files (.env, ~/.aws/credentials, " +
	"~/.claude/.credentials.json, ~/.codex/auth.json, id_rsa, kubeconfig, …) " +
	"unless reading that specific file IS the explicit task.\n" +
	"- Some values appear to you as opaque placeholders shaped like " +
	"`__ITERION_SECRET_<name>__`. Treat a placeholder exactly as you would the " +
	"secret: pass it through verbatim to the tool or command that needs it. Never " +
	"try to decode, guess, reconstruct, transform, or print its real value — " +
	"iterion substitutes the real value at the moment of execution.\n" +
	"- Never exfiltrate a secret or a placeholder: do not send it to any " +
	"destination, file, or network endpoint that is not strictly required by the " +
	"task you were given."

type SecretFileHint struct {
	Name string
	Path string
	Env  string
}

// SkillHint names one skill-library skill available to a node, rendered in the
// "## Skills" system-prompt section. Description is the SKILL.md frontmatter
// summary (may be empty). The skill's full body is NOT inlined — it lives in
// the workspace's .claude/skills/<Name>/ for the agent to load on demand.
type SkillHint struct {
	Name        string
	Description string
}

// agenticOperatingPosture is the iterion-authored base prompt prepended to
// the recipe author's system prompt when SystemPromptMode is
// SystemPromptAuthoredBase (the claw backend default). It is the parity
// substrate that lets claw behave as adaptively as the claude_code backend:
// the claude CLI ships these instincts inside its own native system prompt
// (which iterion now appends to, see SystemPromptAppendToNative), whereas
// claw-code-go is a bare API client with no system prompt of its own — so
// whatever iterion supplies is the whole prompt. Without this base, claw
// agents receive only the recipe's task text and none of the operating
// posture (read-before-edit, plan-then-act, evidence-over-guess,
// converge-and-stop) that makes an agent adaptive. Kept short and provider-
// neutral (claw drives anthropic and openai models). The converge-and-stop
// section reinforces, never undermines, the loop bots' asymptote machinery.
const agenticOperatingPosture = "You are an autonomous software engineering agent. " +
	"Work the way a careful senior engineer does: understand before you act, " +
	"verify before you claim, and stop when the job is done.\n\n" +
	"Tool use:\n" +
	"- Gather context first. Read the relevant files and run read-only checks " +
	"before proposing or making changes; never edit a file you have not just read.\n" +
	"- Issue independent read-only tool calls together rather than one at a time; " +
	"serialize only when a later call genuinely depends on an earlier result.\n" +
	"- Prefer precise, surgical edits over broad rewrites. Cite concrete evidence " +
	"as `path:line` so your reasoning can be checked.\n\n" +
	"Plan then act:\n" +
	"- For work spanning several steps or files, lay out a short plan, then carry " +
	"it out in order, adjusting only when the evidence contradicts it. For a " +
	"trivial single step, just do it — do not pad with ceremony.\n" +
	"- When the work has several steps, keep a running task list with the " +
	"todo_write tool: record the steps up front, mark exactly one in_progress, " +
	"and flip each to completed as you finish it. This keeps your progress " +
	"visible and is the single source of truth for what is left.\n\n" +
	"Evidence over guessing:\n" +
	"- If you are unsure, find out: read the source or run a check. Do not invent " +
	"file paths, symbols, APIs, or results. When you verify something, actually " +
	"run the verification and report what happened — including failures, plainly.\n\n" +
	"Autonomy and delivery:\n" +
	"- You operate autonomously inside a workflow: the operator is not watching in " +
	"real time. For reversible steps that clearly serve the task, act — do not stop " +
	"to ask \"Shall I…?\"; reserve interaction requests for genuine forks the work " +
	"cannot resolve alone.\n" +
	"- Tool call first, narration second: never describe an action as done or in " +
	"progress unless the corresponding tool call happens in the same turn. Before " +
	"ending, reread your final words — if they promise work not yet done " +
	"(\"I'll…\", \"next I would…\"), do that work now instead of ending.\n" +
	"- Your final output is the deliverable consumed downstream: lead with the " +
	"outcome and make it self-contained — anything narrated only mid-run may " +
	"never be read.\n\n" +
	"Converge and stop:\n" +
	"- Drive the task to a stable, finished state, then stop. When you are given " +
	"prior context — earlier outputs, a previous reviewer's verdict, points already " +
	"pushed back — treat it as authoritative and do not re-litigate settled matters " +
	"without new evidence. Repeating resolved points prevents convergence."

// SystemPromptMode selects how BuildSystemPrompt composes the final system
// prompt for a Task, so the same prompt-assembly code serves backends with
// very different baselines.
type SystemPromptMode int

const (
	// SystemPromptStandalone treats the recipe author's SystemPrompt as the
	// entire prompt (plus the interaction/ultracode/calibration suffixes).
	// This is the zero value and the legacy behaviour — used by codex and any
	// caller that does not set the mode.
	SystemPromptStandalone SystemPromptMode = iota

	// SystemPromptAppendToNative emits only the author text + suffixes; the
	// caller (the claude_code backend) routes the result to the CLI's
	// --append-system-prompt so Claude Code's native agentic system prompt
	// remains the base. The agentic posture is provided natively, not by us.
	SystemPromptAppendToNative

	// SystemPromptAuthoredBase prepends agenticOperatingPosture before the
	// author text. Used by the claw backend, which has no native system
	// prompt — iterion must supply the operating posture for parity.
	SystemPromptAuthoredBase
)

// SystemPromptModeForBackend maps a backend name to its system-prompt
// composition mode. Centralised here so the executor and any other Task
// constructor stay consistent.
func SystemPromptModeForBackend(backend string) SystemPromptMode {
	switch backend {
	case BackendClaudeCode:
		return SystemPromptAppendToNative
	case BackendGrok:
		// Grok Build CLI: node's system: is delivered via --rules (append to
		// the CLI's native agentic baseline). Override would strip that
		// baseline — same trap as claude_code --system-prompt.
		return SystemPromptAppendToNative
	case BackendClaw:
		return SystemPromptAuthoredBase
	default:
		// codex, kimi, and any future/legacy backend: author text is the
		// whole prompt (kimi folds it into -p; codex replaces).
		return SystemPromptStandalone
	}
}

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

// TaskMCPServer is a resolved, user/plugin-declared MCP server carried on
// Task.MCPServers for CLI backends to forward to the agent CLI. It mirrors
// the runtime mcp.ServerConfig / claudesdk MCP server shape (stdio uses
// Command/Args/Env; http/sse use URL/Headers).
type TaskMCPServer struct {
	Name      string
	Transport string // "stdio" | "http" | "sse" (empty → stdio)
	Command   string
	Args      []string
	URL       string
	Headers   map[string]string
	Env       map[string]string
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
	// Visibility selects the sharing axis (bot|project|cross_project|
	// user|org|global). Empty keeps the legacy project-shared
	// behaviour; when set, Scope is the space name.
	Visibility string
	// BotID is the stable bot/workflow identifier used to qualify
	// structured visibility=bot spaces. Empty is invalid for
	// structured bot visibility; callers should populate it from the
	// launching bundle/bot id or Workflow.Name.
	BotID string
	// ProjectRoot, when true, re-roots the scope under the run's
	// `RepoRoot` (passed alongside via Task.RepoRoot) instead of the
	// per-run workDir. Enables cross-worktree shared scopes (e.g.
	// session-continuity memory) where bot runs from
	// `<repo>/.iterion/dispatcher/workspaces/<id>` worktrees read
	// and write the same tree a whats-next run at the repo root
	// sees.
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

	// SystemPromptMode selects how BuildSystemPrompt composes the final
	// prompt from SystemPrompt (see SystemPromptMode constants). The zero
	// value (SystemPromptStandalone) preserves legacy behaviour, so any
	// Task that does not set it is unaffected. The executor sets it from
	// the resolved backend via SystemPromptModeForBackend.
	SystemPromptMode SystemPromptMode

	// UserPrompt is the fully resolved user message text.
	UserPrompt string

	// FullAccess, when true, lifts the codex backend's sandbox to
	// "danger-full-access" (unrestricted network egress + out-of-workspace
	// writes) instead of the least-privilege mode derived from AllowedTools.
	// It is Iterion's explicit way to grant a codex node network access — e.g.
	// a CLI that reaches an external API such as codex's built-in imagegen. Off
	// by default; other backends ignore it. Set from the node-level
	// `full_access:` DSL field, so network is a deliberate, per-node choice
	// by the pipeline author.
	FullAccess bool

	// Readonly, when true, forces delegated agents into a read-only sandbox.
	// The executor copies this from the node-level `readonly:` DSL field so
	// backend enforcement matches workspace-safety scheduling.
	Readonly bool

	// Images are node-level input image paths (from the `images:` DSL field,
	// templated + resolved per run). The codex backend forwards them to the
	// codex CLI as `-i` for image-to-image — e.g. a previous keyframe reused as
	// a seed, or a character-identity anchor. Other backends ignore it.
	Images []string

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

	// Permission is the resolved tool-permission policy (the anti-
	// prompt-injection gate). Nil or a disabled policy means no gate
	// (today's bypassPermissions behaviour). When enabled, every tool
	// call is evaluated by permission.Policy.Evaluate: allow → execute,
	// deny → refuse, ask → pause the run and surface the call to the
	// human. Both backends honour the SAME policy so claude_code and
	// claw reach identical decisions. See pkg/backend/permission.
	Permission *permission.Policy

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

	// MCPServers are the user/plugin-declared MCP servers active for this
	// node (from the workflow `mcp_server` decls, project .mcp.json, and
	// enabled plugins' mcp_servers contributions). CLI backends
	// (claude_code) forward them to the agent CLI via --mcp-config so
	// their tools are available ALONGSIDE — never replacing — the native
	// toolset (WebSearch/WebFetch stay on by default). The claw backend
	// does NOT read this: it resolves the same servers into ToolDefs
	// in-process via the MCP manager. Internal servers (ask_user, board)
	// are wired separately by each backend and are absent here.
	MCPServers []TaskMCPServer

	// OutputSchema is the JSON Schema for the expected structured output.
	// Nil means free-form text output.
	OutputSchema json.RawMessage

	// Model is the resolved model spec (e.g. "anthropic/claude-sonnet-4-6").
	// Required for API-based backends; ignored by CLI-based backends.
	Model string

	// Command is the per-node CLI binary override (env-expanded). Honored
	// only by claude_code, where it replaces the default `claude` binary
	// with an alternate claude-code-compatible CLI (a pinned build or wrapper). It takes
	// precedence over the backend-level Command default. Empty means "use
	// the backend default"; ignored by all other backends.
	Command string

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

	// ReasoningEffort is the reasoning effort level sent on the wire.
	// Valid values: "low", "medium", "high", "xhigh", "max". The DSL also
	// accepts "ultracode", but the runtime remaps that to "xhigh" before
	// populating this field (see model.wireEffort) and sets Ultracode below.
	ReasoningEffort string

	// Ultracode enables the "Ultracode" mode: xhigh reasoning paired with a
	// standing-consent prerogative to orchestrate multi-agent workflows.
	// When set, BuildSystemPrompt appends a "## Workflow Orchestration"
	// section granting that consent (mirrors Anthropic's documented
	// orchestration-mode recipe), and the runtime makes the subagent tool
	// available. Reliable only on Opus 4.8 (mid-conversation system messages
	// are 4.8-only); on other models it degrades to plain xhigh.
	// See platform.claude.com/docs/en/build-with-claude/mid-conversation-effort-example.
	Ultracode bool

	// CompressMode is the resolved command-output-compression mode for this
	// node: "on" | "ultra" | "" (empty = off). Carried as a string so this
	// package and the IPC wire form stay decoupled from the rewrite enum;
	// consumers parse it via rewrite.ParseMode. When enabled, the claude_code
	// backend installs a PreToolUse hook that rewrites Bash commands via the
	// rewriter chain (Rewriters), and the claw backend carries the mode +
	// chain into its tool loop so the bash builtin compresses too. The
	// executor resolves it from the precedence chain (run override > node DSL >
	// workflow DSL > ITERION_COMPRESS env).
	CompressMode string

	// Rewriters is the active rewriter-plugin chain (rtk by default) carried
	// alongside CompressMode so both the in-process claude_code hook and the
	// (possibly sandboxed, IPC) claw runner can rebuild the rewrite.Chain. Each
	// entry is a plugin.RewriterSpec (binary locate + invoke contract). Empty
	// when no rewriter plugin is enabled.
	Rewriters []plugin.RewriterSpec

	// SecretsHygiene, when true, appends a "## Secret handling" section to
	// the system prompt: the behavioural backstop of iterion's secrets
	// protection (Layer 1). It tells the agent not to read/exfiltrate
	// credential files and to pass __ITERION_SECRET_<name>__ placeholders
	// through verbatim (iterion materialises the real value at exec). Set
	// by the executor when a secret guard is active. This is a backstop,
	// never the primary control — the structural boundaries are the
	// placeholder materialisation (Layer 1) and egress DLP (Layer 2).
	SecretsHygiene bool

	// SecretFiles lists mounted secret files that the agent may reference by
	// path or env var. BuildSystemPrompt includes the paths while preserving
	// the rule that their contents must not be read, printed, or transformed.
	SecretFiles []SecretFileHint

	// MaterializeSecrets, when non-nil, swaps secret placeholders
	// (__ITERION_SECRET_<name>__) for their real values in a string. The
	// structural half of Layer 1: backends apply it to agent-emitted tool
	// input immediately BEFORE execution, so the real secret never enters
	// the agent's view, the prompt, the event log, or the run store —
	// only the live syscall/subprocess. Set by the executor from the
	// secret guard; nil disables materialisation. Kept as a closure so
	// the delegate package stays decoupled from pkg/backend/secretguard.
	MaterializeSecrets func(string) string

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

	// SkillHints are the skill-library skills this node references (DSL
	// `skills:`), resolved to name+description, that were mirrored into the
	// workspace's .claude/skills/ at run start. They render as a "## Skills"
	// section listing each available skill so the agent knows to load it
	// on demand (the SKILL.md body lives in .claude/skills/, not the prompt).
	// Only skills referenced by THIS node (node list ∪ workflow default,
	// resolved against the library) appear. Empty = skip the section. See
	// ADR-059 and docs/skills-library.md.
	SkillHints []SkillHint

	// PresetFragment is the resolved launch-time preset bias appended to the
	// system prompt under a "## Focus" section. It carries the selected
	// file-based preset's prompt body (template-expanded) plus an optional
	// "Relevant skills:" hint line, set by the executor from the engine's
	// SetPresetFocus. Empty means no preset (or a var-only preset) — backends
	// skip the section. Distinct from CursorFragments: cursors are an
	// author-time per-node dial ("## Calibration"); a preset is an
	// operator-time, run-wide "sous-bot" focus. See ir.Preset.
	PresetFragment string

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
	// workflow runs without isolation. Supported backends route their CLI
	// subprocess calls through it (via the SDK's CommandBuilder hook for
	// claude_code, or directly via Run.Command for shell-out backends) so the
	// agent's tools execute inside the container. A backend whose SDK cannot
	// route the process must reject the task rather than escape to the host.
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
	//
	// This carries a SINGLE hint per Execute call. When the DSL declares
	// an ordered fallback chain (`provider: "anthropic,zai,openai"`), the
	// executor (dispatchWithProviderFallback) re-issues the task with the
	// next hint here after a hard failure beyond the retry budget — the
	// backend itself stays chain-unaware.
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

	// Async-interaction closures (ADR-081), bound by the executor for
	// nodes with `interaction: async`. All three are nil otherwise —
	// backends must treat a nil closure as "async ask not available for
	// this node" and surface an explicit tool error, never a silent no-op.

	// PostAsyncQuestion persists a pending Kind=async Interaction for
	// this node and emits human_input_requested{async:true}. Returns the
	// interaction ID the answer will reference.
	PostAsyncQuestion func(q AsyncQuestion) (interactionID string, err error)

	// PendingAsyncQuestions lists this node's still-unanswered async
	// questions (oldest first).
	PendingAsyncQuestions func() ([]PendingAsync, error)

	// CollectAsyncAnswers formats every answered async question of this
	// node into a single human-readable block (the await_answers tool
	// result when nothing is pending).
	CollectAsyncAnswers func() (string, error)
}

// AsyncQuestion is the input of a non-blocking ask_user_async call
// (ADR-081). Same wire shape as the blocking ask_user question.
type AsyncQuestion struct {
	Question      string
	Options       []AskUserOption
	AllowFreeText bool
}

// PendingAsync identifies one still-unanswered async question.
type PendingAsync struct {
	InteractionID string
	Question      string
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

	// OnAssistantText fires with each non-empty assistant text block
	// streamed mid-session (claude_code) — the agent's narration
	// between tool calls. The executor bridges it to an assistant_text
	// store event so conversation views can render the agent "talking"
	// while it works. Runs on the stream-handling goroutine: must not
	// block.
	OnAssistantText func(text string)
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
	// The base is the recipe author's prompt, optionally fronted by the
	// iterion-authored agentic posture when the backend has no native one
	// (claw). claude_code (SystemPromptAppendToNative) and codex/legacy
	// (SystemPromptStandalone) both emit author-only here; claude_code then
	// routes this to --append-system-prompt so the native prompt stays the base.
	base := t.SystemPrompt
	if t.SystemPromptMode == SystemPromptAuthoredBase {
		base = agenticOperatingPosture + "\n\n" + t.SystemPrompt
	}
	// Pre-grow to dodge 2-3 reallocations in the common path: base
	// prompt (~500B) + optional interaction instruction (~300B) +
	// calibration section (~80B per fragment).
	b.Grow(len(base) + 512)
	b.WriteString(base)
	if t.InteractionEnabled {
		b.WriteString(interactionSystemInstruction)
		// PostAsyncQuestion bound ⇒ the node is interaction: async.
		if t.PostAsyncQuestion != nil {
			b.WriteString(asyncInteractionSystemInstruction)
		}
	}
	if t.Ultracode {
		b.WriteString(ultracodeOrchestrationInstruction)
	}
	if t.SecretsHygiene || len(t.SecretFiles) > 0 {
		b.WriteString(secretsHygieneInstruction)
		if len(t.SecretFiles) > 0 {
			b.WriteString(secretFilesInstruction(t.SecretFiles))
		}
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
	// Skill-library skills referenced by this node. Emitted after calibration,
	// before the preset focus. Only names + descriptions — the agent loads a
	// skill's body on demand from .claude/skills/. Byte-stable (caller sorts).
	if len(t.SkillHints) > 0 {
		b.WriteString("\n\n## Skills\n\n")
		b.WriteString("The following skills are available in this workspace's .claude/skills/. Load one when its description matches the task at hand:\n")
		for _, h := range t.SkillHints {
			b.WriteString("- ")
			b.WriteString(h.Name)
			if h.Description != "" {
				b.WriteString(": ")
				b.WriteString(h.Description)
			}
			b.WriteByte('\n')
		}
	}
	// The preset focus is the operator-selected "sous-bot" bias for this run,
	// emitted last so it frames the task after the author's prompt and any
	// calibration. Kept byte-stable across nodes for prompt-cache stability.
	if t.PresetFragment != "" {
		b.WriteString("\n\n## Focus\n\n")
		b.WriteString(t.PresetFragment)
		b.WriteByte('\n')
	}
	return b.String()
}

func secretFilesInstruction(files []SecretFileHint) string {
	var b strings.Builder
	b.WriteString("\n\nMounted secret files are available for commands that need credential-file paths. Use the path or env var as a reference; do not open, read, cat, print, encode, or summarize the file contents.\n")
	for _, f := range files {
		if f.Path == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(safePromptLiteral(f.Name))
		b.WriteString(": `")
		b.WriteString(safePromptLiteral(f.Path))
		b.WriteString("`")
		if f.Env != "" {
			b.WriteString(" via `$")
			b.WriteString(safePromptLiteral(f.Env))
			b.WriteString("`")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func safePromptLiteral(s string) string {
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
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

	// Options carries the structured selectable answers when the LLM
	// called ask_user with an options list. The studio renders them as
	// clickable choices; the answer wire shape stays a plain string
	// (the picked option's id, or free text when AllowFreeText).
	Options       []AskUserOption
	AllowFreeText bool

	// PermissionMarker is set (non-nil) when this suspension is a
	// tool-permission gate pause rather than an LLM ask_user call. It
	// carries the structured request (tool, input, rule) so the pause
	// surfaces as an approval card and the runtime can compute the grant
	// on resume. See pkg/backend/permission.Marker.
	PermissionMarker map[string]any
}

// AskUserOption is one selectable answer of a structured ask_user call.
// Mirrors claw-code-go's tools.Option so the delegate package stays
// SDK-agnostic while both backends share one wire shape.
type AskUserOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
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
	// Kind refines the condition: RateLimitKindUsageWindow marks a
	// subscription/quota WINDOW exhaustion (the Anthropic forfait 5h /
	// weekly cap — waiting is the only cure), RateLimitKindTransient a
	// plain throttle worth retrying soon. Empty = unclassified
	// (legacy), treated as transient.
	Kind string
	// ResetAt is the best-effort parsed reset instant from the upstream
	// text ("resets 3pm", "reset at 2026-07-17 19:00"). Zero when not
	// parseable — callers must not depend on it.
	ResetAt time.Time
}

// ErrRateLimited.Kind values.
const (
	RateLimitKindUsageWindow = "usage_window"
	RateLimitKindTransient   = "transient"
)

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

// Presentation keys stamped alongside AskUserQuestionKey when the LLM
// called ask_user with structured options. Reserved (underscore) keys:
// the studio detects AskUserOptionsKey to render clickable choices
// instead of a bare textarea; the runtime treats them as opaque
// passthrough (ValidateOutput ignores extra keys, doPause persists the
// questions map raw). The recorded answer stays a plain string — the
// picked option's id, or typed text when AskUserAllowFreeTextKey.
const (
	AskUserOptionsKey       = "_ask_user_options"
	AskUserAllowFreeTextKey = "_ask_user_allow_free_text"
)

// AddAskUserOptionKeys stamps the structured-options presentation keys
// onto an _interaction_questions map. No-op without options so plain
// free-text ask_user pauses keep their historical single-key shape.
// Options serialize as []map so the checkpoint's JSON round-trip is
// loss-free.
func AddAskUserOptionKeys(questions map[string]any, options []AskUserOption, allowFreeText bool) {
	if len(options) == 0 {
		return
	}
	list := make([]any, 0, len(options))
	for _, o := range options {
		list = append(list, map[string]any{"id": o.ID, "label": o.Label})
	}
	questions[AskUserOptionsKey] = list
	questions[AskUserAllowFreeTextKey] = allowFreeText
}

// ParseAskUserToolInput extracts the structured option fields from a raw
// ask_user tool-input map (the claude_code PreToolUse hook's view of the
// call). Mirrors claw-code-go's ParseAskUserInput semantics: malformed
// entries are skipped rather than erroring (the hook must never fail the
// interception), labels are capped, and free text defaults to allowed
// when no options are given.
func ParseAskUserToolInput(in map[string]any) (options []AskUserOption, allowFreeText bool) {
	const (
		maxOptions   = 32
		maxOptionLen = 200
	)
	if raw, ok := in["options"].([]any); ok {
		for _, item := range raw {
			if len(options) == maxOptions {
				break
			}
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			label, _ := m["label"].(string)
			id = strings.TrimSpace(id)
			label = strings.TrimSpace(label)
			if id == "" || label == "" {
				continue
			}
			if len(label) > maxOptionLen {
				label = label[:maxOptionLen]
			}
			options = append(options, AskUserOption{ID: id, Label: label})
		}
	}
	if v, ok := in["allow_free_text"].(bool); ok {
		allowFreeText = v
	} else if len(options) == 0 {
		allowFreeText = true
	}
	return options, allowFreeText
}

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

// AwaitPendingInteractionsKey is the reserved Interaction.Questions key
// marking an await_answers-tool escalation (ADR-081): the agent called
// await_answers while async questions were still pending, so the run
// paused on a synthetic Kind=await interaction. Value shape: []any of
// map{"interaction_id","question"} — the pending async questions at
// pause time. The resume path fans the operator's answers out onto the
// referenced async interaction records before re-invoking the backend.
const AwaitPendingInteractionsKey = "_await_pending_interactions"

// Canonical tool names of the async-interaction pair (ADR-081). The
// claude_code side prefixes them with the MCP server namespace
// (mcp__iterion__…); claw registers them under these bare names.
const (
	AskUserAsyncToolName = "ask_user_async"
	AwaitAnswersToolName = "await_answers"
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
	Output map[string]any

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

	// ThinkingTokens is an approximate count of extended-thinking
	// (reasoning) tokens, re-encoded from the thinking content the backend
	// surfaced (the provider does not report thinking tokens separately).
	// Zero when the run produced no thinking. Always an approximation.
	ThinkingTokens int

	// ThinkingMs is the wall-clock spent in thinking, in milliseconds. For
	// claude_code this is a best-effort proxy (the SDK delivers assembled
	// thinking blocks, not deltas, so it is measured as the time elapsed
	// before a thinking-bearing assistant message arrived). Zero when no
	// thinking was produced.
	ThinkingMs int
}
