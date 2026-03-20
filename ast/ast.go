package ast

// ---------------------------------------------------------------------------
// File — root of the AST
// ---------------------------------------------------------------------------

// File is the root AST node representing an entire .iter source file.
type File struct {
	Vars      *VarsBlock      // top-level vars (optional, at most one)
	Prompts   []*PromptDecl   // prompt declarations
	Schemas   []*SchemaDecl   // schema declarations
	Agents    []*AgentDecl    // agent node declarations
	Judges    []*JudgeDecl    // judge node declarations
	Routers   []*RouterDecl   // router node declarations
	Joins     []*JoinDecl     // join node declarations
	Humans    []*HumanDecl    // human node declarations
	Tools     []*ToolNodeDecl // tool node declarations (direct execution, no LLM)
	Workflows []*WorkflowDecl // workflow declarations
	Comments  []*Comment      // top-level comments (## ...)
	Span      Span
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

type Comment struct {
	Text string
	Span Span
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
)

func (sm SessionMode) String() string {
	switch sm {
	case SessionFresh:
		return "fresh"
	case SessionInherit:
		return "inherit"
	case SessionArtifactsOnly:
		return "artifacts_only"
	default:
		return "unknown"
	}
}

// AgentDecl represents an `agent <name>:` node declaration.
type AgentDecl struct {
	Name         string
	Model        string      // string literal, may contain ${...} env refs
	Delegate     string      // delegation backend name (e.g. "claude_code"); when set, bypasses LLM API
	Input        string      // schema reference name
	Output       string      // schema reference name
	Publish      string      // persistent artifact name (empty if not set)
	System       string      // prompt reference name
	User         string      // prompt reference name
	Session      SessionMode // defaults to SessionFresh
	Tools        []string    // tool capability names
	ToolMaxSteps int         // max tool-use iterations (0 = not set)
	Span         Span
}

// ---------------------------------------------------------------------------
// Nodes — Judge
// ---------------------------------------------------------------------------

// JudgeDecl represents a `judge <name>:` node declaration.
// Structurally identical to AgentDecl; semantically a judge
// produces verdicts and typically does not use tools.
type JudgeDecl struct {
	Name         string
	Model        string
	Delegate     string // delegation backend name; when set, bypasses LLM API
	Input        string
	Output       string
	Publish      string
	System       string
	User         string
	Session      SessionMode
	Tools        []string // usually empty for judges, but allowed
	ToolMaxSteps int
	Span         Span
}

// ---------------------------------------------------------------------------
// Nodes — Router
// ---------------------------------------------------------------------------

// RouterMode represents the routing strategy.
type RouterMode int

const (
	RouterFanOutAll RouterMode = iota // fan out to all targets
	RouterCondition                   // conditional routing
)

func (rm RouterMode) String() string {
	switch rm {
	case RouterFanOutAll:
		return "fan_out_all"
	case RouterCondition:
		return "condition"
	default:
		return "unknown"
	}
}

// RouterDecl represents a `router <name>:` node declaration.
type RouterDecl struct {
	Name string
	Mode RouterMode
	Span Span
}

// ---------------------------------------------------------------------------
// Nodes — Join
// ---------------------------------------------------------------------------

// JoinStrategy represents how a join aggregates branches.
type JoinStrategy int

const (
	JoinWaitAll    JoinStrategy = iota // wait for all required branches
	JoinBestEffort                     // proceed when possible
)

func (js JoinStrategy) String() string {
	switch js {
	case JoinWaitAll:
		return "wait_all"
	case JoinBestEffort:
		return "best_effort"
	default:
		return "unknown"
	}
}

// JoinDecl represents a `join <name>:` node declaration.
type JoinDecl struct {
	Name     string
	Strategy JoinStrategy
	Require  []string // node names to wait for
	Output   string   // schema reference name
	Span     Span
}

// ---------------------------------------------------------------------------
// Nodes — Human
// ---------------------------------------------------------------------------

// HumanMode represents how the human node pauses execution.
type HumanMode int

const (
	HumanPauseUntilAnswers HumanMode = iota
)

func (hm HumanMode) String() string {
	switch hm {
	case HumanPauseUntilAnswers:
		return "pause_until_answers"
	default:
		return "unknown"
	}
}

// HumanDecl represents a `human <name>:` node declaration.
type HumanDecl struct {
	Name         string
	Input        string // schema reference name
	Output       string // schema reference name
	Publish      string // persistent artifact name
	Instructions string // prompt reference name
	Mode         HumanMode
	MinAnswers   int // minimum human answers required
	Span         Span
}

// ---------------------------------------------------------------------------
// Nodes — Tool (direct execution, no LLM)
// ---------------------------------------------------------------------------

// ToolNodeDecl represents a `tool <name>:` node that executes
// a command directly without an LLM call.
type ToolNodeDecl struct {
	Name    string
	Command string // command to execute, may contain ${...} env refs
	Output  string // schema reference name
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
	Name   string
	Vars   *VarsBlock   // workflow-level variable declarations
	Entry  string       // entry node name
	Budget *BudgetBlock // execution limits (optional)
	Edges  []*Edge      // directed edges between nodes
	Span   Span
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
