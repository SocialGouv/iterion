package ast

import "github.com/SocialGouv/iterion/pkg/dsl/types"

// ---------------------------------------------------------------------------
// File — root of the AST
// ---------------------------------------------------------------------------

// File is the root AST node representing an entire .bot source file.
type File struct {
	Vars        *VarsBlock        // top-level vars (optional, at most one)
	Presets     *PresetsBlock     // top-level named preset value sets (optional, at most one)
	Attachments *AttachmentsBlock // top-level attachments (optional, at most one)
	Secrets     *SecretsBlock     // top-level secrets (optional, at most one)
	MCPServers  []*MCPServerDecl  // top-level reusable MCP server declarations
	Prompts     []*PromptDecl     // prompt declarations
	Schemas     []*SchemaDecl     // schema declarations
	Cursors     []*CursorDecl     // cursor declarations (prompt-engineering dials)
	Supervisors []*SupervisorDecl // supervisor declarations (concurrent node-watchers)
	Agents      []*AgentDecl      // agent node declarations
	Judges      []*JudgeDecl      // judge node declarations
	Routers     []*RouterDecl     // router node declarations
	Humans      []*HumanDecl      // human node declarations
	Tools       []*ToolNodeDecl   // tool node declarations (direct execution, no LLM)
	Computes    []*ComputeDecl    // deterministic compute node declarations (no LLM, no shell)
	Emits       []*EmitDecl       // emit node declarations (publish a run-scoped event)
	Waits       []*WaitDecl       // wait node declarations (block until a run-scoped event)
	Groups      []*GroupDecl      // reusable node-cluster declarations (compile-time macros)
	Uses        []*UseDecl        // group instantiations (`use <group> as <prefix>`)
	Subbots     []*SubbotDecl     // sub-bot node declarations (run another .bot as a nested run)
	Workflows   []*WorkflowDecl   // workflow declarations
	Comments    []*Comment        // top-level comments (## ...)
	Span        Span
}

// GroupDecl is a reusable cluster of nodes + internal edges, parameterised by
// Params and instantiated by a UseDecl. It is purely a compile-time macro:
// `expandGroups` clones each node with a `<prefix>.<name>` ID, rewrites the
// internal edges, and substitutes `{{params.X}}` before the IR compiler runs,
// so groups never reach the runtime.
type GroupDecl struct {
	Name     string
	Params   []string
	Agents   []*AgentDecl
	Judges   []*JudgeDecl
	Routers  []*RouterDecl
	Humans   []*HumanDecl
	Tools    []*ToolNodeDecl
	Computes []*ComputeDecl
	Edges    []*Edge
	Span     Span
}

// UseDecl instantiates a GroupDecl: `use <Group> as <Prefix> with { p: v }`.
// Each instance's nodes get IDs `<Prefix>.<nodeName>`; external edges address
// them with that dotted reference.
type UseDecl struct {
	Group  string
	Prefix string
	With   []*WithEntry // parameter bindings (key = param name, value = template/literal)
	Span   Span
}

// SubbotDecl is a graph node that runs another `.bot` as a nested run:
//
//	subbot run_ticket:
//	  source: "child.bot"
//	  with: { issue: "{{input.id}}" }
//	  output: ticket_verdict
//	  needs: worktree_slot
//	  isolated: true
//
// The child executes as a real run (it may contain loops); its terminal
// output is mapped back to `outputs.<subbot>.<field>`.
type SubbotDecl struct {
	Name   string
	Source string       // path/ref to the child .bot (relative to the parent's workdir)
	With   []*WithEntry // vars passed to the child run (key = var name)
	Output string       // schema reference describing the child's terminal output
	Needs  []string     // named resource leases held while the child runs
	// Isolated asserts the child run does NOT mutate the parent's shared
	// workspace (it confines writes to its own run store / worktree), so the
	// workspace-safety guard may fan it out in parallel. Mirror of an
	// agent/judge node's `readonly:`. Default false = conservatively mutating.
	Isolated bool
	Span     Span
}

// EmitDecl is an `emit` node: it publishes a named event with an optional
// immutable payload into the run-scoped event registry (ADR-051). No LLM, no
// shell — a reactive coordination primitive between parallel branches.
//
//	emit <name>:
//	  event: "ready"
//	  with: { value: "{{outputs.producer.n}}" }
type EmitDecl struct {
	Name  string
	Event string       // event name to publish (required)
	With  []*WithEntry // immutable payload (key = payload field)
	Span  Span
}

// WaitDecl is a `wait` node: it blocks its branch until the named event is
// emitted in the same run, then completes with the event payload as its output
// (ADR-051). The `timeout:` is mandatory — the "no silent infinity" invariant.
//
//	wait <name>:
//	  event: "ready"
//	  timeout: "30s"
//	  output: <schema>   ## optional: types the received payload
type WaitDecl struct {
	Name    string
	Event   string // event name to wait for (required)
	Timeout string // Go duration string (required)
	Output  string // optional schema reference for the received payload
	Span    Span
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

type Comment struct {
	Text string
	Span Span
}

// ---------------------------------------------------------------------------
// MCP
// ---------------------------------------------------------------------------

// MCPTransport identifies the transport used by an MCP server.
type MCPTransport = types.MCPTransport

const (
	MCPTransportUnknown = types.MCPTransportUnknown
	MCPTransportStdio   = types.MCPTransportStdio
	MCPTransportHTTP    = types.MCPTransportHTTP
	MCPTransportSSE     = types.MCPTransportSSE
)

// MCPServerDecl represents a top-level `mcp_server <name>:` declaration.
type MCPServerDecl struct {
	Name      string
	Transport MCPTransport
	Command   string
	Args      []string
	URL       string
	Auth      *MCPAuthDecl
	Span      Span
}

// MCPAuthDecl represents an `auth:` block under an `mcp_server`. Only
// the OAuth2 authorization-code + PKCE flow is currently wired; Type
// is "oauth2".
type MCPAuthDecl struct {
	Type      string
	AuthURL   string
	TokenURL  string
	RevokeURL string
	ClientID  string
	Scopes    []string
	Span      Span
}

// MCPConfigDecl represents a workflow-level or node-level `mcp:` block.
// Workflow blocks use AutoloadProject; node blocks use Inherit.
type MCPConfigDecl struct {
	AutoloadProject *bool
	Inherit         *bool
	Servers         []string
	Disable         []string
	Span            Span
}

// ---------------------------------------------------------------------------
// Vars
// ---------------------------------------------------------------------------

// VarsBlock represents a top-level or workflow-level `vars:` block.
type VarsBlock struct {
	Fields []*VarField
	Span   Span
}

// VarField is a single variable declaration: `name: type [= default]`.
type VarField struct {
	Name    string
	Type    TypeExpr
	Default *Literal // nil if no default
	Span    Span
}

// ---------------------------------------------------------------------------
// Presets
// ---------------------------------------------------------------------------

// PresetsBlock represents a top-level `presets:` block. Each preset is a
// named bundle of variable values (typically `dev` / `staging` / `prod`)
// selected at run time via `--preset <name>`.
type PresetsBlock struct {
	Entries []*Preset
	Span    Span
}

// Preset is one named entry inside a `presets:` block.
type Preset struct {
	Name   string
	Values []*PresetValue
	Span   Span
}

// PresetValue is a single `<var>: <literal>` pair inside a preset.
type PresetValue struct {
	Key   string
	Value *Literal
	Span  Span
}

// ---------------------------------------------------------------------------
// Attachments
// ---------------------------------------------------------------------------

// AttachmentTypeExpr enumerates the binary input types that can appear in
// an `attachments:` block. Distinct from TypeExpr because attachments are
// not string-coerceable scalars: they are persisted blobs and the SPA
// renders a file picker for them.
type AttachmentTypeExpr int

const (
	AttachmentTypeFile AttachmentTypeExpr = iota
	AttachmentTypeImage
)

func (a AttachmentTypeExpr) String() string {
	switch a {
	case AttachmentTypeFile:
		return "file"
	case AttachmentTypeImage:
		return "image"
	}
	return "unknown"
}

// AttachmentsBlock represents a top-level or workflow-level
// `attachments:` block. Attachments are binary inputs (files, images)
// uploaded from the Launch modal and persisted under the run.
type AttachmentsBlock struct {
	Fields []*AttachmentField
	Span   Span
}

// AttachmentField is a single attachment declaration. The short form is
// `name: type` (e.g. `logo: image`). The block form additionally accepts
// `description`, `accept_mime`, and `required` as nested properties:
//
//	attachments:
//	  spec: file
//	    description: "PDF de spec produit"
//	    accept_mime: ["application/pdf"]
//	    required: true
type AttachmentField struct {
	Name        string
	Type        AttachmentTypeExpr
	Required    *bool    // nil = false (default)
	AcceptMIME  []string // nil = inherit server allowlist
	Description string
	Span        Span
}

// ---------------------------------------------------------------------------
// Secrets
// ---------------------------------------------------------------------------

// SecretsBlock represents a top-level `secrets:` block. Each secret
// declares a value source (typically a "${ENV}" reference) the agent
// never sees directly — references render as opaque placeholders and
// iterion materialises the real value at tool/shell execution.
type SecretsBlock struct {
	Fields []*SecretField
	Span   Span
}

// SecretField is a single secret declaration. The short form is
// `name: "value"`. The block form additionally accepts `value`,
// `as` ("value" or "file"), `mount_path`, `env`, `hosts`
// (egress scoping consumed by Layer 2), and `description`:
//
//	secrets:
//	  github_token: "${GITHUB_TOKEN}"
//	  deploy_key:
//	    value: "${DEPLOY_KEY}"
//	    as: file
//	    mount_path: "/run/iterion/secrets/deploy_key"
//	    env: "GIT_SSH_KEY_PATH"
//	    hosts: ["api.github.com", "github.com"]
type SecretField struct {
	Name        string
	Value       string   // raw value expr (may contain ${ENV} / {{vars.X}})
	As          string   // ""/"value" (placeholder) or "file" (mounted file)
	MountPath   string   // in-sandbox path for file secrets; empty = runtime default
	Env         string   // optional env var pointing at MountPath for file secrets
	Optional    bool     // file secret: skip the mount (no error) when unresolved
	Hosts       []string // approved egress hosts (nil = no restriction)
	Description string
	Span        Span
}

// ---------------------------------------------------------------------------
// Prompts
// ---------------------------------------------------------------------------

// PromptDecl represents `prompt <name>:` followed by template text.
type PromptDecl struct {
	Name string
	Body string // raw text, may contain {{...}} template expressions
	Span Span
}

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

// SchemaDecl represents `schema <name>:` followed by field definitions.
type SchemaDecl struct {
	Name   string
	Fields []*SchemaField
	Span   Span
}

// SchemaField is a single field in a schema: `name: type [enum: ...]`.
type SchemaField struct {
	Name       string
	Type       FieldType
	EnumValues []string // non-nil only if enum constraint present
	Span       Span
}

// FieldType enumerates the V1 schema field types.
type FieldType = types.FieldType

const (
	FieldTypeString      = types.FieldTypeString
	FieldTypeBool        = types.FieldTypeBool
	FieldTypeInt         = types.FieldTypeInt
	FieldTypeFloat       = types.FieldTypeFloat
	FieldTypeJSON        = types.FieldTypeJSON
	FieldTypeStringArray = types.FieldTypeStringArray
)

// ---------------------------------------------------------------------------
// Cursors — prompt-engineering dials on agent/judge nodes
// ---------------------------------------------------------------------------

// CursorDecl is a top-level `cursor <name>:` declaration. Each cursor
// defines either a closed set of enum values (`values:`) or a numeric
// 0..1 band map (`bands:`) — not both. The runtime resolves an agent's
// `cursors:` invocation against the matching CursorDecl, picks the
// prompt fragment, and appends it to the system prompt under a
// `## Calibration` section.
type CursorDecl struct {
	Name        string
	Description string
	Values      []*CursorEnumValue // enum form (ordered for numeric→position fallback)
	Bands       []*CursorBand      // numeric form
	Span        Span
}

// CursorEnumValue is one entry of a `values:` block on a cursor decl:
// `name: "prompt fragment"`. Order is preserved so a numeric invocation
// (e.g. depth: 0.7) can snap to a position when the cursor declares
// values rather than bands.
type CursorEnumValue struct {
	Name   string
	Prompt string
	Span   Span
}

// CursorBand is one entry of a `bands:` block: `"lo..hi": "prompt"`.
// Lo/Hi are parsed lazily by the IR compiler from Range so the AST
// stays a verbatim mirror of the source text.
type CursorBand struct {
	Range  string // raw "lo..hi" key as written
	Prompt string
	Span   Span
}

// CursorBlock is an agent/judge `cursors:` activation block. Enabled
// gates the whole block: when false, nothing is appended to the
// system prompt regardless of Settings (cache-friendly toggle).
type CursorBlock struct {
	Enabled  bool
	Settings []*CursorSetting // declaration order preserved for round-trip
	Span     Span
}

// CursorSetting binds a cursor name to a raw value (ident, number, or
// quoted string containing ${VAR}). Type detection — enum lookup vs
// numeric band — is deferred to IR resolution.
type CursorSetting struct {
	Key   string
	Value string
	Span  Span
}

// ---------------------------------------------------------------------------
// Supervisors — concurrent node-watchers
// ---------------------------------------------------------------------------

// SupervisorDecl is a top-level `supervisor <name>:` declaration. A
// supervisor is NOT a graph node: it is a concurrent watcher the engine
// spawns at run start, armed only while one of its watched nodes is the
// active node. It enqueues steering messages the supervised node picks
// up at its next turn (node-scoped so a late message can't leak into the
// next node). See docs/supervisors.md.
type SupervisorDecl struct {
	Name     string
	Watches  []string // agent node ids to watch; empty = whole run
	Model    string   // claw model spec; empty = auto-detect / ITERION_DEFAULT_SUPERVISOR_MODEL
	System   string   // prompt reference name (the supervision policy)
	Cooldown string   // raw duration string ("30s"); parsed by the IR compiler
	MaxEvals int      // hard cap on LLM evaluations (0 = default)
	Span     Span
}

// ---------------------------------------------------------------------------
// Nodes — Agent
// ---------------------------------------------------------------------------

// SessionMode represents the LLM session context strategy.
type SessionMode = types.SessionMode

const (
	SessionFresh              = types.SessionFresh
	SessionInherit            = types.SessionInherit
	SessionInheritIfAvailable = types.SessionInheritIfAvailable
	SessionArtifactsOnly      = types.SessionArtifactsOnly
	SessionFork               = types.SessionFork
)

// LLMDecl groups the node properties shared verbatim by AgentDecl and
// JudgeDecl — the two kinds differ only semantically (an agent acts; a
// judge produces verdicts and typically uses no tools), not
// structurally. Embedded (not named) so existing `decl.Model`-style
// field access keeps working via promotion, and parsed by the single
// parseLLMProp helper.
type LLMDecl struct {
	Model             string // string literal, may contain ${...} env refs
	Backend           string // execution backend name (e.g. "claude_code"); when set, bypasses direct LLM API
	Provider          string // credential routing hint(s): single ("anthropic"/"zai"/"openai"/""=auto) or an ordered fallback chain ("anthropic,zai,openai"); may contain ${...} env refs
	Command           string // per-node CLI binary override, honored by claude_code (default "claude"); may contain ${...} env refs
	MCP               *MCPConfigDecl
	Input             string           // schema reference name
	Output            string           // schema reference name
	Publish           string           // persistent artifact name (empty if not set)
	ArtifactLabels    []string         // artifact_labels: applied to the published artifact
	System            string           // prompt reference name
	User              string           // prompt reference name
	Session           SessionMode      // defaults to SessionFresh
	Tools             []string         // tool capability names
	ToolPolicy        []string         // per-node tool policy patterns (nil = inherit workflow)
	Capabilities      []string         // host-side capabilities granted to the node (e.g. board.create)
	Skills            []string         // skill-library skills referenced by the node (nil = inherit workflow default)
	ToolMaxSteps      int              // max tool-use iterations (0 = not set)
	MaxTokens         int              // max output tokens per LLM call (0 = inherit backend default)
	ReasoningEffort   string           // reasoning effort level: "low", "medium", "high", "xhigh", "max"
	Timeout           string           // per-node wall-clock timeout as a Go duration ("20m", "1200s"); empty = none; may contain ${VAR} env refs
	Readonly          bool             // when true, node is not considered mutating for workspace safety
	FullAccess        bool             // when true, lift the codex backend sandbox to danger-full-access (network + out-of-workspace writes); off by default; other backends ignore it
	Images            []string         // node-level `images:` — input image paths (templated) forwarded to the codex backend as `-i` for image-to-image; other backends ignore it
	Interaction       InteractionMode  // interaction handling (default none)
	InteractionPrompt string           // prompt reference guiding LLM for llm_or_human decisions
	InteractionModel  string           // model for llm/llm_or_human modes (fallback to Model)
	Await             AwaitMode        // convergence strategy (none/wait_all/best_effort)
	Compaction        *CompactionBlock // per-node compaction overrides (nil = inherit workflow)
	Memory            *MemoryBlock     // per-node workspace memory opt-in (nil = disabled)
	Sandbox           *SandboxBlock    // node-level sandbox override; nil inherits from workflow (see pkg/sandbox)
	Cursors           *CursorBlock     // prompt-engineering cursor activations (nil = none)
	Compress          string           // compress output-compression mode: on|ultra|off ("" = inherit)
	Permission        string           // permission gate mode override: off|ask|deny ("" = inherit workflow)
	Needs             []string         // resource names acquired before running (workflow.resources)
}

// AgentDecl represents an `agent <name>:` node declaration.
type AgentDecl struct {
	Name string
	LLMDecl
	Span Span
}

// ---------------------------------------------------------------------------
// Nodes — Judge
// ---------------------------------------------------------------------------

// JudgeDecl represents a `judge <name>:` node declaration. Structurally
// identical to AgentDecl (both embed LLMDecl); semantically a judge
// produces verdicts and typically does not use tools.
type JudgeDecl struct {
	Name string
	LLMDecl
	Span Span
}

// ---------------------------------------------------------------------------
// Nodes — Router
// ---------------------------------------------------------------------------

// RouterMode represents the routing strategy.
type RouterMode = types.RouterMode

const (
	RouterFanOutAll  = types.RouterFanOutAll
	RouterCondition  = types.RouterCondition
	RouterRoundRobin = types.RouterRoundRobin
	RouterLLM        = types.RouterLLM
	RouterFanOutEach = types.RouterFanOutEach
)

// RouterDecl represents a `router <name>:` node declaration.
// Routers are fan-out sources and do not support the Await field
// (convergence is only meaningful on target nodes: agent, judge, human, tool).
type RouterDecl struct {
	Name            string
	Mode            RouterMode
	Model           string   // only for mode: llm
	Backend         string   // execution backend name, only for mode: llm
	Provider        string   // credential routing hint, only for mode: llm; may contain ${...} env refs
	System          string   // prompt ref, only for mode: llm
	User            string   // prompt ref, only for mode: llm
	Multi           bool     // multi-route selection, only for mode: llm
	ReasoningEffort string   // reasoning effort level: "low", "medium", "high", "xhigh", "max" (only for mode: llm)
	Over            string   // array source template, only for mode: fan_out_each (e.g. "{{outputs.decompose.tickets}}")
	As              string   // per-item binding name, only for mode: fan_out_each (default: "item")
	Key             string   // item field holding its unique id, only for mode: fan_out_each (enables DAG scheduling)
	DependsOn       string   // item field holding the array of ids it depends on, only for mode: fan_out_each
	Needs           []string // resource names acquired before running (workflow.resources)
	Span            Span
}

// ---------------------------------------------------------------------------
// Await mode — convergence strategy for nodes with multiple incoming edges
// ---------------------------------------------------------------------------

// AwaitMode represents the convergence strategy when a node receives
// inputs from multiple parallel branches.
type AwaitMode = types.AwaitMode

const (
	AwaitNone       = types.AwaitNone
	AwaitWaitAll    = types.AwaitWaitAll
	AwaitBestEffort = types.AwaitBestEffort
)

// ---------------------------------------------------------------------------
// Interaction mode — unified across all LLM nodes
// ---------------------------------------------------------------------------

// InteractionMode controls how a node handles user interaction requests.
// It is available on agent, judge, and human nodes.
type InteractionMode = types.InteractionMode

const (
	InteractionNone       = types.InteractionNone
	InteractionHuman      = types.InteractionHuman
	InteractionLLM        = types.InteractionLLM
	InteractionLLMOrHuman = types.InteractionLLMOrHuman
	InteractionReview     = types.InteractionReview
)

// ---------------------------------------------------------------------------
// Nodes — Human
// ---------------------------------------------------------------------------

// HumanDecl represents a `human <name>:` node declaration.
type HumanDecl struct {
	Name              string
	Input             string          // schema reference name
	Output            string          // schema reference name
	Publish           string          // persistent artifact name
	ArtifactLabels    []string        // artifact_labels: applied to the published artifact
	Instructions      string          // prompt reference name
	Interaction       InteractionMode // defaults to InteractionHuman (replaces Mode)
	InteractionPrompt string          // prompt reference guiding LLM for llm_or_human decisions
	InteractionModel  string          // model for llm/llm_or_human modes (fallback to Model)
	MinAnswers        int             // minimum human answers required
	Model             string          // model identifier (required for llm / llm_or_human / review)
	System            string          // prompt reference for LLM system prompt
	Await             AwaitMode       // convergence strategy (none/wait_all/best_effort)

	// Review-gate fields (interaction: review).
	ReviewURL     string // template for the review env URL (e.g. "{{outputs.provision.url}}")
	Posture       string // "human_required" (default) | "agent_verdict_ok"
	MergeStrategy string // "squash" (default) | "merge"
	MergeInto     string // "current" (default) | "none" | <branch>
	MaxTurns      int    // dialogue asymptote backstop

	Span Span
}

// ---------------------------------------------------------------------------
// Nodes — Tool (direct execution, no LLM)
// ---------------------------------------------------------------------------

// ToolNodeDecl represents a `tool <name>:` node that executes
// a command directly without an LLM call.
//
// A tool node carries EITHER `Command` (an inline shell snippet, executed
// via `sh -c`) OR `Script` (the body of a higher-level interpreter
// snippet such as Node or Python, written to a temp file inside the
// workspace and executed by the interpreter named by `Language`).
// Setting both is a validation error; setting neither is also an error.
type ToolNodeDecl struct {
	Name           string
	Command        string        // command to execute, may contain ${...} env refs and {{...}} template refs
	Script         string        // script body for higher-level interpreters (mutually exclusive with Command)
	Language       string        // interpreter selector for Script: js | py | sh | bash. Defaults to sh when empty.
	Input          string        // optional input schema reference name
	Output         string        // schema reference name
	Publish        string        // persistent artifact name (empty if not set)
	ArtifactLabels []string      // artifact_labels: applied to the published artifact
	Await          AwaitMode     // convergence strategy (none/wait_all/best_effort)
	Sandbox        *SandboxBlock // node-level sandbox override; nil inherits from workflow
	Compress       string        // compress output-compression mode: on|ultra|off ("" = inherit)
	Permission     string        // permission gate mode override: off|ask|deny ("" = inherit workflow)
	Needs          []string      // resource names acquired before running (workflow.resources)

	// Verified Action quad (ADR-044) — all optional; a tool node without
	// these behaves exactly as before (recipe only, exit-code = success).
	Goal          string         // natural-language outcome (fuel for recovery rungs)
	Postcondition string         // cheap deterministic check (shell, exit 0 = met); single source of truth for success
	Policy        string         // required | recover | best_effort ("" defaults to required when Postcondition is set)
	Recovery      *RecoveryBlock // bounded recovery rung config (nil = no recovery rungs)

	// ParallelSafe (`parallel_safe: true`) asserts that concurrent fan-out
	// replays of this tool write only to disjoint, item-keyed targets and never
	// race one another on the shared workspace, letting the workspace-safety
	// guard fan the tool out in parallel. Mirror of a subbot's `isolated:` /
	// an agent-judge `readonly:`, for a tool that still writes but partitions
	// its writes. Default false = conservatively mutating (serialized fan-out).
	ParallelSafe bool
	Span         Span
}

// RecoveryBlock configures the bounded recovery rungs of a Verified Action
// tool node (ADR-044). Recovery rungs only run under `policy: recover`.
type RecoveryBlock struct {
	MaxRepairAttempts int      // rung 3 (self-repair) bound; 0 → default 1 under recover
	MaxAgentAttempts  int      // rung 4 (agent recovery) bound; 0 = OFF (opt-in)
	Model             string   // recovery LLM spec (empty = node/workflow default)
	AgentTools        []string // rung-4 toolset (empty = node capabilities)
	Span              Span
}

// ---------------------------------------------------------------------------
// Nodes — Compute (deterministic expression node, no LLM, no shell)
// ---------------------------------------------------------------------------

// ComputeDecl represents a `compute <name>:` node that evaluates a set of
// expressions over `vars`/`input`/`outputs`/`artifacts`/`loop`/`run` to
// produce a structured output. Used for streak detection, boolean ANDs,
// counters, etc., without invoking an LLM or shelling out.
type ComputeDecl struct {
	Name           string
	Input          string         // optional input schema reference name
	Output         string         // schema reference name (defines the output fields)
	Publish        string         // persistent artifact name (empty if not set)
	ArtifactLabels []string       // artifact_labels: applied to the published artifact
	Expr           []*ComputeExpr // ordered list of field-name → expression-source pairs
	Await          AwaitMode      // convergence strategy (none/wait_all/best_effort)
	Span           Span
}

// ComputeExpr is one entry inside a `compute` node's `expr:` block:
// `<key>: "<expression>"`.
type ComputeExpr struct {
	Key  string
	Expr string // raw expression source (parsed at compile time)
	Span Span
}

// ---------------------------------------------------------------------------
// Terminal nodes — done / fail
// ---------------------------------------------------------------------------

// done and fail are reserved identifiers, not declared in the DSL.
// They appear only as edge targets inside workflow declarations.
// The parser recognizes them by name; no AST declaration is needed.

// ---------------------------------------------------------------------------
// Workflow
// ---------------------------------------------------------------------------

// WorkflowDecl represents a `workflow <name>:` declaration.
type WorkflowDecl struct {
	Name           string
	Vars           *VarsBlock        // workflow-level variable declarations
	Attachments    *AttachmentsBlock // workflow-level attachments declarations
	Entry          string            // entry node name
	DefaultBackend string            // workflow-level default backend (empty = not set)
	ToolPolicy     []string          // workflow-level tool policy patterns (nil = open)
	Capabilities   []string          // workflow-level default host capabilities (nil = inherit none)
	Skills         []string          // workflow-level default skill-library references (nil = none)
	MCP            *MCPConfigDecl    // workflow-level MCP activation/filtering
	Budget         *BudgetBlock      // execution limits (optional)
	Resources      *ResourcesBlock   // named counting semaphores (optional)
	Compaction     *CompactionBlock  // session compaction defaults for all nodes (optional)
	Interaction    *InteractionMode  // workflow-level default interaction mode (nil = not set)
	Worktree       string            // "auto" creates a per-run git worktree; "" or "none" runs in-place
	Compress       string            // compress output-compression mode: on|ultra|off ("" = unset)
	Permission     string            // permission gate mode: off|ask|deny ("" = unset → off)
	Allow          []string          // permission allow rules (Claude-Code `Tool(pattern)` syntax)
	Ask            []string          // permission ask rules
	Deny           []string          // permission deny rules
	Sandbox        *SandboxBlock     // sandbox: short or block form (nil = inherit global default)
	Edges          []*Edge           // directed edges between nodes
	Span           Span
}

// BudgetBlock represents execution limits for a workflow.
type BudgetBlock struct {
	MaxParallelBranches int     // 0 = not set
	MaxDuration         string  // e.g. "60m", empty = not set
	MaxCostUSD          float64 // 0 = not set
	MaxTokens           int     // 0 = not set
	MaxIterations       int     // 0 = not set
	Span                Span
}

// ResourcesBlock declares named counting semaphores at the workflow level.
// Each entry is `<name>: <capacity>`. A node that declares `needs: <name>`
// acquires one slot before running and releases it after — bounding
// concurrent use of a scarce shared resource (e.g. Godot/Blender sessions)
// without a global parallelism cap.
type ResourcesBlock struct {
	Capacities map[string]int // resource name → capacity
	// Members holds the named-instance pool for a resource declared as a
	// bracketed ident-list (e.g. `godot: [godot-s1, godot-s2]`) — each acquire
	// leases one distinct member id. nil/absent for the counting-only form
	// (`godot: 5`), where capacity is just a count with no instance identity.
	Members map[string][]string
	Span    Span
}

// CompactionBlock configures session compaction. Both fields use a "0/nil
// means inherit" convention so workflow-level defaults fall through to
// node-level overrides which fall through to env / built-in defaults.
type CompactionBlock struct {
	Threshold      *float64 // ratio of model context window (0 < t <= 1); nil = inherit
	PreserveRecent *int     // recent messages kept verbatim (>= 1); nil = inherit
	Span           Span
}

// MemoryBlock is the AST shape of an agent/judge `memory:`
// sub-block: workspace-scoped, opt-in iterion memory that lives
// under ~/.iterion/projects/<encoded-workdir>/memory/<scope>/...
// Pointers preserve "field-omitted" vs "explicit zero" so the IR
// compiler can apply defaults consistently.
type MemoryBlock struct {
	Enabled          *bool
	Scope            *string
	Autoload         []string
	Read             *bool
	Write            *bool
	PreCompactInject *bool
	// ProjectRoot re-roots the scope under the project's repo
	// instead of the per-run workDir. Used to share a scope across
	// dispatcher-spawned bot runs (each running in its own worktree)
	// and whats-next runs that live at the repo root — e.g.
	// session-continuity memory shared between bots and Nexie.
	ProjectRoot *bool
	// Visibility selects the sharing axis: bot | project | cross_project
	// | user | org | global. Empty (or absent) keeps the legacy
	// per-bot/per-project behaviour. When set, Scope is the space name.
	Visibility *string
	Span       Span
}

// SandboxBlock is the AST representation of a `sandbox:` block. Two
// surface forms compile down to this same struct:
//
//	sandbox: auto                # short form → Mode="auto", everything else zero
//	sandbox: none                # short form → Mode="none"
//	sandbox:                     # block form → Mode="inline" (or explicit)
//	  image: "alpine:3"
//	  env:
//	    KEY: value
//	  mounts: [...]
//	  network:
//	    mode: allowlist
//	    rules: [...]
//
// Mode is the activation discriminator the IR + runtime consume. The
// remaining fields are populated only by the block form; the short
// form leaves them empty.
type SandboxBlock struct {
	Mode            string               // "auto" | "none" | "inline" | "" (inherit when on a node)
	Image           string               // when Mode=inline (mutually exclusive with Build)
	Build           *SandboxBuildBlock   // when Mode=inline (V2-6, mutually exclusive with Image)
	User            string               // remoteUser
	WorkspaceFolder string               // absolute path inside the container
	HostState       string               // "auto" | "none" | "" — bind host ~/.iterion + ~/.claude into the sandbox
	PostCreate      string               // shell snippet
	Env             map[string]string    // containerEnv
	Mounts          []string             // devcontainer mount syntax
	Network         *SandboxNetworkBlock // egress filtering
	Span            Span
}

// SandboxBuildBlock is the AST representation of a `build:` sub-block
// under `sandbox:`. The fields mirror pkg/sandbox.Build 1:1 — the IR
// compiler converts to the runtime shape via [ir.SandboxBuild].
type SandboxBuildBlock struct {
	Dockerfile string            // relative path; defaults to "Dockerfile"
	Context    string            // relative path; defaults to dir of Dockerfile
	Args       map[string]string // build-arg overrides (--opt build-arg:K=V)
	Span       Span
}

// SandboxNetworkBlock is the AST representation of a `network:`
// sub-block under `sandbox:`. The fields mirror pkg/sandbox.Network
// 1:1 — the IR compiler converts to the runtime shape.
type SandboxNetworkBlock struct {
	Mode    string   // "allowlist" | "denylist" | "open" | ""
	Preset  string   // "iterion-default" or named preset
	Rules   []string // glob patterns + "!exclusions"
	Inherit string   // "merge" | "replace" | "append" — node scope only
	Span    Span
}

// ---------------------------------------------------------------------------
// Edges
// ---------------------------------------------------------------------------

// Edge represents a directed transition: `src -> dst [when ...] [as ...] [with {...}]`.
type Edge struct {
	From    string         // source node name
	To      string         // target node name (can be "done" or "fail")
	When    *WhenClause    // optional condition
	Loop    *LoopClause    // optional loop tracking
	Foreach *ForeachClause // optional sequential foreach iteration (mutually exclusive with Loop)
	With    []*WithEntry   // optional data mappings
	Span    Span
}

// ForeachClause represents `as foreach <name>(<item> in <collection>)` on a
// (back-)edge: it iterates the body once per element of the collection, in
// order, binding the current element under the `each.<name>` namespace
// ({{each.<name>.item|index|count|first|last}}). The collection is a template
// resolved at runtime to a JSON array.
type ForeachClause struct {
	Name       string // iteration name (e.g. "scan")
	Item       string // element binding identifier (e.g. "item") — currently informational
	Collection string // collection template, e.g. "{{outputs.list.items}}"
	Span       Span
}

// WhenClause represents a `when [not] <condition>` or `when <expression>` on
// an edge. When Expr is non-empty, it is the raw expression source and
// supersedes Condition/Negated. The simple boolean-field form remains
// supported for ergonomics; the expression form unlocks compound conditions
// like `when approved && loop.l.previous_output.approved`.
type WhenClause struct {
	Condition string // condition identifier (e.g. "approved"); empty when Expr set
	Negated   bool   // true if `when not <condition>`; ignored when Expr set
	Expr      string // raw expression source (e.g. "a && b == 1"); empty when using simple form
	Span      Span
}

// LoopClause represents `as <loop_name>(<max_iterations>)` on an edge.
// The cap can be either a literal int (`as fix_loop(3)`) or a template
// string evaluated at the moment the loop is consulted
// (`as fix_loop("{{outputs.select_candidate.fix_loop_max}}")`). Exactly
// one of MaxIterations / MaxIterationsExpr is populated.
type LoopClause struct {
	Name              string // loop name (e.g. "refine_loop", "full_recipe_loop")
	MaxIterations     int    // upper bound (literal form)
	MaxIterationsExpr string // template form, e.g. `{{outputs.X.fix_loop_max}}`
	// Unbounded marks `as <name>(unbounded)` / `as <name>(unbounded <fuel>)`:
	// the iteration count is not user-capped; the loop runs until a `when`-exit
	// fires (convergence) with a mandatory fuel ceiling as the backstop, plus
	// a runtime liveness monitor that halts a no-progress (fixpoint) loop.
	Unbounded bool
	FuelCap   int // optional per-loop fuel for the unbounded form (0 = inherit budget.max_iterations)
	Span      Span
}

// WithEntry is a single key-value mapping inside a `with { ... }` block.
type WithEntry struct {
	Key   string // target input field name
	Value string // template string, e.g. "{{outputs.context_builder}}"
	Span  Span
}

// ---------------------------------------------------------------------------
// Types & Literals
// ---------------------------------------------------------------------------

// TypeExpr enumerates the V1 variable/field types.
type TypeExpr int

const (
	TypeString TypeExpr = iota
	TypeBool
	TypeInt
	TypeFloat
	TypeJSON
	TypeStringArray
)

func (te TypeExpr) String() string {
	switch te {
	case TypeString:
		return "string"
	case TypeBool:
		return "bool"
	case TypeInt:
		return "int"
	case TypeFloat:
		return "float"
	case TypeJSON:
		return "json"
	case TypeStringArray:
		return "string[]"
	default:
		return "unknown"
	}
}

// LiteralKind distinguishes literal value types.
type LiteralKind int

const (
	LitString LiteralKind = iota
	LitInt
	LitFloat
	LitBool
)

// Literal represents a default value in a var declaration.
type Literal struct {
	Kind     LiteralKind
	Raw      string  // raw text as written in source
	StrVal   string  // if LitString
	IntVal   int64   // if LitInt
	FloatVal float64 // if LitFloat
	BoolVal  bool    // if LitBool
}
