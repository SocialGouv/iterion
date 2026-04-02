// Package ir defines the canonical Intermediate Representation (IR)
// produced by compiling an AST. The IR is the sole source of truth
// for the runtime — it is execution-oriented, fully resolved, and
// independent of the DSL authoring surface.
package ir

// ---------------------------------------------------------------------------
// Workflow — compiled, execution-ready workflow
// ---------------------------------------------------------------------------

// Workflow is the top-level IR unit. It contains everything needed to
// execute a workflow: resolved nodes, edges, schemas, prompts, vars,
// loops and budget.
type Workflow struct {
	Name    string
	Entry   string             // entry node ID
	Nodes   map[string]*Node   // node ID → node
	Edges   []*Edge            // ordered list of edges
	Schemas map[string]*Schema // schema name → resolved schema
	Prompts map[string]*Prompt // prompt name → resolved prompt
	Vars    map[string]*Var    // var name → resolved variable
	Loops   map[string]*Loop   // loop name → loop definition
	Budget  *Budget            // workflow budget (nil if not set)
	MCP     *MCPConfig         // workflow-level MCP activation/filtering
	// MCPServers contains the explicit top-level declarations from the .iter file.
	MCPServers map[string]*MCPServer
	// ActiveMCPServers and ResolvedMCPServers are populated after project config
	// resolution, not by the compiler itself.
	ActiveMCPServers   []string
	ResolvedMCPServers map[string]*MCPServer
}

// ---------------------------------------------------------------------------
// Node — unified node with a kind discriminator
// ---------------------------------------------------------------------------

// NodeKind discriminates the type of node.
type NodeKind int

const (
	NodeAgent  NodeKind = iota // LLM agent
	NodeJudge                  // verdict-producing LLM node
	NodeRouter                 // deterministic routing (no LLM)
	NodeJoin                   // branch aggregation
	NodeHuman                  // human pause/resume
	NodeTool                   // direct command execution (no LLM)
	NodeDone                   // terminal: success
	NodeFail                   // terminal: failure
)

func (k NodeKind) String() string {
	switch k {
	case NodeAgent:
		return "agent"
	case NodeJudge:
		return "judge"
	case NodeRouter:
		return "router"
	case NodeJoin:
		return "join"
	case NodeHuman:
		return "human"
	case NodeTool:
		return "tool"
	case NodeDone:
		return "done"
	case NodeFail:
		return "fail"
	default:
		return "unknown"
	}
}

// Node is the unified IR node. Fields are populated according to Kind.
type Node struct {
	ID   string // unique identifier (= DSL name)
	Kind NodeKind

	// --- Agent / Judge fields ---
	Model    string     // model identifier (env refs already noted)
	Delegate string     // delegation backend name (empty = direct LLM call)
	MCP      *MCPConfig // node-level MCP activation/filtering
	// ActiveMCPServers is populated after project config resolution.
	ActiveMCPServers []string
	InputSchema      string      // schema reference name (empty if not set)
	OutputSchema     string      // schema reference name (empty if not set)
	Publish          string      // persistent artifact name (empty if not set)
	SystemPrompt     string      // prompt reference name
	UserPrompt       string      // prompt reference name
	Session          SessionMode // session strategy
	Tools            []string    // tool capability names
	ToolMaxSteps     int         // max tool-use iterations (0 = not set)

	// --- Router fields ---
	RouterMode  RouterMode // fan_out_all, condition, round_robin, or llm
	RouterMulti bool       // LLM router: select multiple targets (default: one)

	// --- Join fields ---
	JoinStrategy JoinStrategy // wait_all or best_effort
	Require      []string     // node IDs to wait for
	JoinOutput   string       // schema reference for join output

	// --- Human fields ---
	HumanMode    HumanMode // pause strategy
	MinAnswers   int       // minimum answers required
	Instructions string    // prompt reference for human instructions

	// --- Tool node fields ---
	Command string // command to execute
}

// ---------------------------------------------------------------------------
// Session, Router, Join, Human modes (mirrored from AST for IR independence)
// ---------------------------------------------------------------------------

type SessionMode int

const (
	SessionFresh SessionMode = iota
	SessionInherit
	SessionArtifactsOnly
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

type RouterMode int

const (
	RouterFanOutAll RouterMode = iota
	RouterCondition
	RouterRoundRobin
	RouterLLM
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

type JoinStrategy int

const (
	JoinWaitAll JoinStrategy = iota
	JoinBestEffort
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

type HumanMode int

const (
	HumanPauseUntilAnswers HumanMode = iota
	HumanAutoAnswer
	HumanAutoOrPause
)

func (hm HumanMode) String() string {
	switch hm {
	case HumanPauseUntilAnswers:
		return "pause_until_answers"
	case HumanAutoAnswer:
		return "auto_answer"
	case HumanAutoOrPause:
		return "auto_or_pause"
	default:
		return "unknown"
	}
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

// MCPServer is a reusable MCP server declaration or resolved catalog entry.
type MCPServer struct {
	Name      string
	Transport MCPTransport
	Command   string
	Args      []string
	URL       string
	Headers   map[string]string
}

// MCPConfig represents workflow-level or node-level MCP activation/filtering.
type MCPConfig struct {
	AutoloadProject *bool
	Inherit         *bool
	Servers         []string
	Disable         []string
}

// ---------------------------------------------------------------------------
// Edge — compiled directed transition
// ---------------------------------------------------------------------------

// Edge represents a directed transition between two nodes, with optional
// condition, loop reference, and data mappings.
type Edge struct {
	From string // source node ID
	To   string // target node ID

	// Condition (optional). Condition is a field name from the source
	// node's output schema. Negated inverts the check.
	Condition string
	Negated   bool

	// Loop reference (optional). LoopName references a Loop in Workflow.Loops.
	LoopName string

	// Data mappings (optional). Each entry maps a target input field
	// to a resolved reference expression.
	With []*DataMapping
}

// DataMapping maps a target input field key to a parsed reference.
type DataMapping struct {
	Key  string // target input field name
	Refs []*Ref // parsed references from the template value
	Raw  string // original template string for debugging
}

// ---------------------------------------------------------------------------
// Ref — normalized reference expression
// ---------------------------------------------------------------------------

// RefKind discriminates the namespace of a reference.
type RefKind int

const (
	RefVars      RefKind = iota // {{vars.x}}
	RefInput                    // {{input.field}}
	RefOutputs                  // {{outputs.node}} or {{outputs.node.field}}
	RefArtifacts                // {{artifacts.name}}
)

func (rk RefKind) String() string {
	switch rk {
	case RefVars:
		return "vars"
	case RefInput:
		return "input"
	case RefOutputs:
		return "outputs"
	case RefArtifacts:
		return "artifacts"
	default:
		return "unknown"
	}
}

// Ref is a single normalized reference extracted from a template expression.
// Examples:
//
//	{{vars.x}}                → Kind=RefVars, Path=["x"]
//	{{outputs.node}}          → Kind=RefOutputs, Path=["node"]
//	{{outputs.node.field}}    → Kind=RefOutputs, Path=["node","field"]
//	{{input.field}}           → Kind=RefInput, Path=["field"]
//	{{artifacts.name}}        → Kind=RefArtifacts, Path=["name"]
type Ref struct {
	Kind RefKind
	Path []string // dotted path segments after the namespace
	Raw  string   // original template expression, e.g. "{{outputs.node.field}}"
}

// ---------------------------------------------------------------------------
// Schema — resolved schema definition
// ---------------------------------------------------------------------------

// Schema is a resolved schema with its fields.
type Schema struct {
	Name   string
	Fields []*SchemaField
}

// SchemaField is a single field in a schema.
type SchemaField struct {
	Name       string
	Type       FieldType
	EnumValues []string // non-nil only if enum constraint present
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
// Prompt — resolved prompt with parsed template references
// ---------------------------------------------------------------------------

// Prompt is a resolved prompt declaration. TemplateRefs contains all
// references extracted from the prompt body.
type Prompt struct {
	Name         string
	Body         string // raw template text
	TemplateRefs []*Ref // references found in the body
}

// ---------------------------------------------------------------------------
// Var — resolved workflow variable
// ---------------------------------------------------------------------------

// Var is a resolved workflow variable with its type and optional default.
type Var struct {
	Name       string
	Type       VarType
	HasDefault bool
	Default    interface{} // string, int64, float64, or bool
}

// VarType enumerates variable types.
type VarType int

const (
	VarString VarType = iota
	VarBool
	VarInt
	VarFloat
	VarJSON
	VarStringArray
)

func (vt VarType) String() string {
	switch vt {
	case VarString:
		return "string"
	case VarBool:
		return "bool"
	case VarInt:
		return "int"
	case VarFloat:
		return "float"
	case VarJSON:
		return "json"
	case VarStringArray:
		return "string[]"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Loop — named bounded loop definition
// ---------------------------------------------------------------------------

// Loop defines a named bounded loop. Multiple edges can reference
// the same loop; the runtime shares a single counter per loop name.
type Loop struct {
	Name          string
	MaxIterations int
}

// ---------------------------------------------------------------------------
// Budget — execution limits
// ---------------------------------------------------------------------------

// Budget defines execution limits for a workflow.
type Budget struct {
	MaxParallelBranches int
	MaxDuration         string // e.g. "60m"
	MaxCostUSD          float64
	MaxTokens           int
	MaxIterations       int
}
