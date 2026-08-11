// Package ir defines the canonical Intermediate Representation (IR)
// produced by compiling an AST. The IR is the sole source of truth
// for the runtime — it is execution-oriented, fully resolved, and
// independent of the DSL authoring surface.
package ir

import (
	"time"

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
	Name            string
	Entry           string                 // entry node ID
	Nodes           map[string]Node        // node ID → node
	Edges           []*Edge                // ordered list of edges
	Schemas         map[string]*Schema     // schema name → resolved schema
	Prompts         map[string]*Prompt     // prompt name → resolved prompt
	Vars            map[string]*Var        // var name → resolved variable
	Secrets         map[string]*Secret     // secret name → resolved secret declaration
	Presets         map[string]Preset      // preset name → resolved preset values (var name → typed value)
	Attachments     map[string]*Attachment // attachment name → resolved attachment
	Loops           map[string]*Loop       // loop name → loop definition
	Foreaches       map[string]*Foreach    // foreach name → sequential-iteration definition
	Budget          *Budget                // workflow budget (nil if not set)
	Resources       map[string]int         // named counting semaphores (resource name → capacity); nil = none
	ResourceMembers map[string][]string    // resource name → named-instance lease pool (capacity = len); nil = counting-only
	Compaction      *Compaction            // workflow-level compaction overrides (nil = no override)
	MCP             *MCPConfig             // workflow-level MCP activation/filtering
	DefaultBackend  string                 // workflow-level default backend (empty = not set)
	ToolPolicy      []string               // workflow-level tool policy patterns (nil = open)
	Capabilities    []string               // workflow-level default host capabilities (nil = inherit none)
	Skills          []string               // workflow-level default skill-library references (nil = none)
	Interaction     *InteractionMode       // workflow-level default interaction mode (nil = not set)
	Worktree        string                 // "auto" runs in a per-run git worktree; "" or "none" runs in-place
	Compress        string                 // compress output-compression mode: on|ultra|off ("" = unset)
	AutoMemory      string                 // backend auto-memory (MEMORY.md) switch: on|off ("" = unset → off)
	// LoopBudgetGuard switches the back-edge affordability guard — the
	// refusal to start a loop iteration the budget cannot fund: on|off
	// ("" = unset → ITERION_LOOP_BUDGET_GUARD → on).
	LoopBudgetGuard string
	Permission      string       // permission gate mode: off|ask|deny ("" = unset → off)
	PermissionAllow []string     // allow rules (Claude-Code `Tool(pattern)` syntax, e.g. "Bash(go test:*)")
	PermissionAsk   []string     // ask rules
	PermissionDeny  []string     // deny rules
	Sandbox         *SandboxSpec // workflow-level sandbox spec (nil = inherit global / no sandbox)
	// Cursors map of cursor name → resolved definition. Populated from
	// top-level `cursor NAME:` declarations. Agent/judge `cursors:`
	// invocations are resolved against this map at runtime.
	Cursors map[string]*CursorDef
	// Supervisors are top-level `supervisor NAME:` declarations: concurrent
	// node-watchers the engine spawns at run start (not graph nodes). See
	// docs/supervisors.md.
	Supervisors []*Supervisor
	// MCPServers contains the explicit top-level declarations from the .bot file.
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
	NodeAgent        NodeKind = iota // LLM agent
	NodeJudge                        // verdict-producing LLM node
	NodeRouter                       // deterministic routing (no LLM)
	NodeHuman                        // human pause/resume
	NodeTool                         // direct command execution (no LLM)
	NodeCompute                      // deterministic expression evaluation (no LLM, no shell)
	NodeEmit                         // publishes a run-scoped event (no LLM, no shell)
	NodeWait                         // blocks until a run-scoped event (no LLM, no shell)
	NodeAwaitAnswers                 // blocks until pending async human questions are answered (no LLM, no shell)
	NodeSubbot                       // runs another .bot as a nested run
	NodeDone                         // terminal: success
	NodeFail                         // terminal: failure
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
	case NodeEmit:
		return "emit"
	case NodeWait:
		return "wait"
	case NodeAwaitAnswers:
		return "await_answers"
	case NodeSubbot:
		return "subbot"
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

// NodeNeeds returns the resource names a node acquires before running (the
// `needs:` property). Nodes without a `needs:` declaration — and node kinds
// that don't support it (human/compute/done/fail) — return nil.
func NodeNeeds(n Node) []string {
	switch x := n.(type) {
	case *AgentNode:
		return x.Needs
	case *JudgeNode:
		return x.Needs
	case *RouterNode:
		return x.Needs
	case *ToolNode:
		return x.Needs
	case *SubbotNode:
		return x.Needs
	default:
		return nil
	}
}

// BaseNode provides the common fields embedded in every concrete node.
type BaseNode struct {
	ID          string // unique identifier (= DSL name)
	Description string // optional human-readable label (surfaced in the run console)
}

// NodeID implements Node.
func (b BaseNode) NodeID() string { return b.ID }

// NodeDescription returns the node's optional human-readable label.
// Promoted onto every concrete node type via embedding.
func (b BaseNode) NodeDescription() string { return b.Description }

// ---------------------------------------------------------------------------
// Shared field groups (embedded in concrete node types)
// ---------------------------------------------------------------------------

// LLMFields groups fields shared by LLM-capable nodes (Agent, Judge, Router-LLM).
type LLMFields struct {
	Model           string   // model identifier (env refs already noted)
	Backend         string   // execution backend name (empty = direct LLM call); may contain ${VAR} env refs
	Provider        string   // credential routing hint(s): single ("anthropic"/"zai"/"openai"/""=auto) or an ordered fallback chain ("anthropic,zai,openai"); may contain ${VAR} env refs
	Command         string   // per-node CLI binary override, honored by claude_code; may contain ${VAR}
	SystemPrompt    string   // prompt reference name
	UserPrompt      string   // prompt reference name
	MaxTokens       int      // per-node cap on output tokens (0 = backend default)
	ReasoningEffort string   // reasoning effort level: "low", "medium", "high", "xhigh", "max"
	Timeout         string   // per-node wall-clock timeout as a Go duration ("20m", "1200s"); empty = no per-node bound; may contain ${VAR} env refs
	Readonly        bool     // when true, node is not considered mutating for workspace safety
	FullAccess      bool     // when true, lift the codex backend sandbox to danger-full-access (network + out-of-workspace writes); off by default; other backends ignore it
	Images          []string // node-level `images:` — input image paths (templated) forwarded to the codex backend as `-i` for image-to-image; other backends ignore it
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
	PublishLabels    []string   // DSL artifact_labels: applied to the published artifact
	Session          SessionMode
	Tools            []string // tool capability names
	ToolPolicy       []string // per-node tool policy patterns (nil = inherit workflow)
	Capabilities     []string // host-side capabilities (e.g. board.create); nil = inherit workflow
	Skills           []string // skill-library references; resolved names mirrored into .claude/skills/ (nil = inherit workflow)
	ToolMaxSteps     int      // max tool-use iterations (0 = not set)
	AwaitMode        AwaitMode
	Compaction       *Compaction  // per-node compaction overrides (nil = inherit workflow)
	Memory           *Memory      // per-node workspace memory opt-in (nil = disabled)
	Sandbox          *SandboxSpec // node-level sandbox override (nil = inherit workflow)
	Cursors          *CursorInvocation
	Fallbacks        []Fallback
	Compress         string   // compress output-compression mode: on|ultra|off ("" = inherit)
	AutoMemory       string   // backend auto-memory (MEMORY.md) switch: on|off ("" = inherit workflow)
	Permission       string   // permission gate mode override: off|ask|deny ("" = inherit workflow)
	Needs            []string // resource names this node acquires before running (counting semaphores)
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
	Capabilities     []string // host-side capabilities (e.g. board.read); nil = inherit workflow
	Skills           []string // skill-library references; resolved names mirrored into .claude/skills/ (nil = inherit workflow)
	ToolMaxSteps     int
	AwaitMode        AwaitMode
	Compaction       *Compaction  // per-node compaction overrides (nil = inherit workflow)
	Memory           *Memory      // per-node workspace memory opt-in (nil = disabled)
	Sandbox          *SandboxSpec // node-level sandbox override (nil = inherit workflow)
	Cursors          *CursorInvocation
	Fallbacks        []Fallback
	Compress         string   // compress output-compression mode: on|ultra|off ("" = inherit)
	AutoMemory       string   // backend auto-memory (MEMORY.md) switch: on|off ("" = inherit workflow)
	Permission       string   // permission gate mode override: off|ask|deny ("" = inherit workflow)
	Needs            []string // resource names this node acquires before running (counting semaphores)
}

// NodeKind implements Node.
func (n *JudgeNode) NodeKind() NodeKind { return NodeJudge }

// RouterNode is a routing node with 4 modes: fan_out_all, condition, round_robin, llm.
// LLMFields are only populated when RouterMode == RouterLLM.
type RouterNode struct {
	BaseNode
	LLMFields              // only populated for RouterLLM mode
	RouterMode  RouterMode // fan_out_all, condition, round_robin, llm, or fan_out_each
	RouterMulti bool       // LLM router: select multiple targets (default: one)

	// Data-driven fan-out (RouterFanOutEach only). At runtime the engine
	// resolves Over to an array and re-executes the single outgoing
	// template subgraph once per element, binding the element (and its
	// index) onto this router's per-branch output under ItemBinding /
	// "item" / "index" / "count".
	Over        string // raw array-source template, e.g. "{{outputs.decompose.tickets}}"
	OverRefs    []*Ref // parsed refs from Over (resolved at runtime)
	ItemBinding string // per-item binding name (default "item")

	// Optional DAG scheduling (RouterFanOutEach only). When KeyField is set,
	// each item is identified by item[KeyField] and depends on the ids listed
	// in item[DepsField]; the engine schedules branches in topological order,
	// running independent items in parallel (bounded by max_parallel_branches)
	// and holding a dependent until all its deps have finished. Empty deps =>
	// fully parallel (identical to plain fan_out_each); a linear chain => fully
	// sequential. Empty KeyField => no DAG, plain fan-out.
	KeyField  string // item field holding its unique id
	DepsField string // item field holding the array of ids it depends on

	Needs []string // resource names this node acquires before running (counting semaphores)
}

// NodeKind implements Node.
func (n *RouterNode) NodeKind() NodeKind { return NodeRouter }

// HumanNode is a human pause/resume node.
type HumanNode struct {
	BaseNode
	SchemaFields
	InteractionFields
	Publish       string
	PublishLabels []string // DSL artifact_labels: applied to the published artifact
	MinAnswers    int      // minimum answers required
	Instructions  string   // prompt reference for human instructions
	Model         string   // model for LLM-based interaction modes
	SystemPrompt  string   // prompt reference for LLM-based interaction modes
	AwaitMode     AwaitMode

	// Review-gate fields (interaction: review). The gate runs a
	// companion-driven multi-turn dialogue that walks the human through
	// testing the change, then squash-merges the worktree during the pause.
	ReviewURL     string // raw template (e.g. "{{outputs.provision.url}}") for the review env; resolved at runtime
	ReviewURLRefs []*Ref // parsed refs in ReviewURL (compile-time validation)
	Posture       string // PostureHumanRequired (default) | PostureAgentVerdictOK
	MergeStrategy string // "squash" (default) | "merge"
	MergeInto     string // "current" (default) | "none" | <branch>
	MaxTurns      int    // dialogue asymptote backstop (0 → DefaultReviewMaxTurns)
}

// NodeKind implements Node.
func (n *HumanNode) NodeKind() NodeKind { return NodeHuman }

// ToolNode executes a shell command or higher-level script directly (no LLM).
//
// A node carries EITHER Command (raw shell snippet, executed via `sh -c`)
// OR Script (interpreter snippet, written to a temp file and executed
// via the interpreter named by Language). Setting both is a compile-time
// validation error; setting neither is also an error.
type ToolNode struct {
	BaseNode
	SchemaFields
	Command       string   // command to execute, may contain {{...}} template refs
	CommandRefs   []*Ref   // parsed template references in Command (resolved at runtime)
	Script        string   // script body (interpreter snippet); mutually exclusive with Command
	ScriptRefs    []*Ref   // parsed template references in Script
	Language      string   // interpreter for Script: "js"|"py"|"sh"|"bash" (empty defaults to "sh")
	Publish       string   // persistent artifact name (empty = not published)
	PublishLabels []string // DSL artifact_labels: applied to the published artifact
	Session       SessionMode
	AwaitMode     AwaitMode
	Sandbox       *SandboxSpec // node-level sandbox override (nil = inherit workflow)
	Compress      string       // compress output-compression mode: on|ultra|off ("" = inherit)
	Permission    string       // permission gate mode override: off|ask|deny ("" = inherit workflow)

	// Verified Action quad (ADR-044). All optional; a node with an empty
	// Postcondition runs the recipe with exit-code = success (unchanged).
	Goal          string        // natural-language outcome (fuel for recovery rungs)
	Postcondition string        // cheap deterministic check (shell, exit 0 = met); single source of truth
	PostcondRefs  []*Ref        // parsed template refs in Postcondition (resolved at runtime)
	Policy        string        // "required" | "recover" | "best_effort" (defaulted at compile time)
	Recovery      *RecoverySpec // bounded recovery rung config (nil = no rungs)

	Needs []string // resource names this node acquires before running (counting semaphores)

	// ParallelSafe asserts that concurrent fan-out replays of this tool write
	// only to disjoint, item-keyed targets and never race one another on the
	// shared workspace, letting the workspace-safety guard fan the tool out in
	// parallel (max_parallel_branches > 1). It is scoped to a fan_out_each
	// template — the one place a single node is replayed over items; it has no
	// effect on a static fan_out_all / llm-router (distinct branches, no item
	// key). Unlike a subbot's Isolated, the tool still writes to the shared
	// workspace — it just partitions those writes; unlike an agent/judge
	// Readonly, it is not read-only. Default false = conservatively mutating.
	ParallelSafe bool
}

// Verified Action policy values (ADR-044).
const (
	PolicyRequired   = "required"    // fail (resumable) if postcondition unmet; no recovery rungs
	PolicyRecover    = "recover"     // run recovery rungs, then fail if still unmet
	PolicyBestEffort = "best_effort" // warn + continue if postcondition unmet
)

// RecoverySpec is the compiled bound on a Verified Action node's recovery
// rungs (ADR-044). Rungs only run under Policy == "recover".
type RecoverySpec struct {
	MaxRepairAttempts int      // rung 3 (self-repair) bound
	MaxAgentAttempts  int      // rung 4 (agent recovery) bound; 0 = OFF
	Model             string   // recovery LLM spec (empty = node/workflow default)
	AgentTools        []string // rung-4 toolset (empty = node capabilities)
}

// SubbotNode runs another .bot as a nested run. The runtime resolves With into
// the child's input vars, invokes the host-supplied SubbotRunner (which
// compiles + runs the child in the same store), and maps the child's terminal
// output to outputs.<subbot>.<field>. The child is a real run, so unlike a
// fan-out branch it may contain loops.
type SubbotNode struct {
	BaseNode
	Source       string         // path/ref to the child .bot (relative to the parent workdir)
	With         []*DataMapping // vars passed to the child run (key = child var name)
	OutputSchema string         // schema reference describing the child's terminal output
	Needs        []string       // resource names acquired before running the child
	// Isolated asserts the child does NOT mutate the parent's shared workspace,
	// letting the workspace-safety guard fan this subbot out in parallel. Mirror
	// of AgentNode/JudgeNode Readonly. Default false = conservatively mutating.
	Isolated bool
}

// NodeKind implements Node.
func (n *SubbotNode) NodeKind() NodeKind { return NodeSubbot }

// NodeKind implements Node.
func (n *ToolNode) NodeKind() NodeKind { return NodeTool }

// GetPermission returns the node-level permission gate mode override
// ("" = inherit workflow). ToolNode does not implement LLMNode, but
// exposes this accessor for symmetry with AgentNode/JudgeNode.
func (n *ToolNode) GetPermission() string { return n.Permission }

// ComputeNode evaluates a set of named expressions over the standard
// reference namespaces (vars, input, outputs, artifacts, loop, run) and
// returns them as a structured output. It performs no LLM call and no
// shell-out; expressions are parsed at compile time and re-evaluated on
// each visit.
type ComputeNode struct {
	BaseNode
	SchemaFields
	Exprs         []*ComputeExpr // ordered field-name → parsed AST pairs
	Publish       string         // persistent artifact name (empty = not published)
	PublishLabels []string       // DSL artifact_labels: applied to the published artifact
	AwaitMode     AwaitMode
}

// ComputeExpr is a single field expression in a ComputeNode.
type ComputeExpr struct {
	Key string    // output field name
	AST *expr.AST // parsed expression
	Raw string    // original source for diagnostics / unparse
}

// NodeKind implements Node.
func (n *ComputeNode) NodeKind() NodeKind { return NodeCompute }

// EmitNode publishes a named event with an immutable payload into the
// run-scoped event registry (ADR-051). It performs no LLM call and no shell-out;
// the payload is resolved from the With data-mappings on each visit.
type EmitNode struct {
	BaseNode
	Event string         // event name to publish
	With  []*DataMapping // payload fields (immutable, resolved per visit)
}

// NodeKind implements Node.
func (n *EmitNode) NodeKind() NodeKind { return NodeEmit }

// WaitNode blocks its branch until the named event is emitted in the same run,
// then completes with the event payload as its output (ADR-051). The Timeout is
// mandatory (the "no silent infinity" invariant) and bounds the wait.
type WaitNode struct {
	BaseNode
	SchemaFields               // optional OutputSchema typing the received payload
	Event        string        // event name to wait for
	Timeout      time.Duration // mandatory bound on the wait
}

// NodeKind implements Node.
func (n *WaitNode) NodeKind() NodeKind { return NodeWait }

// AwaitAnswersNode blocks its branch until every pending async human question
// (posted via the ask_user_async tool by the From node — or by any node in the
// run when From is empty) has been answered, then completes with the collected
// answers as its output: {answers: [{interaction_id, node, question, answer}]}.
// The Timeout is mandatory (the "no silent infinity" invariant) and bounds the
// wait. The predicate is level-triggered against the interaction store, so
// answers that arrived while the process was down are honoured on resume.
type AwaitAnswersNode struct {
	BaseNode
	From    string        // optional node ref: only await questions posted by this node ("" = whole run)
	Timeout time.Duration // mandatory bound on the wait
}

// NodeKind implements Node.
func (n *AwaitAnswersNode) NodeKind() NodeKind { return NodeAwaitAnswers }

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
// LLMNode — shared accessor interface for the two full LLM node kinds
// ---------------------------------------------------------------------------

// LLMNode is satisfied by *AgentNode and *JudgeNode — the two node kinds
// that carry the complete LLM field set (LLMFields, SchemaFields,
// InteractionFields plus tools, capabilities, MCP, memory, compaction,
// cursors, compress). It lets the field accessors below and the validators in
// validate*.go / mermaid.go iterate over both uniformly instead of
// repeating a `case *AgentNode … case *JudgeNode …` ladder at every read
// site.
//
// RouterNode embeds LLMFields too but deliberately does NOT satisfy
// LLMNode (it has no Publish/Session/Memory/…); call sites that also
// handle RouterLLM keep an explicit `case *RouterNode`. HumanNode keeps
// its own explicit branches as well. Because the methods are declared on
// *AgentNode / *JudgeNode (not on an embedded carrier), adding LLMNode
// changes neither the struct layout nor the JSON encoding — the
// field-literal construction used across the test suite keeps compiling.
type LLMNode interface {
	Node
	GetLLMFields() *LLMFields
	GetSchemaFields() *SchemaFields
	GetInteractionFields() *InteractionFields
	GetAwaitMode() AwaitMode
	GetSession() SessionMode
	GetPublish() string
	GetTools() []string
	GetToolMaxSteps() int
	GetCapabilities() []string
	GetSkills() []string
	GetActiveMCPServers() []string
	GetCompaction() *Compaction
	GetMemory() *Memory
	GetCursors() *CursorInvocation
	// GetFallbacks returns the node's ordered `fallbacks:` routes. It is
	// on the interface — rather than executor-private state — because
	// three PRE-RUN analyses read a node's backend and would otherwise
	// be computed from the head element alone: the sandbox's iterion
	// bind-mount (containsClawNode), parallel-branch admission
	// (unrestrictedCLIBackendCanWrite), and the fan_out_each mutation
	// guard. See ADR-087 decision 1.
	GetFallbacks() []Fallback
	GetCompress() string
	GetAutoMemory() string
	GetPermission() string
}

var (
	_ LLMNode = (*AgentNode)(nil)
	_ LLMNode = (*JudgeNode)(nil)
)

// LLMNode accessor methods on *AgentNode.
func (n *AgentNode) GetLLMFields() *LLMFields                 { return &n.LLMFields }
func (n *AgentNode) GetSchemaFields() *SchemaFields           { return &n.SchemaFields }
func (n *AgentNode) GetInteractionFields() *InteractionFields { return &n.InteractionFields }
func (n *AgentNode) GetAwaitMode() AwaitMode                  { return n.AwaitMode }
func (n *AgentNode) GetSession() SessionMode                  { return n.Session }
func (n *AgentNode) GetPublish() string                       { return n.Publish }
func (n *AgentNode) GetTools() []string                       { return n.Tools }
func (n *AgentNode) GetToolMaxSteps() int                     { return n.ToolMaxSteps }
func (n *AgentNode) GetCapabilities() []string                { return n.Capabilities }
func (n *AgentNode) GetSkills() []string                      { return n.Skills }
func (n *AgentNode) GetActiveMCPServers() []string            { return n.ActiveMCPServers }
func (n *AgentNode) GetCompaction() *Compaction               { return n.Compaction }
func (n *AgentNode) GetMemory() *Memory                       { return n.Memory }
func (n *AgentNode) GetCursors() *CursorInvocation            { return n.Cursors }
func (n *AgentNode) GetFallbacks() []Fallback                 { return n.Fallbacks }
func (n *AgentNode) GetCompress() string                      { return n.Compress }
func (n *AgentNode) GetAutoMemory() string                    { return n.AutoMemory }
func (n *AgentNode) GetPermission() string                    { return n.Permission }

// LLMNode accessor methods on *JudgeNode.
func (n *JudgeNode) GetLLMFields() *LLMFields                 { return &n.LLMFields }
func (n *JudgeNode) GetSchemaFields() *SchemaFields           { return &n.SchemaFields }
func (n *JudgeNode) GetInteractionFields() *InteractionFields { return &n.InteractionFields }
func (n *JudgeNode) GetAwaitMode() AwaitMode                  { return n.AwaitMode }
func (n *JudgeNode) GetSession() SessionMode                  { return n.Session }
func (n *JudgeNode) GetPublish() string                       { return n.Publish }
func (n *JudgeNode) GetTools() []string                       { return n.Tools }
func (n *JudgeNode) GetToolMaxSteps() int                     { return n.ToolMaxSteps }
func (n *JudgeNode) GetCapabilities() []string                { return n.Capabilities }
func (n *JudgeNode) GetSkills() []string                      { return n.Skills }
func (n *JudgeNode) GetActiveMCPServers() []string            { return n.ActiveMCPServers }
func (n *JudgeNode) GetCompaction() *Compaction               { return n.Compaction }
func (n *JudgeNode) GetMemory() *Memory                       { return n.Memory }
func (n *JudgeNode) GetCursors() *CursorInvocation            { return n.Cursors }
func (n *JudgeNode) GetFallbacks() []Fallback                 { return n.Fallbacks }
func (n *JudgeNode) GetCompress() string                      { return n.Compress }
func (n *JudgeNode) GetAutoMemory() string                    { return n.AutoMemory }
func (n *JudgeNode) GetPermission() string                    { return n.Permission }

// ---------------------------------------------------------------------------
// Node field accessors — exported helpers that extract fields from concrete
// node types via the Node interface. Consumers should use these instead of
// writing their own type switches. The Agent/Judge arms collapse to a
// single `case LLMNode` branch.
// ---------------------------------------------------------------------------

// NodeAwaitMode returns the AwaitMode for nodes that support it, or AwaitNone.
func NodeAwaitMode(n Node) AwaitMode {
	switch n := n.(type) {
	case LLMNode:
		return n.GetAwaitMode()
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

// NodeImplicitOutputFields returns the FIXED output field names of node
// kinds whose output shape is built in rather than schema-declared
// (await_answers → {answers}). nil for every other kind. Reference
// validation accepts exactly these fields and hard-errors on any other,
// keeping the per-kind knowledge here instead of inside the validators.
func NodeImplicitOutputFields(n Node) []string {
	if _, ok := n.(*AwaitAnswersNode); ok {
		return []string{"answers"}
	}
	return nil
}

// NodeOutputSchema returns the OutputSchema for nodes that support it, or "".
func NodeOutputSchema(n Node) string {
	switch n := n.(type) {
	case LLMNode:
		return n.GetSchemaFields().OutputSchema
	case *HumanNode:
		return n.OutputSchema
	case *ToolNode:
		return n.OutputSchema
	case *ComputeNode:
		return n.OutputSchema
	case *SubbotNode:
		return n.OutputSchema
	case *WaitNode:
		return n.OutputSchema
	}
	return ""
}

// NodeInputSchema returns the InputSchema for nodes that support it, or "".
func NodeInputSchema(n Node) string {
	switch n := n.(type) {
	case LLMNode:
		return n.GetSchemaFields().InputSchema
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
	case LLMNode:
		return n.GetPublish()
	case *HumanNode:
		return n.Publish
	case *ToolNode:
		return n.Publish
	case *ComputeNode:
		return n.Publish
	}
	return ""
}

// NodePublishLabels returns the DSL `artifact_labels:` list for the
// publish-capable node types (Agent/Human/Tool/Compute), or nil. Judge
// nodes don't publish, so they're excluded.
func NodePublishLabels(n Node) []string {
	switch n := n.(type) {
	case *AgentNode:
		return n.PublishLabels
	case *HumanNode:
		return n.PublishLabels
	case *ToolNode:
		return n.PublishLabels
	case *ComputeNode:
		return n.PublishLabels
	}
	return nil
}

// NodeInteraction returns the Interaction field for nodes that support it, or InteractionNone.
func NodeInteraction(n Node) InteractionMode {
	switch n := n.(type) {
	case LLMNode:
		return n.GetInteractionFields().Interaction
	case *HumanNode:
		return n.Interaction
	}
	return InteractionNone
}

// NodeActiveMCPServers returns the ActiveMCPServers list for nodes that support it, or nil.
func NodeActiveMCPServers(n Node) []string {
	if ln, ok := n.(LLMNode); ok {
		return ln.GetActiveMCPServers()
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
	case LLMNode:
		refs = appendLLMPromptRefs(refs, n.GetLLMFields())
		if p := n.GetInteractionFields().InteractionPrompt; p != "" {
			refs = append(refs, p)
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
	SessionFresh              = types.SessionFresh
	SessionInherit            = types.SessionInherit
	SessionInheritIfAvailable = types.SessionInheritIfAvailable
	SessionArtifactsOnly      = types.SessionArtifactsOnly
	SessionFork               = types.SessionFork
)

type RouterMode = types.RouterMode

const (
	RouterFanOutAll  = types.RouterFanOutAll
	RouterCondition  = types.RouterCondition
	RouterRoundRobin = types.RouterRoundRobin
	RouterLLM        = types.RouterLLM
	RouterFanOutEach = types.RouterFanOutEach
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
	InteractionReview     = types.InteractionReview
	InteractionAsync      = types.InteractionAsync
)

// Review-gate posture values (interaction: review).
const (
	PostureHumanRequired  = "human_required"   // always wait for the human's merge action (default)
	PostureAgentVerdictOK = "agent_verdict_ok" // a high-confidence companion approval may auto-merge
)

// DefaultReviewMaxTurns bounds the companion↔human dialogue so it always
// converges to an asymptote rather than re-pausing forever.
const DefaultReviewMaxTurns = 8

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
	// Env carries extra environment variables for a stdio server process.
	// The DSL `mcp_server` block has no `env:`; this is populated only for
	// plugin-contributed servers (e.g. firecrawl's FIRECRAWL_API_URL /
	// FIRECRAWL_API_KEY), whose manifest env is resolved at catalog-build
	// time. Without threading it here the self-host routing is lost and the
	// server would fall back to its public API.
	Env  map[string]string
	Auth *MCPAuth
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

	// IsElse marks the explicit fallback edge (`src -> dst else`): taken
	// only when no conditional sibling matched. Runtime-wise it plays
	// the same role as a bare unconditional edge among conditional
	// siblings — the compiler validates the stricter contract (C015/
	// C039/C040) and IsConditional stays false (else is guardless).
	IsElse bool

	// Loop reference (optional). LoopName references a Loop in Workflow.Loops.
	LoopName string

	// Foreach reference (optional). ForeachName references a Foreach in
	// Workflow.Foreaches: a (back-)edge that iterates its body over a
	// collection, in order. Mutually exclusive with LoopName.
	ForeachName string

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

// IsBoundedIteration reports whether an edge is a bounded iteration back-edge —
// either a named loop (max_iterations) or a foreach (collection-bounded). Such
// edges are cycles by design and are not default fall-through edges.
func (e *Edge) IsBoundedIteration() bool {
	return e != nil && (e.LoopName != "" || e.ForeachName != "")
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
	RefSecrets                    // {{secrets.<name>}} — renders the placeholder; materialised at exec
	RefEach                       // {{each.<name>.item|index|count|first|last}} — sequential foreach binding
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
	case RefSecrets:
		return "secrets"
	case RefEach:
		return "each"
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
	// Unquoted indicates the author requested raw substitution by writing
	// `{{!input.X}}` (bang prefix). The runtime substitutes the value
	// verbatim into shell tool commands instead of running it through
	// shellEscape — useful when the substituted value is itself a shell
	// snippet that must be re-interpreted (e.g. a command line emitted
	// upstream by a stack-detection agent). Trades shell-injection
	// containment for re-interpretability; only use on trusted inputs.
	Unquoted bool
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
	FieldTypeFile        = types.FieldTypeFile
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
	EnumValues []string // non-nil only if enum constraint present (string vars)
	HasDefault bool
	Default    any // string, int64, float64, or bool
}

// Secret is a resolved workflow secret declaration. Value is the raw
// value expression (typically "${ENV}" / a {{vars.X}} reference),
// resolved to the real plaintext at run start by the runtime; the agent
// only ever sees either a placeholder (As=value) or the mounted file path
// (As=file). Hosts scopes which egress destinations the secret may be
// materialised toward (Layer 2).
type Secret struct {
	Name        string
	Value       string
	As          string
	MountPath   string
	Env         string
	Optional    bool // file secret: skip the mount (no error) when unresolved
	Hosts       []string
	Description string
}

func (s *Secret) IsFile() bool {
	return s != nil && s.As == "file"
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

// AsFieldType maps a VarType to the equivalent schema FieldType. ok is false
// for VarJSON (which is "any" — no single field type) so callers can bail to
// "no opinion" rather than treat it as a concrete type. The two enums are
// parallel; this is the one canonical mapping between them.
func (vt VarType) AsFieldType() (FieldType, bool) {
	switch vt {
	case VarString:
		return FieldTypeString, true
	case VarBool:
		return FieldTypeBool, true
	case VarInt:
		return FieldTypeInt, true
	case VarFloat:
		return FieldTypeFloat, true
	case VarStringArray:
		return FieldTypeStringArray, true
	}
	return FieldTypeJSON, false // VarJSON / unknown → no opinion
}

// ---------------------------------------------------------------------------
// Preset — resolved named bundle of variable values
// ---------------------------------------------------------------------------

// Preset is a resolved named "sous-bot": a launch-time specialization of a
// bot. Values are variable overrides stored with their coerced Go types
// (string, int64, float64, bool) matching the declared Var's type; the
// runtime overlays them onto the default vars before applying any `--var`
// flag. Prompt/Skills/DisplayName/Description are populated only for
// file-based presets (a bundle's `presets/<name>.md`); in-source `presets:`
// blocks leave them empty (var-only). Operators select a preset at run time
// via `--preset <name>` or the studio Launch picker.
type Preset struct {
	Name string
	// Values are variable overrides applied to the run (defaults < preset <
	// --var). Keys not declared by the workflow's `vars:` are dropped by the
	// engine's resolveVars, same as a stray --var.
	Values map[string]any
	// DisplayName is the operator-facing label (e.g. "Improve Quality (SRE)");
	// falls back to Name when empty. File-based presets only.
	DisplayName string
	// Description is a one-line summary surfaced in the studio Launch picker.
	// File-based presets only.
	Description string
	// Prompt is the bias fragment appended to every LLM node's system prompt
	// under a "## Focus" section at run time (see delegate.Task.PresetFragment).
	// Supports `{{vars.X}}` template refs, resolved per node. File-based only.
	Prompt string
	// Skills lists bundle skill names this preset makes relevant (e.g.
	// "lang-js-fallow"). All bundle skills are mirrored regardless; this list
	// is surfaced as a hint in the "## Focus" section and in the studio.
	// File-based presets only.
	Skills []string
}

// ---------------------------------------------------------------------------
// Attachment — resolved workflow attachment (binary input)
// ---------------------------------------------------------------------------

// Attachment is a resolved attachment declaration. The bytes themselves
// are persisted by the run store; this struct only carries the schema
// (name, type, validation hints) consumed by the parser, runtime and
// studio frontend.
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
	// MaxIterationsExpr carries the raw template source when the cap
	// was declared as `as <name>("{{outputs.X.cap}}")`. Empty for
	// literal-int caps. Refs are pre-parsed at compile time so the
	// runtime lookup is a pure string interpolation against rs.
	MaxIterationsExpr     string
	MaxIterationsExprRefs []*Ref
	// Unbounded marks `as <name>(unbounded)`: the loop has no user iteration
	// cap. It still terminates — the runtime bounds it by FuelCap (the
	// effective fuel ceiling) and by a liveness monitor (no-progress halt).
	// The cycle is still *declared*, so C019 stays silent. FuelCap is the
	// resolved per-loop fuel: the clause's own fuel, else budget.max_iterations.
	Unbounded bool
	FuelCap   int
	// Body is the set of node IDs that participate in the loop's cycle —
	// each node from which the loop's edge target is reachable and which
	// can reach the loop's edge source (i.e. nodes on a path that closes
	// the iteration). Computed at compile time. The runtime resets the
	// loop's counter when a non-loop edge enters a body node from a
	// non-body source, so the budget becomes per-entry rather than
	// global to the whole run (a fix loop nested inside a package loop
	// gets a fresh budget every package).
	Body map[string]bool
	// Entries is the set of node IDs that serve as the loop's entry
	// point — i.e. the targets of the loop-bearing back-edges. Used by
	// the runtime to scope the counter-reset rule precisely to "we are
	// re-entering the loop at its top", instead of "we are entering
	// any body node from outside the body". The looser rule misfires
	// when the body is computed too narrowly (e.g. a nested loop whose
	// non-loop forward+reverse BFS yields only the back-edge endpoints
	// — see recovery_loop in bots/secured-renovacy/main.bot: the body
	// was {alt_review, review_commit_auto}, so the edge
	// fix_X → review_commit_auto reset the counter every cycle and
	// review_commit_auto's iteration_path stuck at recovery_loop=0).
	Entries map[string]bool
}

// Foreach defines a named sequential iteration over a collection. A
// back-edge `... as foreach <name>(item in <collection>)` re-enters its
// body once per element, in order. The runtime advances an index (sharing
// rs.loopCounters under the foreach name) and exposes the current element
// via the `each.<name>` namespace ({{each.<name>.item|index|count|first|last}}).
type Foreach struct {
	Name           string
	Item           string // element binding identifier (informational)
	CollectionRaw  string // collection template source, e.g. "{{outputs.list.items}}"
	CollectionRefs []*Ref // pre-parsed refs resolved to a []any at runtime
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
	// WarnTokens is advisory-only: crossing it emits a budget_warning
	// (advisory) but never blocks execution. 0 = disabled.
	WarnTokens    int
	MaxIterations int
}

// ClampToCeiling lowers each numeric limit so it never EXCEEDS the
// corresponding ceiling (a non-zero ceiling field). Unlike applyBudgetOverrides
// (which lets a value rise), this only ever shrinks — it is the multitenant
// safeguard a cloud runner applies so a tenant's bot, however large its
// declared budget (especially `as X(unbounded)` whose fuel falls back to
// budget.MaxIterations), can never exceed the platform's hard ceiling. A zero
// ceiling field means "no platform limit on this dimension" and is ignored. A
// zero workflow field means "unlimited" and is RAISED to the ceiling (so an
// unbudgeted bot still inherits the platform cap). Duration is compared by
// parsed seconds; an unparseable value is replaced by the ceiling.
func (b *Budget) ClampToCeiling(ceiling *Budget) {
	if b == nil || ceiling == nil {
		return
	}
	b.MaxIterations = clampToCeiling(b.MaxIterations, ceiling.MaxIterations)
	b.MaxTokens = clampToCeiling(b.MaxTokens, ceiling.MaxTokens)
	b.MaxParallelBranches = clampToCeiling(b.MaxParallelBranches, ceiling.MaxParallelBranches)
	b.MaxCostUSD = clampToCeiling(b.MaxCostUSD, ceiling.MaxCostUSD)
	if ceiling.MaxDuration != "" {
		cd, cerr := time.ParseDuration(ExpandEnvWithDefault(ceiling.MaxDuration))
		if cerr == nil {
			vd, verr := time.ParseDuration(ExpandEnvWithDefault(b.MaxDuration))
			if verr != nil || b.MaxDuration == "" || vd > cd {
				b.MaxDuration = ceiling.MaxDuration
			}
		}
	}
}

// clampToCeiling lowers v to max when max is a real ceiling (>0) and v either
// exceeds it or is unlimited (<=0). A zero ceiling means "no platform limit"
// and leaves v untouched. Shared by ClampToCeiling's int and float dimensions.
func clampToCeiling[T int | float64](v, max T) T {
	if max > 0 && (v <= 0 || v > max) {
		return max
	}
	return v
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

// Memory opts a node into the iterion workspace memory tree at
// ~/.iterion/projects/<encoded-workdir>/memory/<Scope>/. The scope
// is a feature-bound subfolder (e.g. "session-continuity",
// "whats-next"). Autoload lists glob patterns relative to the
// scope whose content is mirrored into the system prompt at node
// start; default is the scope's INDEX.md only (keeps the LLM
// index-first, pulling richer files via memory_read on demand).
//
// PreCompactInject re-injects the autoload set before claw's
// heuristic compaction so its content survives summarisation.
type Memory struct {
	Enabled          bool
	Scope            string
	Autoload         []string
	Read             bool
	Write            bool
	PreCompactInject bool
	// ProjectRoot, when true, re-roots the scope under the run's
	// `RepoRoot` (the source-of-truth field stored on the run record)
	// instead of the per-run workDir. Lets a dispatcher-spawned bot
	// running in `<repo>/.iterion/dispatcher/workspaces/<id>` share a
	// scope (e.g. session-continuity memory) with a whats-next run
	// that lives at the repo root.
	ProjectRoot bool
	// Visibility selects the sharing axis (bot | project | cross_project
	// | user | org | global). Empty keeps the legacy per-bot/per-project
	// behaviour; when set, Scope is the space name and the runtime
	// resolves the tenant/user/project identity.
	Visibility string
}

// ---------------------------------------------------------------------------
// Cursors — prompt-engineering dials (IR side)
// ---------------------------------------------------------------------------

// CursorDef is the normalized IR form of a `cursor NAME:` declaration.
// Exactly one of Values / Bands is non-nil (validated by C085).
type CursorDef struct {
	Name        string
	Description string
	Values      []CursorValue // enum form: ordered, numeric invocations snap to position
	Bands       []CursorBandSpec
}

// CursorValue is one ordered entry of an enum cursor.
type CursorValue struct {
	Name   string
	Prompt string
}

// CursorBandSpec is one resolved entry of a numeric cursor: Lo..Hi
// (inclusive on both ends) → Prompt. Parsed from the AST band Range
// string ("0.0..0.33").
type CursorBandSpec struct {
	Lo     float64
	Hi     float64
	Prompt string
}

// CursorInvocation is the IR form of an agent/judge `cursors:` block.
// Settings preserves declaration order; resolution sorts by cursor
// name alphabetically before composing the prompt suffix so identical
// activations produce identical prompts (prompt-cache friendly).
// Fallback is one compiled route of a node's `fallbacks:` block: a
// complete alternative (backend + model + credential hint) the runtime
// tries when the preceding route fails.
//
// Every field may carry ${VAR} refs, resolved at run time.
type Fallback struct {
	Name     string
	Backend  string   // "" = the node's backend
	Model    string   // "" = the node's model
	Provider string   // "" = auto
	On       []string // failure categories that may route here; empty = the runtime default
	Metered  bool     // the author's acknowledgement that this route spends a metered credential
}

type CursorInvocation struct {
	Enabled  bool
	Settings []CursorSetting
}

// CursorSetting is one `name: value` pair inside a `cursors:` block.
// Value is stored raw (may contain ${VAR}); resolution happens at
// runtime against the workflow's Cursors map.
type CursorSetting struct {
	Key   string
	Value string
}
