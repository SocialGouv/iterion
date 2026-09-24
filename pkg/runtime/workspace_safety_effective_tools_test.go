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

// decoratorWithoutTheSeam wraps the production executor and forwards neither
// method admission reads — the shape both seams are reached by, since they are
// optional type assertions. Embedding NodeExecutor keeps it a legal executor.
type decoratorWithoutTheSeam struct{ NodeExecutor }

// forwardsToolsOnly wraps the production executor and forwards the tool seam
// but not the backend seam: half the remedy, the shape #1767 measured
// admitting what the production executor refuses.
type forwardsToolsOnly struct {
	NodeExecutor
	inner *model.ClawExecutor
}

func (f forwardsToolsOnly) EffectiveToolNames(node ir.Node, mayEscalateToUltracode bool) []string {
	return f.inner.EffectiveToolNames(node, mayEscalateToUltracode)
}

// The two executors an engine is built with in production answer both seams
// admission reads, and that is a COMPILE-time guarantee (the pins beside the
// interfaces and in pkg/dryrun). This test names the class the pins protect, so
// removing a pin AND its method is a red test, not only a build that still
// passes: an executor that stops answering is read at the worst case, its
// fan-outs refused as if every agent and judge not marked `readonly:` declared
// a write tool.
func TestBothProductionExecutorsAnswerBothSeams(t *testing.T) {
	var claw any = (*model.ClawExecutor)(nil)
	if _, ok := claw.(EffectiveToolSurfaceResolver); !ok {
		t.Error("*model.ClawExecutor no longer answers the tool seam — admission would read every agent and judge it runs at the worst case, as if each declared a write tool")
	}
	if _, ok := claw.(EffectiveBackendResolver); !ok {
		t.Error("*model.ClawExecutor no longer answers the backend seam — admission would read every agent and judge it runs at the worst case, as if each declared a write tool")
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

// A route that no `tools:` list bounds must be refused whatever the engine's
// executor happens to implement. Keyed on "a tool-surface resolver was
// supplied", `iterion validate --exec --strict` — a documented CI gate, whose
// dry-run executor implemented no tool surface at the time — reported a clean
// verdict on a fan-out the same tree kills at the router. It answers the seam
// since #1652, so that divergence cannot recur through THAT executor; the
// parameter is what keeps the next one from re-opening it. Two products of one
// tree disagreeing about one file is the defect; the rule is a property of the
// QUESTION, not of the caller's capabilities. What the tool seam's absence
// changes, it changes only toward refusal: an executor that cannot answer is
// read at the worst case (the witnesses below).
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

// An executor that cannot answer a seam is read at the worst case, and the run
// says WHICH executor, by concrete type, and which seams it lacks, once however
// many branches ask — and by the time the refusal it causes is returned, so a
// refused fan-out is not the first an operator hears of it.
func TestTheRunSaysWhichExecutorCannotAnswerAndWhichSeamsItLacks(t *testing.T) {
	var buf bytes.Buffer
	wf := twoReaderBranches()
	production := model.NewClawExecutor(model.NewRegistry(), wf)
	e := &Engine{workflow: wf, executor: decoratorWithoutTheSeam{production}, logger: iterlog.New(iterlog.LevelWarn, &buf)}

	var refusal error
	for i := 0; i < 3; i++ {
		refusal = e.validateWorkspaceSafety("fan", wf.Edges)
	}
	if refusal == nil {
		t.Fatal("the fan-out was admitted — this test needs the refusal to check the warning precedes it")
	}
	out := buf.String()
	if !strings.Contains(out, "decoratorWithoutTheSeam") {
		t.Fatalf("by the time the refusal is returned, the run has not named the executor that could not answer: %q", out)
	}
	if got := strings.Count(out, "does not implement runtime.EffectiveToolSurfaceResolver or runtime.EffectiveBackendResolver"); got != 1 {
		t.Errorf("warned %d times for one engine naming both seams, want 1 — admission asks per node, per branch, on every fan-out: %q", got, out)
	}

	// Half the remedy is named for the half it lacks, and only that half.
	var half bytes.Buffer
	h := &Engine{workflow: wf, executor: forwardsToolsOnly{production, production}, logger: iterlog.New(iterlog.LevelWarn, &half)}
	if err := h.validateWorkspaceSafety("fan", wf.Edges); err == nil {
		t.Fatal("a wrapper forwarding only EffectiveToolNames was admitted — this test needs its refusal")
	}
	if got := strings.Count(half.String(), "does not implement runtime.EffectiveBackendResolver"); got != 1 || strings.Contains(half.String(), "EffectiveToolSurfaceResolver or") {
		t.Errorf("the warning must name the backend seam once, and only it: %q", half.String())
	}

	// The executor that DOES answer says nothing: a line an operator sees on
	// every healthy run is a line they learn to ignore.
	var quiet bytes.Buffer
	ok := &Engine{workflow: wf, executor: production, logger: iterlog.New(iterlog.LevelWarn, &quiet)}
	if err := ok.validateWorkspaceSafety("fan", wf.Edges); err != nil {
		t.Fatalf("the production executor refused two reader branches: %v", err)
	}
	if strings.Contains(quiet.String(), "does not implement") {
		t.Errorf("the production executor warned although it answers both seams: %q", quiet.String())
	}
}

// twoReaderBranches is a fan-out of two claw agents that declare only a reader
// and are granted nothing more: the production executor admits it onto one
// worktree, so any refusal of it comes from the executor, not from the nodes.
func twoReaderBranches(mutate ...func(*ir.AgentNode)) *ir.Workflow {
	return twoBranches(func(id string) ir.Node {
		n := agentNode(id, []string{"read_file"}, "claw", nil)
		for _, m := range mutate {
			m(n)
		}
		return n
	})
}

// twoBranches is a fan_out_all router `fan` with one branch per id, `a` and
// `b`, each built by mk. Its Edges are exactly the router's fan edges.
func twoBranches(mk func(id string) ir.Node) *ir.Workflow {
	router := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fan"}, RouterMode: ir.RouterFanOutAll}
	return &ir.Workflow{
		Nodes: map[string]ir.Node{"fan": router, "a": mk("a"), "b": mk("b")},
		Edges: []*ir.Edge{{From: "fan", To: "a"}, {From: "fan", To: "b"}},
	}
}

func judgeNode(id string, tools []string, backend string) *ir.JudgeNode {
	return &ir.JudgeNode{BaseNode: ir.BaseNode{ID: id}, LLMFields: ir.LLMFields{Backend: backend}, Tools: tools}
}

// resolvesBackendTo resolves every node to one backend and answers nothing
// else — a silent executor whose backend resolution still decides verdicts.
type resolvesBackendTo struct {
	NodeExecutor
	backend string
}

func (r resolvesBackendTo) EffectiveBackendName(ir.Node) string { return r.backend }

// resolvesBackendToAndAnswers is resolvesBackendTo answering the tool seam too,
// adding nothing: the same routing, from an executor that is not silent.
type resolvesBackendToAndAnswers struct{ resolvesBackendTo }

func (resolvesBackendToAndAnswers) EffectiveToolNames(ir.Node, bool) []string { return nil }

// The zero value of the seam is the refusal. An executor that cannot say what
// its nodes will hold — a wrapper that forgot to forward the method, an
// executor of an embedder's own — gets the worst case at parallel-branch
// admission: the reader branches the production executor admits are refused
// with the typed WORKSPACE_SAFETY error, and the reason names the executor and
// the method to implement.
func TestAnExecutorThatCannotAnswerTheSeamIsRefusedAParallelFanOutByName(t *testing.T) {
	for _, shape := range []struct {
		name           string
		mk             func(id string) ir.Node
		defaultBackend string
	}{
		{"agents declaring only a reader", func(id string) ir.Node { return agentNode(id, []string{"read_file"}, "claw", nil) }, ""},
		{"agents declaring no tools", func(id string) ir.Node { return agentNode(id, nil, "claw", nil) }, ""},
		{"judges declaring only a reader", func(id string) ir.Node { return judgeNode(id, []string{"read_file"}, "claw") }, ""},
		{"judges declaring no tools", func(id string) ir.Node { return judgeNode(id, nil, "claw") }, ""},
		{"agents on a backend whose declaration bounds them, not claw", func(id string) ir.Node { return agentNode(id, []string{"read_file"}, "codex", nil) }, ""},
		{"agents whose backend is the workflow's default_backend", func(id string) ir.Node { return agentNode(id, []string{"read_file"}, "", nil) }, "claw"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			wf := twoBranches(shape.mk)
			wf.DefaultBackend = shape.defaultBackend
			production := model.NewClawExecutor(model.NewRegistry(), wf)
			if err := (&Engine{workflow: wf, executor: production}).validateWorkspaceSafety("fan", wf.Edges); err != nil {
				t.Fatalf("precondition: the production executor must admit these two branches, or the refusal below proves nothing about the executor: %v", err)
			}
			for _, ex := range []struct {
				name     string
				executor NodeExecutor
				typ      string
				missing  string
				unread   string
			}{
				{"a wrapper around the production executor that forwards neither method", decoratorWithoutTheSeam{production}, "runtime.decoratorWithoutTheSeam",
					"runtime.EffectiveToolSurfaceResolver or runtime.EffectiveBackendResolver", "neither the route it will take nor the tools it will hold can be read"},
				{"an executor that resolves backends and nothing else", stubExecutorNoSurface{}, "runtime.stubExecutorNoSurface",
					"runtime.EffectiveToolSurfaceResolver", "the tools it will hold cannot be read"},
				{"a wrapper that forwards the tool seam and not the backend seam", forwardsToolsOnly{production, production}, "runtime.forwardsToolsOnly",
					"runtime.EffectiveBackendResolver", "the route it will take cannot be read"},
			} {
				t.Run(ex.name, func(t *testing.T) {
					err := (&Engine{workflow: wf, executor: ex.executor}).validateWorkspaceSafety("fan", wf.Edges)
					if err == nil {
						t.Fatal("two branches the executor cannot vouch for were admitted onto one shared worktree — the IR decided, which is the reading #1652 and #1767 removed")
					}
					rerr, ok := err.(*RuntimeError)
					if !ok || rerr.Code != ErrCodeWorkspaceSafety {
						t.Fatalf("err = %v, want the typed workspace-safety refusal a node declaring a write tool gets", err)
					}
					for _, id := range []string{"a", "b"} {
						want := `node "` + id + `" counts as writing because executor ` + ex.typ + " does not implement " + ex.missing + ", so " + ex.unread + " — implement or forward EffectiveToolNames and EffectiveBackendName on the executor, or mark the node `readonly:`"
						if !strings.Contains(rerr.Message, want) {
							t.Errorf("the refusal does not say why branch %q counts, nor what to implement:\n got: %s\nwant: …%s…", id, rerr.Message, want)
						}
					}
				})
			}
		})
	}
}

// The worst case reads a node exactly as a declared writer, and nowhere else:
// one branch the executor cannot vouch for is still admitted beside a reader,
// `readonly:` still opts a node out, and a fan_out_each template is refused
// only when it would actually replay concurrently — then with the executor
// named. Each verdict is checked against the same topology with a node that
// DECLARES a writer, under the production executor.
func TestAnUnvouchedNodeIsAdmittedExactlyWhereADeclaredWriterWouldBe(t *testing.T) {
	production := func(wf *ir.Workflow) *Engine {
		return &Engine{workflow: wf, executor: model.NewClawExecutor(model.NewRegistry(), wf)}
	}
	writes := func(n *ir.AgentNode) { n.Tools = []string{"write_file"} }
	tmpl := agentNode("t", []string{"read_file"}, "claw", nil)
	router := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "each"}, RouterMode: ir.RouterFanOutEach}
	each := &ir.Workflow{
		Nodes: map[string]ir.Node{"each": router, "t": tmpl},
		Edges: []*ir.Edge{{From: "each", To: "t"}},
	}
	writerTmpl := agentNode("t", []string{"write_file"}, "claw", nil)
	writerEach := &ir.Workflow{
		Nodes: map[string]ir.Node{"each": router, "t": writerTmpl},
		Edges: []*ir.Edge{{From: "each", To: "t"}},
	}
	for _, silent := range []struct {
		name   string
		mk     func(wf *ir.Workflow) NodeExecutor
		reason string
	}{
		{"an executor that cannot say what its nodes will hold", func(*ir.Workflow) NodeExecutor { return stubExecutorNoSurface{} },
			"counts as writing because executor runtime.stubExecutorNoSurface does not implement runtime.EffectiveToolSurfaceResolver"},
		{"an executor that cannot say where its nodes will run", func(wf *ir.Workflow) NodeExecutor {
			p := model.NewClawExecutor(model.NewRegistry(), wf)
			return forwardsToolsOnly{p, p}
		}, "counts as writing because executor runtime.forwardsToolsOnly does not implement runtime.EffectiveBackendResolver"},
	} {
		cannotAnswer := func(wf *ir.Workflow) *Engine {
			return &Engine{workflow: wf, executor: silent.mk(wf)}
		}
		t.Run(silent.name, func(t *testing.T) {
			t.Run("one unvouched branch beside a readonly one is admitted", func(t *testing.T) {
				wf := twoReaderBranches()
				wf.Nodes["b"].(*ir.AgentNode).Readonly = true
				if err := cannotAnswer(wf).validateWorkspaceSafety("fan", wf.Edges); err != nil {
					t.Fatalf("at most one mutating branch is allowed, and there is one: %v", err)
				}
				twin := twoReaderBranches(writes)
				twin.Nodes["b"].(*ir.AgentNode).Readonly = true
				if err := production(twin).validateWorkspaceSafety("fan", twin.Edges); err != nil {
					t.Fatalf("precondition: a declared writer beside a readonly branch is admitted: %v", err)
				}
			})
			t.Run("readonly branches are admitted", func(t *testing.T) {
				wf := twoReaderBranches(func(n *ir.AgentNode) { n.Readonly = true })
				if err := cannotAnswer(wf).validateWorkspaceSafety("fan", wf.Edges); err != nil {
					t.Fatalf("`readonly:` is the author's assertion and is honoured before the surface is read: %v", err)
				}
			})

			t.Run("a fan_out_each template replayed one at a time is admitted", func(t *testing.T) {
				if err := cannotAnswer(each).validateFanOutEachWorkspaceSafety("each", each.Edges[0], "", 3, 1); err != nil {
					t.Fatalf("max_parallel_branches=1 replays in sequence: %v", err)
				}
				if err := production(writerEach).validateFanOutEachWorkspaceSafety("each", writerEach.Edges[0], "", 3, 1); err != nil {
					t.Fatalf("precondition: a declared-writer template replayed one at a time is admitted: %v", err)
				}
			})
			t.Run("a fan_out_each template replayed concurrently is refused, naming the executor", func(t *testing.T) {
				if err := production(writerEach).validateFanOutEachWorkspaceSafety("each", writerEach.Edges[0], "", 3, 2); err == nil {
					t.Fatal("precondition: a declared-writer template replayed concurrently is refused")
				}
				err := cannotAnswer(each).validateFanOutEachWorkspaceSafety("each", each.Edges[0], "", 3, 2)
				rerr, ok := err.(*RuntimeError)
				if !ok || rerr.Code != ErrCodeWorkspaceSafety {
					t.Fatalf("err = %v, want the typed workspace-safety refusal", err)
				}
				if !strings.Contains(rerr.Message, `node "t" `+silent.reason) {
					t.Errorf("the fan_out_each refusal does not say the executor is why the template counts: %s", rerr.Message)
				}
			})
			t.Run("a parallel_safe tool the template exempts is not the reason given", func(t *testing.T) {
				fetch := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "fetch"}, Command: "curl -fsS https://example.invalid", ParallelSafe: true}
				wf := &ir.Workflow{
					Nodes: map[string]ir.Node{"each": router, "fetch": fetch, "t": agentNode("t", []string{"read_file"}, "claw", nil)},
					Edges: []*ir.Edge{{From: "each", To: "fetch"}, {From: "fetch", To: "t"}},
				}
				err := cannotAnswer(wf).validateFanOutEachWorkspaceSafety("each", wf.Edges[0], "", 3, 2)
				rerr, ok := err.(*RuntimeError)
				if !ok || rerr.Code != ErrCodeWorkspaceSafety {
					t.Fatalf("err = %v, want the typed workspace-safety refusal", err)
				}
				if !strings.Contains(rerr.Message, `node "t" `+silent.reason) || strings.Contains(rerr.Message, `node "fetch"`) {
					t.Errorf("the reason must be read in the template's own context, where `parallel_safe:` exempts fetch: %s", rerr.Message)
				}
			})
		})
	}
	t.Run("the production executor admits the same concurrent template", func(t *testing.T) {
		e := &Engine{workflow: each, executor: model.NewClawExecutor(model.NewRegistry(), each)}
		if err := e.validateFanOutEachWorkspaceSafety("each", each.Edges[0], "", 3, 2); err != nil {
			t.Fatalf("precondition: a reader template the production executor vouches for replays concurrently: %v", err)
		}
	})
}

// The executor is blamed only when its silence is what made a BRANCH count. A
// branch that writes on its own account anywhere along it is refused for that
// and never blamed on the executor, since implementing the method would not
// change its verdict, and a refusal telling the author to would send them the
// wrong way. These fixtures' reasons do not depend on what an executor answers,
// so the two refusals read the same, word for word.
func TestARefusalBlamesTheExecutorOnlyWhenItsSilenceDecided(t *testing.T) {
	chain := func() *ir.Workflow {
		router := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fan"}, RouterMode: ir.RouterFanOutAll}
		return &ir.Workflow{
			Nodes: map[string]ir.Node{
				"fan": router,
				"a":   agentNode("a", []string{"read_file"}, "claw", nil),
				"a2":  agentNode("a2", []string{"write_file"}, "claw", nil),
				"b":   agentNode("b", []string{"read_file"}, "claw", nil),
				"b2":  agentNode("b2", []string{"write_file"}, "claw", nil),
			},
			Edges: []*ir.Edge{{From: "fan", To: "a"}, {From: "fan", To: "b"}, {From: "a", To: "a2"}, {From: "b", To: "b2"}},
		}
	}
	for _, tc := range []struct {
		name   string
		wf     *ir.Workflow
		fanTo  int
		reason string // the reason every executor must give, when the row pins one
	}{
		{"each branch declares a writer", twoReaderBranches(func(n *ir.AgentNode) { n.Tools = []string{"write_file"} }), 2, ""},
		{"each branch reaches a declared writer after a reader", chain(), 2, ""},
		{"each branch is a parallel_safe tool, which a static fan-out does not exempt", twoBranches(func(id string) ir.Node {
			return &ir.ToolNode{BaseNode: ir.BaseNode{ID: id}, Command: "true", ParallelSafe: true}
		}), 2, ""},
		{"each branch has full_access", twoReaderBranches(func(n *ir.AgentNode) { n.FullAccess = true }), 2, ""},
		{"each branch is a subbot that does not assert isolation", twoBranches(func(id string) ir.Node {
			return &ir.SubbotNode{BaseNode: ir.BaseNode{ID: id}}
		}), 2, ""},
		{"each branch falls back to pi, a route no list bounds", twoReaderBranches(func(n *ir.AgentNode) {
			n.Fallbacks = []ir.Fallback{{Name: "alt", Backend: "pi"}}
		}), 2, `node "a" falls back to pi, where a ` + "`tools:`" + ` list does not bound what the node holds`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fan := tc.wf.Edges[:tc.fanTo]
			production := model.NewClawExecutor(model.NewRegistry(), tc.wf)
			answered := (&Engine{workflow: tc.wf, executor: production}).validateWorkspaceSafety("fan", fan)
			for _, silent := range []struct {
				name     string
				executor NodeExecutor
			}{
				{"forwarding neither seam", decoratorWithoutTheSeam{production}},
				{"forwarding the tool seam only", forwardsToolsOnly{production, production}},
			} {
				unanswered := (&Engine{workflow: tc.wf, executor: silent.executor}).validateWorkspaceSafety("fan", fan)
				if answered == nil || unanswered == nil {
					t.Fatalf("%s: both branches write on their own account and must be refused by any executor: answered=%v unanswered=%v", silent.name, answered, unanswered)
				}
				if strings.Contains(unanswered.Error(), "counts as writing because executor") {
					t.Errorf("%s: the refusal blames the executor for branches that write on their own account: %s", silent.name, unanswered.Error())
				}
				if answered.Error() != unanswered.Error() {
					t.Errorf("%s: a branch refused on its own account reads differently depending on the executor:\n answered:   %s\n unanswered: %s", silent.name, answered.Error(), unanswered.Error())
				}
				if tc.reason != "" && !strings.Contains(unanswered.Error(), tc.reason) {
					t.Errorf("%s: the refusal does not name the route that leaves the node unbounded:\n got: %s\nwant: …%s…", silent.name, unanswered.Error(), tc.reason)
				}
			}
		})
	}

	// The executor's own backend resolution counts as the branch's own
	// account: a node it routes to claude_code is unbounded by its list
	// whatever the tool seam would say.
	t.Run("the executor routes each branch where no list bounds it", func(t *testing.T) {
		wf := twoReaderBranches()
		silent := resolvesBackendTo{backend: "claude_code"}
		unanswered := (&Engine{workflow: wf, executor: silent}).validateWorkspaceSafety("fan", wf.Edges)
		answered := (&Engine{workflow: wf, executor: resolvesBackendToAndAnswers{silent}}).validateWorkspaceSafety("fan", wf.Edges)
		if unanswered == nil || answered == nil {
			t.Fatalf("two claude_code branches must be refused by any executor: answered=%v unanswered=%v", answered, unanswered)
		}
		if !strings.Contains(unanswered.Error(), `node "a" runs on claude_code, where a`) || unanswered.Error() != answered.Error() {
			t.Errorf("the reason must be the route the executor resolved, which implementing the tool seam would not change:\n answered:   %s\n unanswered: %s", answered.Error(), unanswered.Error())
		}
	})
}

// forwardsBothSeams is a wrapper that follows the refusal's remedy to the
// letter: it forwards the two methods admission reads, and nothing else.
type forwardsBothSeams struct {
	NodeExecutor
	inner *model.ClawExecutor
}

func (f forwardsBothSeams) EffectiveToolNames(node ir.Node, mayEscalateToUltracode bool) []string {
	return f.inner.EffectiveToolNames(node, mayEscalateToUltracode)
}

func (f forwardsBothSeams) EffectiveBackendName(node ir.Node) string {
	return f.inner.EffectiveBackendName(node)
}

// The refusal's remedy, followed to the letter, gives back the production
// verdict — including where a launch override routes a node elsewhere than its
// IR backend, which admission reads through EffectiveBackendName, not through
// the tool seam.
func TestFollowingTheRefusalsRemedyGivesBackTheProductionVerdict(t *testing.T) {
	toClaudeCode, err := model.ParseModelOverrides(nil, []string{"claude_code"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	verdict := func(err error) string {
		if err == nil {
			return "admitted"
		}
		return err.Error()
	}
	for _, tc := range []struct {
		name string
		opts []model.ClawExecutorOption
	}{
		{"no launch override", nil},
		{"a launch override routing every node to claude_code", []model.ClawExecutorOption{model.WithModelOverrides(toClaudeCode)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := twoReaderBranches()
			production := model.NewClawExecutor(model.NewRegistry(), wf, tc.opts...)
			want := verdict((&Engine{workflow: wf, executor: production}).validateWorkspaceSafety("fan", wf.Edges))
			got := verdict((&Engine{workflow: wf, executor: forwardsBothSeams{production, production}}).validateWorkspaceSafety("fan", wf.Edges))
			if got != want {
				t.Errorf("a wrapper forwarding both methods the refusal names reads differently from the executor it wraps:\n wrapper:    %s\n production: %s", got, want)
			}
		})
	}
}

// #1767's P12 case. A wrapper that forwards the tool seam and not the backend
// seam read the IR's backend, so under a launch override routing every node to
// claude_code it admitted two reader branches the production executor refuses.
// Half the remedy is now read at the worst case for the route it cannot report:
// refused where the production executor refuses, and wherever a declared
// writer would be, with the reason naming the seam it lacks.
func TestForwardingHalfTheRemedyIsStillReadAtTheWorstCase(t *testing.T) {
	toClaudeCode, err := model.ParseModelOverrides(nil, []string{"claude_code"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	override := []model.ClawExecutorOption{model.WithModelOverrides(toClaudeCode)}
	for _, tc := range []struct {
		name              string
		backend           string
		opts              []model.ClawExecutorOption
		productionRefuses bool
	}{
		{"claw readers under a launch override routing every node to claude_code", "claw", override, true},
		{"readers naming no backend under the same override", "", override, true},
		{"readers naming `auto` under the same override", "auto", override, true},
		{"claw readers with no launch override", "claw", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := twoReaderBranches(func(n *ir.AgentNode) { n.Backend = tc.backend })
			production := model.NewClawExecutor(model.NewRegistry(), wf, tc.opts...)
			if err := (&Engine{workflow: wf, executor: production}).validateWorkspaceSafety("fan", wf.Edges); (err != nil) != tc.productionRefuses {
				t.Fatalf("precondition: production refuses=%v, want %v (err=%v)", err != nil, tc.productionRefuses, err)
			}
			err := (&Engine{workflow: wf, executor: forwardsToolsOnly{production, production}}).validateWorkspaceSafety("fan", wf.Edges)
			rerr, ok := err.(*RuntimeError)
			if !ok || rerr.Code != ErrCodeWorkspaceSafety {
				t.Fatalf("a wrapper forwarding only EffectiveToolNames was admitted (err=%v): admission read the IR's backend, which a launch override never touches", err)
			}
			want := `node "a" counts as writing because executor runtime.forwardsToolsOnly does not implement runtime.EffectiveBackendResolver, so the route it will take cannot be read`
			if !strings.Contains(rerr.Message, want) {
				t.Errorf("the refusal does not name the seam the wrapper lacks:\n got: %s\nwant: …%s…", rerr.Message, want)
			}
		})
	}
}

// The sandbox reads the backend seam too, to decide whether to mount the claw
// runner, and that is not an admission question. An executor that cannot
// answer gets the engine's stand-in, never nil — and the sandbox reads the IR
// alone for it, exactly as for a nil resolver: the stand-in never names claw.
func TestTheSandboxReadsTheIRAloneForAnExecutorThatCannotAnswerTheBackendSeam(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend string
		want    bool
	}{
		{"an agent declaring claude_code", "claude_code", false},
		{"an agent declaring claw", "claw", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": agentNode("a", []string{"read_file"}, tc.backend, nil)}}
			e := &Engine{workflow: wf, executor: decoratorWithoutTheSeam{}}
			resolver := e.backendResolver()
			if _, ok := resolver.(unansweredBackend); !ok {
				t.Fatalf("precondition: an executor without EffectiveBackendName gets the stand-in, got %T", resolver)
			}
			got, fromIR := containsClawNode(wf, resolver), containsClawNode(wf, nil)
			if got != fromIR || got != tc.want {
				t.Errorf("mount decision = %v with the stand-in, %v reading the IR alone, want %v for both", got, fromIR, tc.want)
			}
		})
	}
}

// A `{{vars.…}}` backend is read pessimistically only because nothing read the
// template, and reading it is the backend seam's job. So an executor that
// cannot answer that seam is blamed for the refusal — the production executor,
// and a wrapper forwarding both methods, resolve the same template and admit —
// and the reason never quotes the template as if it were a backend.
func TestAnUnreadTemplateBackendIsTheExecutorsSilenceNotTheBranchsOwnAccount(t *testing.T) {
	wf := twoReaderBranches(func(n *ir.AgentNode) { n.Backend = "{{vars.b}}" })
	production := model.NewClawExecutor(model.NewRegistry(), wf)
	production.SetVars(map[string]any{"b": "claw"})
	if err := (&Engine{workflow: wf, executor: production}).validateWorkspaceSafety("fan", wf.Edges); err != nil {
		t.Fatalf("precondition: the production executor resolves {{vars.b}} to claw and admits two reader branches: %v", err)
	}
	if err := (&Engine{workflow: wf, executor: forwardsBothSeams{production, production}}).validateWorkspaceSafety("fan", wf.Edges); err != nil {
		t.Fatalf("precondition: a wrapper forwarding both methods gives back the production verdict: %v", err)
	}
	for _, silent := range []struct {
		name     string
		executor NodeExecutor
		reason   string
	}{
		{"forwarding neither seam", decoratorWithoutTheSeam{production},
			`node "a" counts as writing because executor runtime.decoratorWithoutTheSeam does not implement runtime.EffectiveToolSurfaceResolver or runtime.EffectiveBackendResolver`},
		{"forwarding the tool seam only", forwardsToolsOnly{production, production},
			`node "a" counts as writing because executor runtime.forwardsToolsOnly does not implement runtime.EffectiveBackendResolver`},
	} {
		t.Run(silent.name, func(t *testing.T) {
			err := (&Engine{workflow: wf, executor: silent.executor}).validateWorkspaceSafety("fan", wf.Edges)
			if err == nil {
				t.Fatal("two branches whose route nobody can read were admitted onto one shared worktree")
			}
			if !strings.Contains(err.Error(), silent.reason) || strings.Contains(err.Error(), "runs on {{") {
				t.Errorf("the refusal must blame the executor whose silence left the template unread, and never quote the template as a backend:\n got: %s\nwant: …%s…", err.Error(), silent.reason)
			}
		})
	}
}

// The sandbox asks the backend seam on every run, fan-out or not; the warning
// is about what admission does, so it is logged at the first parallel-branch
// admission and nowhere earlier.
func TestTheSeamWarningIsLoggedAtAdmissionNotAtTheSandboxsQuestion(t *testing.T) {
	var buf bytes.Buffer
	wf := twoReaderBranches()
	e := &Engine{workflow: wf, executor: decoratorWithoutTheSeam{}, logger: iterlog.New(iterlog.LevelWarn, &buf)}
	_ = e.backendResolver()
	_ = e.toolSurfaceResolver()
	if buf.Len() != 0 {
		t.Fatalf("asking a seam outside admission — as the sandbox's setup does on every run — warned: %q", buf.String())
	}
	if err := e.validateWorkspaceSafety("fan", wf.Edges); err == nil {
		t.Fatal("precondition: the fan-out is refused")
	}
	if got := strings.Count(buf.String(), "does not implement"); got != 1 {
		t.Errorf("admission warned %d times, want once: %q", got, buf.String())
	}
}

// The backend seam replaces a node's IR backend rather than adding to it: a
// launch override routes a `backend: claude_code` node onto claw, where its
// reader list bounds it. So while an executor cannot answer that seam, the
// route the IR names is not the branch's own account — the refusal names the
// executor, not a route the node may never take.
func TestARouteTheIRNamesIsTheExecutorsToAnswer(t *testing.T) {
	wf := twoReaderBranches(func(n *ir.AgentNode) { n.Backend = "claude_code" })
	toClaw, err := model.ParseModelOverrides(nil, []string{"claw"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	production := model.NewClawExecutor(model.NewRegistry(), wf, model.WithModelOverrides(toClaw))
	if err := (&Engine{workflow: wf, executor: production}).validateWorkspaceSafety("fan", wf.Edges); err != nil {
		t.Fatalf("precondition: the production executor routes both claude_code readers onto claw and admits them: %v", err)
	}
	if err := (&Engine{workflow: wf, executor: forwardsBothSeams{production, production}}).validateWorkspaceSafety("fan", wf.Edges); err != nil {
		t.Fatalf("precondition: forwarding both methods gives back the production verdict: %v", err)
	}
	err = (&Engine{workflow: wf, executor: forwardsToolsOnly{production, production}}).validateWorkspaceSafety("fan", wf.Edges)
	if err == nil {
		t.Fatal("two branches whose route nobody can read were admitted onto one shared worktree")
	}
	want := `node "a" counts as writing because executor runtime.forwardsToolsOnly does not implement runtime.EffectiveBackendResolver`
	if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "runs on claude_code") {
		t.Errorf("the refusal must name the executor that hid the route, not the route the IR names:\n got: %s\nwant: …%s…", err.Error(), want)
	}
}

// Neither route a declaration bounds is the more permissive one for every node:
// claw un-restricts a list the moment a CLI fallback exists, while a codex
// reader whose fallback is codex holds only its list. So a codex reader with a
// codex fallback does not write on its own account, and while the backend seam
// is silent the refusal names the executor — never a bare node.
func TestTheOwnAccountIsReadOnEveryRouteADeclarationBounds(t *testing.T) {
	wf := twoReaderBranches(func(n *ir.AgentNode) {
		n.Backend = "codex"
		n.Fallbacks = []ir.Fallback{{Name: "alt", Backend: "codex"}}
	})
	production := model.NewClawExecutor(model.NewRegistry(), wf)
	if err := (&Engine{workflow: wf, executor: production}).validateWorkspaceSafety("fan", wf.Edges); err != nil {
		t.Fatalf("precondition: the production executor admits two codex readers whose fallback is codex: %v", err)
	}
	// On claw the same codex fallback does count — a claw list stops meaning
	// what it means once a CLI route exists — and the reason must not claim
	// codex leaves a list unbounded, which it does not.
	onClaw := twoReaderBranches(func(n *ir.AgentNode) { n.Fallbacks = []ir.Fallback{{Name: "alt", Backend: "codex"}} })
	clawErr := (&Engine{workflow: onClaw, executor: model.NewClawExecutor(model.NewRegistry(), onClaw)}).validateWorkspaceSafety("fan", onClaw.Edges)
	if clawErr == nil {
		t.Fatal("precondition: claw readers with a codex fallback are refused by the claw→CLI rule")
	}
	if strings.Contains(clawErr.Error(), "falls back to codex, where") {
		t.Errorf("the reason claims codex leaves a `tools:` list unbounded: %s", clawErr.Error())
	}
	for _, silent := range []struct {
		name     string
		executor NodeExecutor
		reason   string
	}{
		{"forwarding neither seam", decoratorWithoutTheSeam{production}, `node "a" counts as writing because executor runtime.decoratorWithoutTheSeam does not implement`},
		{"forwarding the tool seam only", forwardsToolsOnly{production, production}, `node "a" counts as writing because executor runtime.forwardsToolsOnly does not implement runtime.EffectiveBackendResolver`},
	} {
		t.Run(silent.name, func(t *testing.T) {
			err := (&Engine{workflow: wf, executor: silent.executor}).validateWorkspaceSafety("fan", wf.Edges)
			if err == nil {
				t.Fatal("two branches whose route nobody can read were admitted onto one shared worktree")
			}
			if !strings.Contains(err.Error(), silent.reason) {
				t.Errorf("the refusal must name the executor whose silence decided it:\n got: %s\nwant: …%s…", err.Error(), silent.reason)
			}
		})
	}
}

// A route still spelled as a template when the reason is written was read
// pessimistically BECAUSE nothing resolved it, so the reason never names it as
// a backend — for a primary route an executor left unresolved, and for a
// fallback, which no executor resolves before the run.
func TestAReasonNeverNamesATemplateAsABackend(t *testing.T) {
	t.Run("a templated fallback", func(t *testing.T) {
		wf := twoReaderBranches(func(n *ir.AgentNode) { n.Fallbacks = []ir.Fallback{{Name: "alt", Backend: "{{vars.fb}}"}} })
		production := model.NewClawExecutor(model.NewRegistry(), wf)
		production.SetVars(map[string]any{"fb": "claw"})
		err := (&Engine{workflow: wf, executor: production}).validateWorkspaceSafety("fan", wf.Edges)
		if err == nil {
			t.Fatal("precondition: a fallback left to a template is read pessimistically and refused")
		}
		if strings.Contains(err.Error(), "falls back to {{") {
			t.Errorf("the reason names a template as the backend the node falls back to: %s", err.Error())
		}
	})
	t.Run("a templated primary an executor answers with nothing", func(t *testing.T) {
		wf := twoReaderBranches(func(n *ir.AgentNode) { n.Backend = "{{vars.b}}" })
		err := (&Engine{workflow: wf, executor: resolvesBackendToAndAnswers{resolvesBackendTo{backend: ""}}}).validateWorkspaceSafety("fan", wf.Edges)
		if err == nil {
			t.Fatal("precondition: a primary left to a template nothing resolved is read pessimistically and refused")
		}
		if strings.Contains(err.Error(), "runs on {{") {
			t.Errorf("the reason names a template as the backend the node runs on: %s", err.Error())
		}
	})
}
