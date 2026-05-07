// Package ir defines the canonical Intermediate Representation (IR)
// produced by compiling an AST. The IR is the sole source of truth
// for the runtime — it is execution-oriented, fully resolved, and
// independent of the DSL authoring surface.
package ir

import (
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/types"
)

// ---------------------------------------------------------------------------
// Workflow — compiled, execution-ready workflow
// ---------------------------------------------------------------------------

// Workflow is the top-level IR unit. It contains everything needed to
// execute a workflow: resolved nodes, edges, schemas, prompts, vars,
// loops and budget.
type Workflow struct {
	Name           string
	Entry          string                 // entry node ID
	Nodes          map[string]Node        // node ID → node
	Edges          []*Edge                // ordered list of edges
	Schemas        map[string]*Schema     // schema name → resolved schema
	Prompts        map[string]*Prompt     // prompt name → resolved prompt
	Vars           map[string]*Var        // var name → resolved variable
	Attachments    map[string]*Attachment // attachment name → resolved attachment
	Loops          map[string]*Loop       // loop name → loop definition
	Budget         *Budget                // workflow budget (nil if not set)
	Compaction     *Compaction            // workflow-level compaction overrides (nil = no override)
	MCP            *MCPConfig             // workflow-level MCP activation/filtering
	DefaultBackend string                 // workflow-level default backend (empty = not set)
	ToolPolicy     []string               // workflow-level tool policy patterns (nil = open)
	Interaction    *InteractionMode       // workflow-level default interaction mode (nil = not set)
	Worktree       string                 // "auto" runs in a per-run git worktree; "" or "none" runs in-place
	Sandbox        *SandboxSpec           // workflow-level sandbox spec (nil = inherit global / no sandbox)
	// MCPServers contains the explicit top-level declarations from the .iter file.
	MCPServers map[string]*MCPServer
	// ActiveMCPServers and ResolvedMCPServers are populated after project config
	// resolution, not by the compiler itself.
	ActiveMCPServers   []string
	ResolvedMCPServers map[string]*MCPServer
}

// ---------------------------------------------------------------------------
// Node — interface with concrete types per kind
// ---------------------------------------------------------------------------

// NodeKind discriminates the type of node.
type NodeKind int

const (
	NodeAgent   NodeKind = iota // LLM agent
	NodeJudge                   // verdict-producing LLM node
	NodeRouter                  // deterministic routing (no LLM)
	NodeHuman                   // human pause/resume
	NodeTool                    // direct command execution (no LLM)
	NodeCompute                 // deterministic expression evaluation (no LLM, no shell)
	NodeDone                    // terminal: success
	NodeFail                    // terminal: failure
)

func (k NodeKind) String() string {
	switch k {
	case NodeAgent:
		return "agent"
	case NodeJudge:
		return "judge"
	case NodeRouter:
		return "router"
	case NodeHuman:
		return "human"
	case NodeTool:
		return "tool"
	case NodeCompute:
		return "compute"
	case NodeDone:
		return "done"
	case NodeFail:
		return "fail"
	default:
		return "unknown"
	}
}

// Node is the IR node interface. Concrete types: AgentNode, JudgeNode,
// RouterNode, HumanNode, ToolNode, DoneNode, FailNode.
type Node interface {
	NodeID() string
	NodeKind() NodeKind
}

// BaseNode provides the common ID field embedded in every concrete node.
type BaseNode struct {
	ID string // unique identifier (= DSL name)
}

// NodeID implements Node.
func (b BaseNode) NodeID() string { return b.ID }

// ---------------------------------------------------------------------------
// Shared field groups (embedded in concrete node types)
// ---------------------------------------------------------------------------

// LLMFields groups fields shared by LLM-capable nodes (Agent, Judge, Router-LLM).
type LLMFields struct {
	Model           string // model identifier (env refs already noted)
	Backend         string // execution backend name (empty = direct LLM call)
	SystemPrompt    string // prompt reference name
	UserPrompt      string // prompt reference name
	MaxTokens       int    // per-node cap on output tokens (0 = backend default)
	ReasoningEffort string // reasoning effort level: "low", "medium", "high", "xhigh", "max"
	Readonly        bool   // when true, node is not considered mutating for workspace safety
}

// SchemaFields groups input/output schema references.
type SchemaFields struct {
	InputSchema  string // schema reference name (empty if not set)
	OutputSchema string // schema reference name (empty if not set)
}

// InteractionFields groups interaction-related fields.
type InteractionFields struct {
	Interaction       InteractionMode // interaction handling mode
	InteractionPrompt string          // prompt reference guiding LLM for llm_or_human decisions
	InteractionModel  string          // model for llm/llm_or_human modes (fallback to Model)
}

// ---------------------------------------------------------------------------
// Concrete node types
// ---------------------------------------------------------------------------

// AgentNode is an LLM agent node with tools, structured I/O, and optional delegation.
type AgentNode struct {
	BaseNode
	LLMFields
	SchemaFields
	InteractionFields
	MCP              *MCPConfig // node-level MCP activation/filtering
	ActiveMCPServers []string   // populated after project config resolution
	Publish          string     // persistent artifact name (empty if not set)
	Session          SessionMode
	Tools            []string // tool capability names
	ToolPolicy       []string // per-node tool policy patterns (nil = inherit workflow)
	ToolMaxSteps     int      // max tool-use iterations (0 = not set)
	AwaitMode        AwaitMode
	Compaction       *Compaction  // per-node compaction overrides (nil = inherit workflow)
	Sandbox          *SandboxSpec // node-level sandbox override (nil = inherit workflow)
}

// NodeKind implements Node.
func (n *AgentNode) NodeKind() NodeKind { return NodeAgent }

// JudgeNode is a verdict-producing LLM node (typically no tools).
type JudgeNode struct {
	BaseNode
	LLMFields
	SchemaFields
	InteractionFields
	MCP              *MCPConfig
	ActiveMCPServers []string
	Publish          string
	Session          SessionMode
	Tools            []string
	ToolPolicy       []string // per-node tool policy patterns (nil = inherit workflow)
	ToolMaxSteps     int
	AwaitMode        AwaitMode
	Compaction       *Compaction  // per-node compaction overrides (nil = inherit workflow)
	Sandbox          *SandboxSpec // node-level sandbox override (nil = inherit workflow)
}

// NodeKind implements Node.
func (n *JudgeNode) NodeKind() NodeKind { return NodeJudge }

// RouterNode is a routing node with 4 modes: fan_out_all, condition, round_robin, llm.
// LLMFields are only populated when RouterMode == RouterLLM.
type RouterNode struct {
	BaseNode
	LLMFields              // only populated for RouterLLM mode
	RouterMode  RouterMode // fan_out_all, condition, round_robin, or llm
	RouterMulti bool       // LLM router: select multiple targets (default: one)
}

// NodeKind implements Node.
func (n *RouterNode) NodeKind() NodeKind { return NodeRouter }

// HumanNode is a human pause/resume node.
type HumanNode struct {
	BaseNode
	SchemaFields
	InteractionFields
	Publish      string
	MinAnswers   int    // minimum answers required
	Instructions string // prompt reference for human instructions
	Model        string // model for LLM-based interaction modes
	SystemPrompt string // prompt reference for LLM-based interaction modes
	AwaitMode    AwaitMode
}

// NodeKind implements Node.
func (n *HumanNode) NodeKind() NodeKind { return NodeHuman }

// ToolNode executes a shell command directly (no LLM).
type ToolNode struct {
	BaseNode
	SchemaFields
	Command     string // command to execute, may contain {{...}} template refs
	CommandRefs []*Ref // parsed template references in Command (resolved at runtime)
	Session     SessionMode
	AwaitMode   AwaitMode
	Sandbox     *SandboxSpec // node-level sandbox override (nil = inherit workflow)
}

// NodeKind implements Node.
func (n *ToolNode) NodeKind() NodeKind { return NodeTool }

// ComputeNode evaluates a set of named expressions over the standard
// reference namespaces (vars, input, outputs, artifacts, loop, run) and
// returns them as a structured output. It performs no LLM call and no
// shell-out; expressions are parsed at compile time and re-evaluated on
// each visit.
type ComputeNode struct {
	BaseNode
	SchemaFields
	Exprs     []*ComputeExpr // ordered field-name → parsed AST pairs
	AwaitMode AwaitMode
}

// ComputeExpr is a single field expression in a ComputeNode.
type ComputeExpr struct {
	Key string    // output field name
	AST *expr.AST // parsed expression
	Raw string    // original source for diagnostics / unparse
}

// NodeKind implements Node.
func (n *ComputeNode) NodeKind() NodeKind { return NodeCompute }

// DoneNode is a terminal success node.
type DoneNode struct {
	BaseNode
	AwaitMode AwaitMode // convergence strategy when multiple branches arrive
}

// NodeKind implements Node.
func (n *DoneNode) NodeKind() NodeKind { return NodeDone }

// FailNode is a terminal failure node.
type FailNode struct {
	BaseNode
	AwaitMode AwaitMode // convergence strategy when multiple branches arrive
}

// NodeKind implements Node.
func (n *FailNode) NodeKind() NodeKind { return NodeFail }

// ---------------------------------------------------------------------------
// Node field accessors — exported helpers that extract fields from concrete
// node types via the Node interface. Consumers should use these instead of
// writing their own type switches.
// ---------------------------------------------------------------------------

// NodeAwaitMode returns the AwaitMode for nodes that support it, or AwaitNone.
func NodeAwaitMode(n Node) AwaitMode {
	switch n := n.(type) {
	case *AgentNode:
		return n.AwaitMode
	case *JudgeNode:
		return n.AwaitMode
	case *HumanNode:
		return n.AwaitMode
	case *ToolNode:
		return n.AwaitMode
	case *ComputeNode:
		return n.AwaitMode
	case *DoneNode:
		return n.AwaitMode
	case *FailNode:
		return n.AwaitMode
	}
	return AwaitNone
}

// NodeOutputSchema returns the OutputSchema for nodes that support it, or "".
func NodeOutputSchema(n Node) string {
	switch n := n.(type) {
	case *AgentNode:
		return n.OutputSchema
	case *JudgeNode:
		return n.OutputSchema
	case *HumanNode:
		return n.OutputSchema
	case *ToolNode:
		return n.OutputSchema
	case *ComputeNode:
		return n.OutputSchema
	}
	return ""
}

// NodeInputSchema returns the InputSchema for nodes that support it, or "".
func NodeInputSchema(n Node) string {
	switch n := n.(type) {
	case *AgentNode:
		return n.InputSchema
	case *JudgeNode:
		return n.InputSchema
	case *HumanNode:
		return n.InputSchema
	case *ToolNode:
		return n.InputSchema
	case *ComputeNode:
		return n.InputSchema
	}
	return ""
}

// NodePublish returns the Publish field for nodes that support it, or "".
func NodePublish(n Node) string {
	switch n := n.(type) {
	case *AgentNode:
		return n.Publish
	case *JudgeNode:
		return n.Publish
	case *HumanNode:
		return n.Publish
	}
	return ""
}

// NodeInteraction returns the Interaction field for nodes that support it, or InteractionNone.
func NodeInteraction(n Node) InteractionMode {
	switch n := n.(type) {
	case *AgentNode:
		return n.Interaction
	case *JudgeNode:
		return n.Interaction
	case *HumanNode:
		return n.Interaction
	}
	return InteractionNone
}

// NodeActiveMCPServers returns the ActiveMCPServers list for nodes that support it, or nil.
func NodeActiveMCPServers(n Node) []string {
	switch n := n.(type) {
	case *AgentNode:
		return n.ActiveMCPServers
	case *JudgeNode:
		return n.ActiveMCPServers
	}
	return nil
}

// IsTerminalNode returns true if the node is a DoneNode or FailNode.
func IsTerminalNode(n Node) bool {
	switch n.(type) {
	case *DoneNode, *FailNode:
		return true
	}
	return false
}

// NodePromptRefs returns all prompt reference names used by a node.
func NodePromptRefs(node Node) []string {
	var refs []string
	// Extract LLMFields prompts if applicable.
	switch n := node.(type) {
	case *AgentNode:
		refs = appendLLMPromptRefs(refs, &n.LLMFields)
		if n.InteractionPrompt != "" {
			refs = append(refs, n.InteractionPrompt)
		}
	case *JudgeNode:
		refs = appendLLMPromptRefs(refs, &n.LLMFields)
		if n.InteractionPrompt != "" {
			refs = append(refs, n.InteractionPrompt)
		}
	case *RouterNode:
		refs = appendLLMPromptRefs(refs, &n.LLMFields)
	case *HumanNode:
		if n.SystemPrompt != "" {
			refs = append(refs, n.SystemPrompt)
		}
		if n.InteractionPrompt != "" {
			refs = append(refs, n.InteractionPrompt)
		}
		if n.Instructions != "" {
			refs = append(refs, n.Instructions)
		}
	}
	return refs
}

// appendLLMPromptRefs appends SystemPrompt and UserPrompt from LLMFields if set.
func appendLLMPromptRefs(refs []string, f *LLMFields) []string {
	if f.SystemPrompt != "" {
		refs = append(refs, f.SystemPrompt)
	}
	if f.UserPrompt != "" {
		refs = append(refs, f.UserPrompt)
	}
	return refs
}

// ---------------------------------------------------------------------------
// Session, Router, Await, Interaction modes (mirrored from AST for IR independence)
// ---------------------------------------------------------------------------

type SessionMode = types.SessionMode

const (
	SessionFresh         = types.SessionFresh
	SessionInherit       = types.SessionInherit
	SessionArtifactsOnly = types.SessionArtifactsOnly
	SessionFork          = types.SessionFork
)

type RouterMode = types.RouterMode

const (
	RouterFanOutAll  = types.RouterFanOutAll
	RouterCondition  = types.RouterCondition
	RouterRoundRobin = types.RouterRoundRobin
	RouterLLM        = types.RouterLLM
)

// AwaitMode determines how a convergence point handles multiple incoming branches.
type AwaitMode = types.AwaitMode

const (
	AwaitNone       = types.AwaitNone
	AwaitWaitAll    = types.AwaitWaitAll
	AwaitBestEffort = types.AwaitBestEffort
)

// InteractionMode controls how a node handles user interaction requests.
// Available on agent, judge, and human nodes.
type InteractionMode = types.InteractionMode

const (
	InteractionNone       = types.InteractionNone
	InteractionHuman      = types.InteractionHuman
	InteractionLLM        = types.InteractionLLM
	InteractionLLMOrHuman = types.InteractionLLMOrHuman
)

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

// MCPServer is a reusable MCP server declaration or resolved catalog entry.
type MCPServer struct {
	Name      string
	Transport MCPTransport
	Command   string
	Args      []string
	URL       string
	Headers   map[string]string
	Auth      *MCPAuth
}

// MCPAuth describes how to authenticate against an MCP server.
// Only the OAuth2 authorization-code + PKCE flow is wired today;
// `Type` is reserved for future schemes (bearer, mTLS, ...).
type MCPAuth struct {
	// Type is the authentication scheme. The only supported value is
	// "oauth2"; other values produce a C-code diagnostic.
	Type string

	// AuthURL is the OAuth authorization endpoint the user's browser
	// visits to consent.
	AuthURL string

	// TokenURL is the back-channel endpoint that issues access and
	// refresh tokens.
	TokenURL string

	// RevokeURL is the optional RFC 7009 revocation endpoint.
	RevokeURL string

	// ClientID is the OAuth client identifier registered with the
	// provider.
	ClientID string

	// Scopes is the set of OAuth scopes requested at authorization.
	Scopes []string
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
	// node's output schema. Negated inverts the check. Mutually exclusive
	// with Expression: the compiler chooses one form per edge.
	Condition string
	Negated   bool

	// Expression (optional). When non-nil, this parsed expression replaces
	// Condition/Negated and is evaluated against the source node's output
	// (exposed as `input`/`outputs.<self>`), the run vars, artifacts, and
	// loop/run namespaces.
	Expression    *expr.AST
	ExpressionSrc string // original source string preserved for unparse/debug

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

// IsConditional reports whether an edge carries any predicate (simple
// boolean field or parsed expression). Used by validators and the runtime
// to distinguish guarded edges from unconditional fallbacks.
func (e *Edge) IsConditional() bool {
	if e == nil {
		return false
	}
	return e.Condition != "" || e.Expression != nil
}

// ---------------------------------------------------------------------------
// Ref — normalized reference expression
// ---------------------------------------------------------------------------

// RefKind discriminates the namespace of a reference.
type RefKind int

const (
	RefVars        RefKind = iota // {{vars.x}}
	RefInput                      // {{input.field}}
	RefOutputs                    // {{outputs.node}} or {{outputs.node.field}}
	RefArtifacts                  // {{artifacts.name}}
	RefAttachments                // {{attachments.name[.path|.url|.mime|.size|.sha256]}}
	RefLoop                       // {{loop.<name>.iteration}} / .max / .previous_output[.field]
	RefRun                        // {{run.id}}
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
	case RefAttachments:
		return "attachments"
	case RefLoop:
		return "loop"
	case RefRun:
		return "run"
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
// Attachment — resolved workflow attachment (binary input)
// ---------------------------------------------------------------------------

// Attachment is a resolved attachment declaration. The bytes themselves
// are persisted by the run store; this struct only carries the schema
// (name, type, validation hints) consumed by the parser, runtime and
// editor frontend.
type Attachment struct {
	Name        string
	Type        AttachmentType
	Required    bool
	AcceptMIME  []string // nil = inherit server allowlist
	Description string
}

// AttachmentType enumerates the supported attachment binary types.
type AttachmentType int

const (
	AttachmentFile AttachmentType = iota
	AttachmentImage
)

func (a AttachmentType) String() string {
	switch a {
	case AttachmentFile:
		return "file"
	case AttachmentImage:
		return "image"
	}
	return "unknown"
}

// AttachmentSubFields enumerates the sub-fields that may appear after
// `attachments.<name>.` in a template reference.
//
// Example: `{{attachments.logo.url}}` has SubField "url".
var AttachmentSubFields = map[string]struct{}{
	"path":   {},
	"url":    {},
	"mime":   {},
	"size":   {},
	"sha256": {},
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

// ---------------------------------------------------------------------------
// Compaction — session compaction overrides
// ---------------------------------------------------------------------------

// Compaction overrides the default compaction behavior. Threshold is
// applied as a fraction of the model's context window (0 means inherit).
// PreserveRecent caps the number of recent messages kept verbatim
// (0 means inherit).
type Compaction struct {
	Threshold      float64 // 0 = inherit (env / 0.85 default)
	PreserveRecent int     // 0 = inherit (default 4)
}
