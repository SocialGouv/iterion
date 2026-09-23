package runtime

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The parallel-branch guard exists to stop N branches racing one git index. It
// decides from the node's DECLARED `tools:` list, before the run — and that
// list is not the list the node holds: assembleEffectiveTools folds the
// runtime's own opt-ins over it at build time.
//
// Two of those opt-ins widen the WORKSPACE surface, and neither is visible in
// the IR: `auto_memory: on` grants write_file on claw (and resolves through a
// launch flag, the workflow, and ITERION_AUTO_MEMORY), and ultracode grants
// the unbounded `agent`/`workflow` pair (and resolves through a per-node model
// override). So the guard asks the production executor rather than re-deriving
// the rules — the same seam, and the same reason, as EffectiveBackendName.
//
// The resolver here is the REAL *model.ClawExecutor, never a stub: a stub that
// answers what the test wants certifies the test, not the product.
func effectiveSurfaceEngine(t *testing.T, wf *ir.Workflow, opts ...model.ClawExecutorOption) *Engine {
	t.Helper()
	return &Engine{workflow: wf, executor: model.NewClawExecutor(model.NewRegistry(), wf, opts...)}
}

func agentNode(id string, tools []string, backend string, mutate func(*ir.AgentNode)) *ir.AgentNode {
	n := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: id},
		LLMFields: ir.LLMFields{Backend: backend},
		Tools:     tools,
	}
	if mutate != nil {
		mutate(n)
	}
	return n
}

func TestParallelAdmissionReadsTheToolsANodeWillHoldNotTheOnesItDeclared(t *testing.T) {
	readOnlyList := []string{"read_file"}

	cases := []struct {
		name    string
		node    *ir.AgentNode
		wf      *ir.Workflow
		edges   []*ir.Edge
		env     map[string]string
		opts    []model.ClawExecutorOption
		mutates bool
		why     string
	}{
		{
			name: "auto_memory grants write_file to a node that declared only a reader",
			node: agentNode("n", readOnlyList, "claw", func(n *ir.AgentNode) { n.AutoMemory = "on" }),
			// `tools: [read_file]` + auto_memory -> [read_file todo_write write_file glob]
			mutates: true,
			why:     "N such branches share one worktree while the node holds a file writer",
		},
		{
			name:    "auto_memory taken from the WORKFLOW, which the node never mentions",
			node:    agentNode("n", readOnlyList, "claw", nil),
			wf:      &ir.Workflow{AutoMemory: "on"},
			mutates: true,
			why:     "the append is decided by a value that is not on the node",
		},
		{
			name:    "auto_memory taken from the LAUNCH, which is in no IR at all",
			node:    agentNode("n", readOnlyList, "claw", nil),
			opts:    []model.ClawExecutorOption{model.WithAutoMemoryOverride("on")},
			mutates: true,
			why:     "re-deriving the append rules from the IR cannot see a launch flag",
		},
		{
			name:    "ultracode grants the unbounded agent subagent tool",
			node:    agentNode("n", readOnlyList, "claw", func(n *ir.AgentNode) { n.ReasoningEffort = "ultracode" }),
			mutates: true,
			why:     "a subagent may do anything the parent could",
		},
		// …and the shapes that must NOT flip. A blocking guard's false
		// positive refuses a legitimate workflow at run start, which on this
		// surface costs as much as the hole. Every append below acts somewhere
		// other than the shared worktree.
		{
			name:    "interaction adds ask_user, which is a question channel",
			node:    agentNode("n", readOnlyList, "claw", func(n *ir.AgentNode) { n.Interaction = ir.InteractionHuman }),
			mutates: false,
		},
		{
			name:    "async interaction adds the non-blocking pair",
			node:    agentNode("n", readOnlyList, "claw", func(n *ir.AgentNode) { n.Interaction = ir.InteractionAsync }),
			mutates: false,
		},
		{
			name:    "todo_write, which claw keeps out of the repository on purpose",
			node:    agentNode("n", readOnlyList, "claw", nil),
			mutates: false,
		},
		// The board and runs MCP tools act on the board and the run store, so
		// they alone would not flip a node — but a claude_code node is
		// mutating for another reason, the row below.
		// On CLAW, so the row can actually witness the term: a claude_code
		// node is mutating for an unrelated reason and `readonly:` is honoured
		// before the surface is consulted, so either would make this assertion
		// unable to redden.
		{
			name:    "board capabilities add tools that act on the BOARD",
			node:    agentNode("n", readOnlyList, "claw", func(n *ir.AgentNode) { n.Capabilities = []string{"board.read", "board.move"} }),
			mutates: false,
		},
		{
			name:    "runs.read adds tools that act on the run store",
			node:    agentNode("n", readOnlyList, "claw", func(n *ir.AgentNode) { n.Capabilities = []string{"runs.read"} }),
			mutates: false,
		},
		// A backend that never RECEIVES the list cannot be bounded by it.
		{
			name:    "a declared list on a backend that never receives it is no bound",
			node:    agentNode("n", readOnlyList, "kimi", nil),
			mutates: true,
			why:     "the agent keeps its own full toolset whatever the list says",
		},
		{
			// The primary is claude_code, not claw: the pre-existing
			// claw→CLI arm is not consulted for it, so only the rule under
			// test can answer this row.
			name: "…and the same is true of a ROUTE that never receives it",
			node: agentNode("n", readOnlyList, "claude_code", func(n *ir.AgentNode) {
				n.Fallbacks = []ir.Fallback{{Name: "cheap", Backend: "kimi"}}
			}),
			mutates: true,
			why:     "admission is decided once; a fall-through must not un-bound the node",
		},
		// The appends are decided PER ROUTE, so a non-claw primary with a claw
		// route picks up claw's appends when it falls through.
		{
			name: "a claw ROUTE brings claw's appends to a claude_code node",
			node: agentNode("n", readOnlyList, "claude_code", func(n *ir.AgentNode) {
				n.AutoMemory = "on"
				n.Fallbacks = []ir.Fallback{{Name: "gpt_forfait", Backend: "claw"}}
			}),
			mutates: true,
			why:     "buildTask is re-run for a route that changes the backend",
		},
		// A declaration reaches claude_code as `--disallowedTools` over a
		// CLOSED native roster this project does not own, so it removes the
		// names iterion happens to enumerate and nothing else — measured on
		// 2.1.220, what survives includes tools that move the worktree the
		// session acts in. #1671 already drew this conclusion for `tools: []`;
		// a non-empty list has no better claim. The rule therefore names no
		// tool: a guard that enumerated the survivors found a new spelling
		// every round.
		{
			name:    "a claude_code declaration is not a bound on what the node holds",
			node:    agentNode("n", readOnlyList, "claude_code", nil),
			mutates: true,
			why:     "the list becomes --disallowedTools over a closed roster this project does not own, so what survives is a property of the installed CLI",
		},
		// …and the escape hatch the engine already documents for exactly this:
		// `readonly:` is a scheduling assertion, honoured before the surface
		// is ever consulted.
		{
			name:    "…and `readonly:` is the escape hatch that keeps it eligible",
			node:    agentNode("n", readOnlyList, "claude_code", func(n *ir.AgentNode) { n.Readonly = true }),
			mutates: false,
		},
		// An incoming edge can hand a node `_reasoning_effort: "ultracode"` at
		// dispatch — after admission has decided — and `ultracode` IS a member
		// of ir.ValidReasoningEfforts. Where such an edge exists the surface is
		// read pessimistically; where it does not, nothing changes.
		{
			name:    "an edge that maps _reasoning_effort can grant the subagent tool later",
			node:    agentNode("n", readOnlyList, "claw", nil),
			edges:   []*ir.Edge{{From: "r", To: "n", With: []*ir.DataMapping{{Key: "_reasoning_effort", Raw: "ultracode"}}}},
			mutates: true,
			why:     "the input arrives after admission has already admitted the branch",
		},
		{
			name:    "an edge that maps something else changes nothing",
			node:    agentNode("n", readOnlyList, "claw", nil),
			edges:   []*ir.Edge{{From: "r", To: "n", With: []*ir.DataMapping{{Key: "topic", Raw: "x"}}}},
			mutates: false,
		},
		// The VALUE decides, not the key. Reading every effort edge as a
		// possible escalation refused fan-outs that are safe by construction —
		// `low` is not ultracode and never will be.
		{
			name:    "an edge that maps a LITERAL non-ultracode effort changes nothing",
			node:    agentNode("n", readOnlyList, "claw", nil),
			edges:   []*ir.Edge{{From: "r", To: "n", With: []*ir.DataMapping{{Key: "_reasoning_effort", Raw: `"low"`}}}},
			mutates: false,
		},
		// …and a TEMPLATE is the case the pessimism was written for: its value
		// arrives when the router dispatches.
		{
			name:    "an edge that maps a TEMPLATED effort is read pessimistically",
			node:    agentNode("n", readOnlyList, "claw", nil),
			edges:   []*ir.Edge{{From: "r", To: "n", With: []*ir.DataMapping{{Key: "_reasoning_effort", Raw: "{{vars.effort}}", Refs: []*ir.Ref{{}}}}}},
			mutates: true,
			why:     "the value is not resolved here, so the widening it may carry is taken as present",
		},
		// An ultracode claude_code node stays mutating with the orchestration
		// knob SET. This row asserts the verdict, not the mechanism: the knob
		// being answered per NODE rather than per process is witnessed where
		// it can be falsified, in delegate's
		// TestTheOrchestrationKnobIsAnsweredPerNodeNotPerProcess — here
		// `Workflow` alone would keep the verdict red, so the row cannot see
		// the difference.
		{
			name:    "an ultracode claude_code node is mutating even under the orchestration knob",
			node:    agentNode("n", readOnlyList, "claude_code", func(n *ir.AgentNode) { n.ReasoningEffort = "ultracode" }),
			env:     map[string]string{"ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS": "1"},
			mutates: true,
			why:     "claudeSpawnBounds withholds nothing from an ultracode node, knob or not",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			wf := tc.wf
			if wf == nil {
				wf = &ir.Workflow{}
			}
			if wf.Nodes == nil {
				wf.Nodes = map[string]ir.Node{}
			}
			wf.Nodes[tc.node.ID] = tc.node
			wf.Edges = append(wf.Edges, tc.edges...)
			e := effectiveSurfaceEngine(t, wf, tc.opts...)
			got := isMutatingNodeIn(wf, tc.node, wf.DefaultBackend, e.backendResolver(), e.toolSurfaceResolver(), false)
			if got != tc.mutates {
				if tc.mutates {
					t.Fatalf("admitted as read-only: %s (effective tools: %v)", tc.why,
						e.toolSurfaceResolver().EffectiveToolNames(tc.node, len(tc.edges) > 0))
				}
				t.Fatalf("refused as mutating although nothing it holds touches the workspace (effective tools: %v)",
					e.toolSurfaceResolver().EffectiveToolNames(tc.node, len(tc.edges) > 0))
			}
		})
	}
}

// …and the same verdict through the guard the engine actually runs: two
// branches that each declare only a reader, and each hold write_file.
func TestFanOutRefusesTwoBranchesThatBothHoldAWriterTheyNeverDeclared(t *testing.T) {
	a := agentNode("a", []string{"read_file"}, "claw", func(n *ir.AgentNode) { n.AutoMemory = "on" })
	b := agentNode("b", []string{"read_file"}, "claw", func(n *ir.AgentNode) { n.AutoMemory = "on" })
	router := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fan"}, RouterMode: ir.RouterFanOutAll}
	wf := &ir.Workflow{
		Nodes: map[string]ir.Node{"fan": router, "a": a, "b": b},
		Edges: []*ir.Edge{{From: "fan", To: "a"}, {From: "fan", To: "b"}},
	}
	e := effectiveSurfaceEngine(t, wf)
	err := e.validateWorkspaceSafety("fan", wf.Edges)
	if err == nil {
		t.Fatal("two branches holding write_file were admitted onto one shared worktree — the git-index race this guard exists to prevent")
	}
	rerr, ok := err.(*RuntimeError)
	if !ok || rerr.Code != ErrCodeWorkspaceSafety {
		t.Fatalf("err = %v, want the typed workspace-safety refusal", err)
	}
}

// decoratorWithoutTheSeam wraps the production executor and forgets the
// tool-surface method — the shape the seam is reached by, since it is an
// optional type assertion. Embedding NodeExecutor keeps it a legal executor.
type decoratorWithoutTheSeam struct{ NodeExecutor }

// The two executors an engine is built with in production both answer the
// tool-surface seam, and that is a COMPILE-time guarantee (the pins beside the
// interface and in pkg/dryrun). This test names the class the pins protect, so
// removing the pin AND the method is a red test, not only a silent widening.
// (The pin alone is the build-time guard; this test is the runtime witness that
// the class is still named.)
//
// Why the guarantee is made there and not at runtime, measured both ways: an
// executor that cannot answer sends admission back to the declared list, and
// neither available runtime answer is shippable. Falling back to a
// program-only executor reads the declared list verbatim whenever no
// backend resolves at all (neither the node's `backend:` nor the workflow's
// `default_backend:`) — and it refuses what `--auto-memory off` admits. Refusing outright costs
// 88 top-level tests of this package (113 with subtests), measured with
// `if surfaces == nil { return sharedWorkspace }`, and takes parallel fan-out
// away from any engine built through pkg/benchmark's exported ExecutorFactory.
func TestBothProductionExecutorsAnswerTheToolSurfaceSeam(t *testing.T) {
	var claw any = (*model.ClawExecutor)(nil)
	if _, ok := claw.(EffectiveToolSurfaceResolver); !ok {
		t.Error("*model.ClawExecutor no longer answers the seam — admission would read the declared `tools:` list, which is the reading #1652 removed")
	}
	// dryrun's executor is pinned in its own package: pkg/dryrun imports
	// pkg/runtime, so this package cannot name it.
}

// stubExecutorNoSurface is an executor that resolves backends and nothing else.
// No production executor has this shape any more — both leaves answer the seam,
// and both are pinned — so it is not a double of one: it exists so the verdict
// can be measured against an executor that CANNOT answer.
type stubExecutorNoSurface struct{ NodeExecutor }

func (stubExecutorNoSurface) EffectiveBackendName(ir.Node) string { return "" }

// The shared-worktree question must be answered the same way whatever the
// engine's executor happens to implement. Keyed on "a tool-surface resolver was
// supplied", `iterion validate --exec --strict` — a documented CI gate, whose
// dry-run executor implemented no tool surface at the time — reported a clean
// verdict on a fan-out the same tree kills at the router. It answers the seam
// since #1652, so that divergence cannot recur through THAT executor; the
// parameter is what keeps the next one from re-opening it. Two products of one
// tree disagreeing about one file is the defect; the rule is a property of the
// QUESTION, not of the caller's capabilities.
func TestTheSharedWorktreeQuestionDoesNotDependOnWhatTheExecutorImplements(t *testing.T) {
	branch := func(id string) *ir.AgentNode {
		return agentNode(id, []string{"read_file"}, "claude_code", nil)
	}
	l, r := branch("l"), branch("r")
	router := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fan"}, RouterMode: ir.RouterFanOutAll}
	wf := &ir.Workflow{
		Nodes: map[string]ir.Node{"fan": router, "l": l, "r": r},
		Edges: []*ir.Edge{{From: "fan", To: "l"}, {From: "fan", To: "r"}},
	}
	for _, tc := range []struct {
		name string
		eng  *Engine
	}{
		{"an executor that resolves the whole tool surface", effectiveSurfaceEngine(t, wf)},
		{"an executor that resolves nothing but backends", &Engine{workflow: wf, executor: stubExecutorNoSurface{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.eng.validateWorkspaceSafety("fan", wf.Edges); err == nil {
				t.Fatalf("two claude_code branches were admitted onto one worktree — the verdict moved with the executor's capabilities, so `iterion validate --exec` and `iterion run` answer differently about one file")
			}
		})
	}
}

// An executor that cannot answer the seam leaves admission reading the program
// alone — a weaker reading than a run can give, and one an operator has no
// other way to learn about. So the run says WHICH executor, by concrete type,
// and says it once however many branches ask.
func TestTheRunSaysWhichExecutorCannotAnswerTheToolSurfaceSeam(t *testing.T) {
	var buf bytes.Buffer
	node := agentNode("n", []string{"read_file"}, "claw", func(n *ir.AgentNode) { n.AutoMemory = "on" })
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"n": node}}
	production := model.NewClawExecutor(model.NewRegistry(), wf)
	e := &Engine{workflow: wf, executor: decoratorWithoutTheSeam{production}, logger: iterlog.New(iterlog.LevelWarn, &buf)}

	for i := 0; i < 3; i++ {
		_ = isMutatingNodeIn(wf, node, "", e.backendResolver(), e.toolSurfaceResolver(), false)
	}
	out := buf.String()
	if !strings.Contains(out, "decoratorWithoutTheSeam") {
		t.Fatalf("the run does not name the executor that could not answer, so nobody can find the decorator that dropped the method: %q", out)
	}
	if got := strings.Count(out, "does not implement the effective-tool-surface seam"); got != 1 {
		t.Errorf("warned %d times for one engine, want 1 — admission asks per node, per branch, on every fan-out", got)
	}

	// The executor that DOES answer says nothing: a line an operator sees on
	// every healthy run is a line they learn to ignore.
	var quiet bytes.Buffer
	ok := &Engine{workflow: wf, executor: production, logger: iterlog.New(iterlog.LevelWarn, &quiet)}
	_ = isMutatingNodeIn(wf, node, "", ok.backendResolver(), ok.toolSurfaceResolver(), false)
	if strings.Contains(quiet.String(), "effective-tool-surface seam") {
		t.Errorf("the production executor warned although it answers the seam: %q", quiet.String())
	}
}
