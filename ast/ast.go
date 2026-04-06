package ast

// ---------------------------------------------------------------------------
// File — root of the AST
// ---------------------------------------------------------------------------

// File is the root AST node representing an entire .iter source file.
type File struct {
	Vars       *VarsBlock       // top-level vars (optional, at most one)
	MCPServers []*MCPServerDecl // top-level reusable MCP server declarations
	Prompts    []*PromptDecl    // prompt declarations
	Schemas    []*SchemaDecl    // schema declarations
	Agents     []*AgentDecl     // agent node declarations
	Judges     []*JudgeDecl     // judge node declarations
	Routers    []*RouterDecl    // router node declarations
	Humans     []*HumanDecl     // human node declarations
	Tools      []*ToolNodeDecl  // tool node declarations (direct execution, no LLM)
	Workflows  []*WorkflowDecl  // workflow declarations
	Comments   []*Comment       // top-level comments (## ...)
	Span       Span
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
type MCPTransport int

const (
	MCPTransportUnknown MCPTransport = iota
	MCPTransportStdio
	MCPTransportHTTP
	MCPTransportSSE
)

func (mt MCPTransport) String() string {
	switch mt {
	case MCPTransportStdio:
		return "stdio"
	case MCPTransportHTTP:
		return "http"
	case MCPTransportSSE:
		return "sse"
	default:
		return "unknown"
	}
}

// MCPServerDecl represents a top-level `mcp_server <name>:` declaration.
type MCPServerDecl struct {
	Name      string
	Transport MCPTransport
	Command   string
	Args      []string
	URL       string
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
type FieldType int

const (
	FieldTypeString FieldType = iota
	FieldTypeBool
	FieldTypeInt
	FieldTypeFloat
	FieldTypeJSON
	FieldTypeStringArray
)

func (ft FieldType) String() string {
	switch ft {
	case FieldTypeString:
		return "string"
	case FieldTypeBool:
		return "bool"
	case FieldTypeInt:
		return "int"
	case FieldTypeFloat:
		return "float"
	case FieldTypeJSON:
		return "json"
	case FieldTypeStringArray:
		return "string[]"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Nodes — Agent
// ---------------------------------------------------------------------------

// SessionMode represents the LLM session context strategy.
type SessionMode int

const (
	SessionFresh         SessionMode = iota // new context
	SessionInherit                          // inherit parent session
	SessionArtifactsOnly                    // only persistent artifacts
	SessionFork                             // non-consuming fork from parent session
)

func (sm SessionMode) String() string {
	switch sm {
	case SessionFresh:
		return "fresh"
	case SessionInherit:
		return "inherit"
	case SessionArtifactsOnly:
		return "artifacts_only"
	case SessionFork:
		return "fork"
	default:
		return "unknown"
	}
}

// AgentDecl represents an `agent <name>:` node declaration.
type AgentDecl struct {
	Name              string
	Model             string // string literal, may contain ${...} env refs
	Delegate          string // delegation backend name (e.g. "claude_code"); when set, bypasses LLM API
	MCP               *MCPConfigDecl
	Input             string          // schema reference name
	Output            string          // schema reference name
	Publish           string          // persistent artifact name (empty if not set)
	System            string          // prompt reference name
	User              string          // prompt reference name
	Session           SessionMode     // defaults to SessionFresh
	Tools             []string        // tool capability names
	ToolMaxSteps      int             // max tool-use iterations (0 = not set)
	ReasoningEffort   string          // reasoning effort level: "low", "medium", "high", "extra_high"
	Readonly          bool            // when true, node is not considered mutating for workspace safety
	Interaction       InteractionMode // interaction handling (default none for agents)
	InteractionPrompt string          // prompt reference guiding LLM for llm_or_human decisions
	InteractionModel  string          // model for llm/llm_or_human modes (fallback to Model)
	Await             AwaitMode       // convergence strategy (none/wait_all/best_effort)
	Span              Span
}

// ---------------------------------------------------------------------------
// Nodes — Judge
// ---------------------------------------------------------------------------

// JudgeDecl represents a `judge <name>:` node declaration.
// Structurally identical to AgentDecl; semantically a judge
// produces verdicts and typically does not use tools.
type JudgeDecl struct {
	Name              string
	Model             string
	Delegate          string // delegation backend name; when set, bypasses LLM API
	MCP               *MCPConfigDecl
	Input             string
	Output            string
	Publish           string
	System            string
	User              string
	Session           SessionMode
	Tools             []string // usually empty for judges, but allowed
	ToolMaxSteps      int
	ReasoningEffort   string          // reasoning effort level: "low", "medium", "high", "extra_high"
	Readonly          bool            // when true, node is not considered mutating for workspace safety
	Interaction       InteractionMode // interaction handling (default none for judges)
	InteractionPrompt string          // prompt reference guiding LLM for llm_or_human decisions
	InteractionModel  string          // model for llm/llm_or_human modes (fallback to Model)
	Await             AwaitMode       // convergence strategy (none/wait_all/best_effort)
	Span              Span
}

// ---------------------------------------------------------------------------
// Nodes — Router
// ---------------------------------------------------------------------------

// RouterMode represents the routing strategy.
type RouterMode int

const (
	RouterFanOutAll  RouterMode = iota // fan out to all targets
	RouterCondition                    // conditional routing
	RouterRoundRobin                   // round-robin: cycle through targets one at a time
	RouterLLM                          // LLM-based routing decision
)

func (rm RouterMode) String() string {
	switch rm {
	case RouterFanOutAll:
		return "fan_out_all"
	case RouterCondition:
		return "condition"
	case RouterRoundRobin:
		return "round_robin"
	case RouterLLM:
		return "llm"
	default:
		return "unknown"
	}
}

// RouterDecl represents a `router <name>:` node declaration.
// Routers are fan-out sources and do not support the Await field
// (convergence is only meaningful on target nodes: agent, judge, human, tool).
type RouterDecl struct {
	Name   string
	Mode   RouterMode
	Model  string // only for mode: llm
	System string // prompt ref, only for mode: llm
	User   string // prompt ref, only for mode: llm
	Multi  bool   // multi-route selection, only for mode: llm
	Span   Span
}

// ---------------------------------------------------------------------------
// Await mode — convergence strategy for nodes with multiple incoming edges
// ---------------------------------------------------------------------------

// AwaitMode represents the convergence strategy when a node receives
// inputs from multiple parallel branches.
type AwaitMode int

const (
	AwaitNone       AwaitMode = iota // not a convergence point (or not explicitly set)
	AwaitWaitAll                     // wait for all incoming branches (default for convergence)
	AwaitBestEffort                  // proceed when possible, tolerate failures
)

func (am AwaitMode) String() string {
	switch am {
	case AwaitNone:
		return "none"
	case AwaitWaitAll:
		return "wait_all"
	case AwaitBestEffort:
		return "best_effort"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Interaction mode — unified across all LLM nodes
// ---------------------------------------------------------------------------

// InteractionMode controls how a node handles user interaction requests.
// It replaces the former HumanMode and is available on agent, judge, and human nodes.
type InteractionMode int

const (
	InteractionNone       InteractionMode = iota // no interaction capability (default for agent/judge)
	InteractionHuman                             // always pause for human input (default for human nodes)
	InteractionLLM                               // LLM auto-answers interaction questions
	InteractionLLMOrHuman                        // LLM decides whether to answer or escalate to human
)

func (im InteractionMode) String() string {
	switch im {
	case InteractionNone:
		return "none"
	case InteractionHuman:
		return "human"
	case InteractionLLM:
		return "llm"
	case InteractionLLMOrHuman:
		return "llm_or_human"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Nodes — Human
// ---------------------------------------------------------------------------

// HumanDecl represents a `human <name>:` node declaration.
type HumanDecl struct {
	Name              string
	Input             string          // schema reference name
	Output            string          // schema reference name
	Publish           string          // persistent artifact name
	Instructions      string          // prompt reference name
	Interaction       InteractionMode // defaults to InteractionHuman (replaces Mode)
	InteractionPrompt string          // prompt reference guiding LLM for llm_or_human decisions
	InteractionModel  string          // model for llm/llm_or_human modes (fallback to Model)
	MinAnswers        int             // minimum human answers required
	Model             string          // model identifier (required for llm / llm_or_human)
	System            string          // prompt reference for LLM system prompt
	Await             AwaitMode       // convergence strategy (none/wait_all/best_effort)
	Span              Span
}

// ---------------------------------------------------------------------------
// Nodes — Tool (direct execution, no LLM)
// ---------------------------------------------------------------------------

// ToolNodeDecl represents a `tool <name>:` node that executes
// a command directly without an LLM call.
type ToolNodeDecl struct {
	Name    string
	Command string    // command to execute, may contain ${...} env refs
	Output  string    // schema reference name
	Await   AwaitMode // convergence strategy (none/wait_all/best_effort)
	Span    Span
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
	Name        string
	Vars        *VarsBlock       // workflow-level variable declarations
	Entry       string           // entry node name
	MCP         *MCPConfigDecl   // workflow-level MCP activation/filtering
	Budget      *BudgetBlock     // execution limits (optional)
	Interaction *InteractionMode // workflow-level default interaction mode (nil = not set)
	Edges       []*Edge          // directed edges between nodes
	Span        Span
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

// ---------------------------------------------------------------------------
// Edges
// ---------------------------------------------------------------------------

// Edge represents a directed transition: `src -> dst [when ...] [as ...] [with {...}]`.
type Edge struct {
	From string       // source node name
	To   string       // target node name (can be "done" or "fail")
	When *WhenClause  // optional condition
	Loop *LoopClause  // optional loop tracking
	With []*WithEntry // optional data mappings
	Span Span
}

// WhenClause represents a `when [not] <condition>` on an edge.
type WhenClause struct {
	Condition string // condition identifier (e.g. "approved", "green", "needs_human_input")
	Negated   bool   // true if `when not <condition>`
	Span      Span
}

// LoopClause represents `as <loop_name>(<max_iterations>)` on an edge.
type LoopClause struct {
	Name          string // loop name (e.g. "refine_loop", "full_recipe_loop")
	MaxIterations int    // upper bound
	Span          Span
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
