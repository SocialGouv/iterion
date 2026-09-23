package runtime

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// Workspace mutation safety
// ---------------------------------------------------------------------------

// readOnlyTools is the set of built-in tool names that are guaranteed to
// never modify the workspace. These are safe for parallel execution.
//
// Two vocabularies live here on purpose. `glob`, `grep`, `read_file` and
// `web_fetch` are claw's REAL read-only tools — the names a claw node must
// use, since claw resolves its `tools:` list against the registry (C135). The
// others (`git_diff`, `git_status`, `list_files`, `search_codebase`, `tree`)
// are not registered anywhere: they are declarative intent on CLI-backend
// nodes, whose lists are advisory, and dozens of catalog bots express
// "this reviewer only reads" with them. Dropping them would silently reclassify
// those nodes as mutating and change parallel-branch admission, so they stay.
//
// The claw names were missing until 2026-08, which made the C135 migration
// (`list_files` → `glob`, `search_codebase` → `grep`) flip a read-only claw
// reviewer to mutating — tightening admission for a rename that changed
// nothing about what the node can do.
var readOnlyTools = map[string]bool{
	// claw built-ins that only read.
	"read_file": true,
	"glob":      true,
	"grep":      true,
	"web_fetch": true,
	// Declarative intent on CLI backends, where the list is advisory.
	"git_diff":        true,
	"git_status":      true,
	"list_files":      true,
	"search_codebase": true,
	"tree":            true,
}

// nonWorkspaceTools names the tools the RUNTIME opens for its own plumbing,
// which act somewhere other than the shared worktree.
//
// It is deliberately NOT merged into readOnlyTools. That map answers a wider
// question — "can this node act at all" — and two other readers depend on that
// reading: pkg/runtime's own declared-tools contract, and the catalog guard
// that decides which bot nodes must carry the UNTRUSTED INPUT BOUNDARY
// paragraph. A name added there would silently take a node out of that class.
// This map is read by ONE arm, the effective-surface check below, and answers
// only "does this touch the workspace two parallel branches share".
//
// `todo_write` writes a file, and out of tree on purpose: claw keeps the list
// outside the repository precisely because a copy inside it dirtied git status
// on every run. `ask_user` and its async pair are question channels. The
// board/runs MCP tools act on the board and the run store, recognised through
// the delegate package's own predicate rather than a prefix spelled here.
var nonWorkspaceTools = map[string]bool{
	"ask_user":       true,
	"ask_user_async": true,
	"await_answers":  true,
	"todo_write":     true,
	"read_image":     true,
}

// toolIsWorkspaceSafe reports whether holding this tool leaves the shared
// worktree untouched.
func toolIsWorkspaceSafe(name string) bool {
	return readOnlyTools[name] || nonWorkspaceTools[name] || delegate.IsIterionMCPTool(name)
}

// effectiveToolSurfaceResolver is implemented by the production model
// executor. A node's `tools:` list is not the list it holds: the runtime folds
// its own opt-ins over it at build time, from state that is not in the IR (a
// launch-time --auto-memory, a per-node model override raising the node to
// ultracode). So the guard asks the executor for the effective surface instead
// of re-deriving the append rules — the same seam, and the same reason, as
// effectiveBackendResolver.
//
// Admission runs when a fan-out router DISPATCHES, once per invocation, not
// before the run: the router's own output is already resolved by then, but the
// per-branch mappings are not read here, so a value an edge would inject is
// taken pessimistically rather than resolved. That is a deliberate
// simplification of a guard that must answer for a whole branch, not a claim
// that the value is unknowable.
type effectiveToolSurfaceResolver interface {
	EffectiveToolNames(node ir.Node, mayEscalateToUltracode bool) []string
}

// toolSurfaceResolver is the ONE place the engine asks its executor what a
// node will really hold. Returns nil for an executor that cannot answer (a
// stub), which every caller reads as "the declaration is all there is".
func (e *Engine) toolSurfaceResolver() effectiveToolSurfaceResolver {
	if e == nil {
		return nil
	}
	r, _ := e.executor.(effectiveToolSurfaceResolver)
	return r
}

// isMutatingNode returns true if the node may modify the workspace.
// Tool nodes are always mutating in this general classifier. Agent/judge nodes
// are mutating when full_access is set, when they have at least one tool that is
// not in the read-only set, or when the effective backend is a CLI delegate and
// its tool list is omitted (CLI delegates treat an empty list as unrestricted
// native tools). The engine asks the production executor for the effective
// backend so launch overrides, environment defaults, and auto-detection are
// included. Subbot nodes run a child .bot that may do anything (including mutate
// the shared worktree), so they are conservatively treated as mutating — this
// keeps validateWorkspaceSafety from admitting two subbot branches that would
// race the same workspace. A subbot may opt out with `isolated:` (it asserts
// the child confines writes to its own run store / worktree) and an agent/judge
// node with Readonly=true; both are context-independent, so they are honoured
// everywhere. A tool's `parallel_safe:` opt-out is NOT honoured here: it only
// applies to a fan_out_each template (see isMutatingNodeCtx), because only there
// is a single node replayed over distinct items with disjoint, item-keyed
// writes — in a static fan_out_all / llm-router the branches are different nodes
// with no such guarantee.
func isMutatingNode(node ir.Node) bool {
	return isMutatingNodeWithBackend(node, "", nil, nil)
}

// effectiveBackendResolver is implemented by the production model executor.
// Keeping the interface here avoids duplicating its evolving resolution chain
// (launch override -> DSL -> workflow default -> env -> auto-detection).
type effectiveBackendResolver interface {
	EffectiveBackendName(ir.Node) string
}

// backendResolver is the ONE place the engine asks its executor for the
// dispatch-time backend resolution. Every pre-run analysis that keys on a
// node's backend — workspace-safety admission, the sandbox's claw
// bind-mount — reads it here rather than re-deriving from the IR, which
// would silently miss the launch-time `--backend` / `--model` overrides.
// Returns nil for an executor that does not resolve backends (a stub),
// which every caller reads as "the IR is all there is".
func (e *Engine) backendResolver() effectiveBackendResolver {
	if e == nil {
		return nil
	}
	r, _ := e.executor.(effectiveBackendResolver)
	return r
}

func isMutatingNodeWithBackend(node ir.Node, defaultBackend string, resolver effectiveBackendResolver, surfaces effectiveToolSurfaceResolver) bool {
	return isMutatingNodeCtx(node, defaultBackend, resolver, surfaces, false)
}

// isMutatingNodeCtx classifies a node for workspace-safety. fanOutEachTemplate is
// true only when walking a fan_out_each template branch; there a tool marked
// `parallel_safe:` is treated as non-mutating (the author asserts its concurrent
// replays write only to disjoint, item-keyed targets — mirror of a subbot's
// `isolated:` / an agent-judge `readonly:`, but scoped to the fan_out_each
// replay, the only place a single template node is fanned out over items). In
// every other context (fanOutEachTemplate=false) a tool is conservatively
// mutating even with the flag: static fan_out_all / llm-router branches are
// DISTINCT nodes with no item-key disjointness guarantee. Subbot Isolated and
// agent/judge Readonly are context-independent and honoured in both.
func isMutatingNodeCtx(node ir.Node, defaultBackend string, resolver effectiveBackendResolver, surfaces effectiveToolSurfaceResolver, fanOutEachTemplate bool) bool {
	return isMutatingNodeIn(nil, node, defaultBackend, resolver, surfaces, fanOutEachTemplate)
}

// isMutatingNodeIn is isMutatingNodeCtx with the graph in hand, so it can tell
// whether an incoming edge may raise the node to ultracode at dispatch — an
// input that does not exist before the run and that widens the node's tool
// surface when it arrives.
func isMutatingNodeIn(wf *ir.Workflow, node ir.Node, defaultBackend string, resolver effectiveBackendResolver, surfaces effectiveToolSurfaceResolver, fanOutEachTemplate bool) bool {
	escalates := edgeMayEscalateEffort(wf, node)
	switch n := node.(type) {
	case *ir.ToolNode:
		return !fanOutEachTemplate || !n.ParallelSafe
	case *ir.SubbotNode:
		// A subbot marked `isolated:` asserts the child confines its writes to
		// its own run store / worktree and never touches the parent's shared
		// workspace, so it is safe to fan out in parallel (mirror of an
		// agent/judge node's `readonly:`). Absent the flag, the child may do
		// anything — conservatively mutating.
		return !n.Isolated
	case *ir.AgentNode:
		if n.Readonly {
			return false
		}
		return llmToolSurfaceCanWrite(node, n.LLMFields, n.Tools, defaultBackend, resolver, surfaces, escalates, true, nil)
	case *ir.JudgeNode:
		if n.Readonly {
			return false
		}
		return llmToolSurfaceCanWrite(node, n.LLMFields, n.Tools, defaultBackend, resolver, surfaces, escalates, true, nil)
	}
	return false
}

// IsReadOnlyTool reports whether a declared tool name belongs to the
// read-only vocabulary above — the names an agent/judge may hold without
// being able to change the workspace.
func IsReadOnlyTool(name string) bool {
	return readOnlyTools[name]
}

// ToolSurfaceCanWrite reports whether an agent/judge node's EFFECTIVE tool
// surface can act on the workspace: `full_access:`, a declared tool outside
// the read-only vocabulary, or an omitted `tools:` list on a backend where
// omission means the full native toolset (a CLI delegate, or claw when a
// `fallbacks:` route reaches one). Every other node kind reports false.
//
// `readonly:` is deliberately NOT consulted. The codex and pi delegates
// enforce it as a sandbox mode; claude_code never reads it and runs under
// bypassPermissions with whatever the tool list leaves visible. It is
// therefore a scheduling assertion — isMutatingNodeCtx honours it for
// parallel-branch admission — and not a bound on what the node can do, which
// is the question a prompt-injection boundary asks.
//
// lookup resolves `${VAR:-default}` references in backend names: nil reads
// the process environment (what a run does), a lookup that returns "" for
// every name reads the authored defaults alone (what a catalog guard wants,
// so its verdict does not depend on the host it runs on).
func ToolSurfaceCanWrite(node ir.Node, defaultBackend string, lookup func(string) string) bool {
	switch n := node.(type) {
	case *ir.AgentNode:
		return llmToolSurfaceCanWrite(node, n.LLMFields, n.Tools, defaultBackend, nil, nil, false, false, lookup)
	case *ir.JudgeNode:
		return llmToolSurfaceCanWrite(node, n.LLMFields, n.Tools, defaultBackend, nil, nil, false, false, lookup)
	}
	return false
}

// llmToolSurfaceCanWrite is the readonly-free half of the agent/judge
// classification, shared by ToolSurfaceCanWrite and isMutatingNodeCtx so the
// two cannot drift on what "can write" means.
func llmToolSurfaceCanWrite(
	node ir.Node,
	fields ir.LLMFields,
	tools []string,
	defaultBackend string,
	resolver effectiveBackendResolver,
	surfaces effectiveToolSurfaceResolver,
	escalatesToUltracode bool,
	sharedWorkspace bool,
	lookup func(string) string,
) bool {
	if fields.FullAccess || unrestrictedCLIBackendCanWrite(node, fields, tools, defaultBackend, resolver, lookup, sharedWorkspace) {
		return true
	}
	for _, t := range tools {
		if !readOnlyTools[t] {
			return true
		}
	}
	// The declared list is not the list the node holds. `assembleEffectiveTools`
	// folds the runtime's own opt-ins over it, and two of them widen the
	// workspace surface: `auto_memory: on` grants write_file on claw, and
	// ultracode grants claw's unbounded `agent` subagent tool. A node that
	// declared `tools: [read_file]` and is admitted read-only on that basis
	// then shares one worktree with N siblings while holding a writer.
	//
	// Asked, never re-derived: both opt-ins resolve outside the IR.
	if surfaces != nil {
		for _, t := range surfaces.EffectiveToolNames(node, escalatesToUltracode) {
			if !toolIsWorkspaceSafe(t) {
				return true
			}
		}
	}
	return false
}

func unrestrictedCLIBackendCanWrite(
	node ir.Node,
	fields ir.LLMFields,
	tools []string,
	defaultBackend string,
	resolver effectiveBackendResolver,
	lookup func(string) string,
	sharedWorkspace bool,
) bool {
	backend := strings.TrimSpace(ir.ExpandWithDefault(fields.Backend, lookup))
	if backend == "" {
		backend = strings.TrimSpace(ir.ExpandWithDefault(defaultBackend, lookup))
	}
	if resolver != nil {
		if effective := strings.TrimSpace(resolver.EffectiveBackendName(node)); effective != "" {
			backend = effective
		}
	}
	// A `{{vars.…}}` backend the resolver did not read (a nil resolver, a
	// stub) may be claw with a CLI route or a CLI backend outright:
	// admission is decided once, before the run, so it is pessimistic —
	// mutating — rather than read-only and eligible for a shared worktree.
	if strings.Contains(backend, "{{") {
		return true
	}
	// A DECLARED-EMPTY `tools: []` reaches exactly the verdict an undeclared
	// list reaches, and that is deliberate: it is not a proof that the node
	// holds nothing. On claude_code the bound is `--disallowedTools` over
	// `claudeNativeTools`, a hardcoded 14-name enumeration of a roster
	// iterion does not own — the package's own `orchestrationTools` names
	// `Agent`, `TaskOutput` and `Monitor` outside it, and MCP tools are not
	// on it either, so all of those survive. On claw the runtime's own
	// `interaction:` append puts `ask_user` back, and the appends below it
	// `todo_write` and, under `auto_memory:`, `write_file` (C270 says so at
	// compile time). The loop below cannot see any of it: it iterates the
	// declared names, and there are none.
	//
	// A claw→CLI route un-restricts the node's tool set WHATEVER it
	// declared: under the always-on bypassPermissions, claude_code
	// ignores the lowercase `tools:` list entirely and always carries
	// the full native toolset. So this is checked BEFORE the
	// list-based early return — a `tools: [read_file]` claw node with a
	// CLI route still gains Edit/Write on fall-through, and admission
	// happens once, before the run.
	if backend == "claw" && fallbacksReachCLIBackend(node, lookup) {
		return true
	}
	// A declaration only bounds a route that can be bounded BY it, and two
	// kinds cannot.
	//
	// pi, kimi, grok and opencode never receive the list at all (C270 says so
	// at compile time), so `tools: [read_file]` there is a note to the reader
	// while the agent keeps its own full toolset.
	//
	// claude_code receives it, and still is not bounded by it: the list
	// becomes `--disallowedTools` over a CLOSED native roster this project
	// does not own, so it removes the names iterion happens to enumerate and
	// nothing else. Which names survive is a property of the installed CLI,
	// not of the declaration — measured on 2.1.220, the surviving set includes
	// tools that move the worktree the session acts in. #1671 already drew
	// this conclusion for `tools: []`; a non-empty list has no better claim,
	// and a guard that tried to enumerate the survivors found a new spelling
	// every round. So the rule names no tool: on a route whose declaration is
	// not a bound, the declaration proves nothing, and `readonly:` — the
	// scheduling assertion the engine already documents, honoured before this
	// is ever reached — is how an author says otherwise.
	//
	// An unnamed backend is deliberately NOT included: `backend: ""` is
	// resolved at dispatch, and a pre-run reading of it would be a guess.
	// Widening that one is a separate question with its own measure.
	//
	// A DECLARED-EMPTY list is covered as well as a named one: reading the
	// stricter declaration as safer than the looser one would invert the guard.
	if (len(tools) > 0 || toolcatalog.ToolsDeclared(tools)) && routeDeclarationIsNoBound(node, backend, lookup, sharedWorkspace) {
		return true
	}
	if len(tools) > 0 {
		return false
	}
	if backend == "" || backend == "auto" {
		return false
	}
	if backend != "claw" {
		return true
	}
	// The node resolves to claw, where an empty tools list means ZERO
	// tools. But a `fallbacks:` route (ADR-087) on a CLI backend would
	// run that same empty list as the FULL unrestricted native toolset
	// under bypassPermissions — so a node admitted here as read-only
	// could start writing the moment its chain falls through.
	//
	// Admission is decided ONCE, before the run, so it must be
	// pessimistic over the WHOLE chain: mutating if ANY route would be.
	// The cost is real and accepted — a tools-less claw node that
	// declares a CLI route stops being eligible for parallel read-only
	// fan-out — and it is the right trade against N concurrent writers
	// racing on one worktree with every guard already passed.
	return fallbacksReachCLIBackend(node, lookup)
}

// routeDeclarationIsNoBound reports whether the node's primary backend, or any
// of its `fallbacks:` routes, runs somewhere a `tools:` declaration does not
// bound what the node holds. An empty/auto name is not one: it is resolved at
// dispatch and answered by the arm that follows.
func routeDeclarationIsNoBound(node ir.Node, backend string, lookup func(string) string, sharedWorkspace bool) bool {
	if declarationIsNoBound(backend, sharedWorkspace) {
		return true
	}
	llm, ok := node.(ir.LLMNode)
	if !ok {
		return false
	}
	for _, fb := range llm.GetFallbacks() {
		b := strings.TrimSpace(ir.ExpandWithDefault(fb.Backend, lookup))
		if declarationIsNoBound(b, sharedWorkspace) {
			return true
		}
	}
	return false
}

// declarationIsNoBound answers for ONE named backend, and names no tool.
//
// Two routes fail to bound a declaration, for two different reasons. pi, kimi,
// grok and opencode never receive the list at all (C270's subject), so the
// agent keeps its own toolset whatever the author wrote. claude_code does
// receive it and still is not bounded by it: the list becomes
// `--disallowedTools` over a CLOSED native roster this project does not own, so
// it removes the names iterion happens to enumerate and nothing else — measured
// on CLI 2.1.220, a node declaring `tools: [read_file]` still registers
// `EnterWorktree` and `ExitWorktree`, which move the worktree the session acts
// in.
//
// Both answers are scoped to the SHARED-WORKSPACE question — "may two branches
// run on one worktree" — and deliberately not to the wider one
// `ToolSurfaceCanWrite` answers for the catalog's prompt-injection contract
// ("what can this node do"). The two are the same fact read at two altitudes,
// and moving the second one re-classifies bots across the catalog: a real
// change, with its own measure and its own review, and not this one.
//
// `sharedWorkspace` is therefore a parameter of the QUESTION, passed by each
// entry point, and never derived from what the engine's executor happens to
// implement: keyed on the resolver, `iterion validate --exec --strict` — whose
// dry-run executor implements no tool surface — answered OK on a workflow the
// same tree kills at the router.
func declarationIsNoBound(backend string, sharedWorkspace bool) bool {
	b := strings.TrimSpace(backend)
	if b == "" || b == "auto" || !sharedWorkspace {
		return false
	}
	return !toolcatalog.ReceivesToolList(b) || b == delegate.BackendClaudeCode
}

// edgeMayEscalateEffort reports whether any edge INTO this node may hand it
// `_reasoning_effort: "ultracode"`, which grants claw's unbounded `agent` tool
// — a widening this classifier would otherwise miss, because the mapped value
// is resolved when the router dispatches.
//
// A LITERAL mapping is decided here and now: `_reasoning_effort: "low"` can
// never be ultracode, and reading every effort edge as a possible escalation
// refused fan-outs that are perfectly safe. Only a mapping carrying references
// (`{{outputs.…}}`, `{{vars.…}}`) is unknowable at this point, and only that
// one is read pessimistically.
//
// With no graph in hand the answer is false: the node's own declaration is
// then all there is to read.
func edgeMayEscalateEffort(wf *ir.Workflow, node ir.Node) bool {
	if wf == nil || node == nil {
		return false
	}
	id := node.NodeID()
	for _, edge := range wf.Edges {
		if edge == nil || edge.To != id {
			continue
		}
		for _, m := range edge.With {
			if m == nil || m.Key != dynamicEffortInputKey {
				continue
			}
			if len(m.Refs) > 0 {
				return true // a template; its value arrives later
			}
			// Compared EXACTLY, against the value the compiler produces
			// (the parser has already stripped the quotes). Trimming would
			// escalate on `" ultracode "`, which the runtime itself refuses —
			// ir.ValidReasoningEfforts has no such key — so the guard would
			// refuse a fan-out for an escalation that cannot happen.
			if m.Raw == ultracodeEffort {
				return true
			}
		}
	}
	return false
}

// ultracodeEffort is the one `_reasoning_effort` value that widens a node's
// tool surface: it is the mode that grants the orchestration prerogative, not
// an API effort level (see ir.ValidReasoningEfforts, which carries it).
const ultracodeEffort = "ultracode"

// dynamicEffortInputKey is the mapped input an edge raises a node's reasoning
// effort with (see model.resolveReasoningEffort). Spelled once here because
// the model package keeps its own copy unexported.
const dynamicEffortInputKey = "_reasoning_effort"

// fallbacksReachCLIBackend reports whether any of a node's `fallbacks:`
// routes runs on a backend where an empty `tools:` list means the full
// native toolset rather than none.
func fallbacksReachCLIBackend(node ir.Node, lookup func(string) string) bool {
	llm, ok := node.(ir.LLMNode)
	if !ok {
		return false
	}
	for _, fb := range llm.GetFallbacks() {
		b := strings.TrimSpace(ir.ExpandWithDefault(fb.Backend, lookup))
		if b == "" || b == "auto" {
			continue // inherits the node's backend, which is claw here
		}
		if b != "claw" {
			return true
		}
	}
	return false
}

// branchContainsMutation walks from startNodeID to globalConvergence (or to a
// terminal node) and returns true if any node along the path may mutate the
// workspace.
//
// The previous implementation stopped walking at the FIRST node with
// AwaitMode != AwaitNone — i.e. at any intermediate join — which meant that
// in a topology like
//
//	router(fan_out_all) -> A -> joinA -> mutA -> globalJoin
//	                    -> B -> joinB -> mutB -> globalJoin
//
// the BFS treated `joinA` / `joinB` as the stopping point and never saw
// `mutA` or `mutB`. Both branches passed validateWorkspaceSafety, then ran
// in parallel and raced on the shared workspace (e.g. git index).
//
// The correct stopping condition is the GLOBAL convergence point of the
// fan-out (the node where all branches reconverge), not the first
// intermediate join. We pass that in explicitly. Terminal nodes (done/fail)
// also stop the walk because the branch ends there.
//
// fanOutEachTemplate is true only when the branch is a fan_out_each template
// (one node replayed over items); it relaxes a `parallel_safe:` tool to
// non-mutating along the walk. A branch that also contains any OTHER mutating
// node (a non-parallel_safe tool, a full_access agent, a non-isolated subbot)
// is still reported as mutating.
func (e *Engine) branchContainsMutation(startNodeID, globalConvergence string, fanOutEachTemplate bool) bool {
	visited := map[string]bool{}
	queue := []string{startNodeID}
	for len(queue) > 0 {
		nodeID := queue[0]
		queue = queue[1:]
		if visited[nodeID] {
			continue
		}
		visited[nodeID] = true

		// Stop at the global convergence point — beyond it, nodes are
		// post-fan-out and shared by all branches sequentially.
		if globalConvergence != "" && nodeID == globalConvergence {
			continue
		}

		node, ok := e.workflow.Nodes[nodeID]
		if !ok {
			continue
		}
		// Stop walking at terminal nodes.
		if isTerminalNode(node) {
			continue
		}
		if isMutatingNodeIn(e.workflow, node, e.workflow.DefaultBackend, e.backendResolver(), e.toolSurfaceResolver(), fanOutEachTemplate) {
			return true
		}
		for _, edge := range e.workflow.Edges {
			if edge.From == nodeID {
				queue = append(queue, edge.To)
			}
		}
	}
	return false
}

// validateWorkspaceSafety checks that at most one branch in a fan-out
// contains mutating nodes. Returns an error if the topology is unsafe.
//
// routerNodeID + fanEdges are used to compute the global convergence point
// up-front; we pass it down to branchContainsMutation so the BFS doesn't
// stop early at intermediate joins.
func (e *Engine) validateWorkspaceSafety(routerNodeID string, fanEdges []*ir.Edge) error {
	globalConvergence := e.findConvergencePoint(routerNodeID, fanEdges)
	mutatingCount := 0
	var mutatingBranches []string
	for _, edge := range fanEdges {
		// Static fan_out_all / llm-router: branches are DISTINCT nodes, so a
		// tool's `parallel_safe:` (item-keyed disjoint replays) does not apply —
		// pass fanOutEachTemplate=false.
		if e.branchContainsMutation(edge.To, globalConvergence, false) {
			mutatingCount++
			mutatingBranches = append(mutatingBranches, edge.To)
		}
	}
	if mutatingCount > 1 {
		return &RuntimeError{
			Code: ErrCodeWorkspaceSafety,
			Message: fmt.Sprintf("workspace safety violation: %d branches contain mutating nodes %v%s",
				mutatingCount, mutatingBranches, e.mutationCauses(mutatingBranches, globalConvergence)),
			Hint: "at most 1 mutating branch is allowed in parallel on the same workspace; move the mutating work to sequential steps, or assert the branch is safe where the node kind allows it — `readonly:` on an agent/judge, `isolated:` on a subbot, `parallel_safe:` on a fan_out_each tool",
		}
	}
	return nil
}

// mutationCauses names, per refused branch, the first node that can write and
// the tool that decides it. The verdict is computed from a surface the author
// never wrote — `auto_memory:` grants write_file, ultracode grants the subagent
// tool, a route that ignores the list grants everything — so a message naming
// only the branch sends the reader looking for a tool node that is not there.
// Recomputed on the error path alone.
func (e *Engine) mutationCauses(branches []string, convergence string) string {
	var parts []string
	for _, b := range branches {
		if cause := e.branchMutationCause(b, convergence); cause != "" {
			parts = append(parts, cause)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " — " + strings.Join(parts, "; ")
}

func (e *Engine) branchMutationCause(startNodeID, globalConvergence string) string {
	visited := map[string]bool{}
	queue := []string{startNodeID}
	surfaces := e.toolSurfaceResolver()
	for len(queue) > 0 {
		nodeID := queue[0]
		queue = queue[1:]
		if visited[nodeID] {
			continue
		}
		visited[nodeID] = true
		if globalConvergence != "" && nodeID == globalConvergence {
			continue
		}
		node, ok := e.workflow.Nodes[nodeID]
		if !ok || isTerminalNode(node) {
			continue
		}
		if !isMutatingNodeIn(e.workflow, node, e.workflow.DefaultBackend, e.backendResolver(), surfaces, false) {
			for _, edge := range e.workflow.Edges {
				if edge.From == nodeID {
					queue = append(queue, edge.To)
				}
			}
			continue
		}
		if surfaces != nil {
			// Declared names are compared through the shared spelling table,
			// not literally: the surface carries the runtime's own vocabulary
			// and an author's `agent` is the same tool as a backend's `Agent`.
			declared := map[string]bool{}
			for _, t := range llmDeclaredTools(node) {
				declared[toolcatalog.CanonicalToolName(t)] = true
			}
			for _, t := range surfaces.EffectiveToolNames(node, edgeMayEscalateEffort(e.workflow, node)) {
				if toolIsWorkspaceSafe(t) || declared[toolcatalog.CanonicalToolName(t)] {
					continue
				}
				return fmt.Sprintf("node %q holds %q, which its `tools:` list does not declare", nodeID, t)
			}
		}
		if llm, ok := node.(ir.LLMNode); ok {
			// Read exactly as the classifier read it, expansion included: a
			// message quoting `${VAR:-claw}` as if it were a backend, and
			// blaming a rule the verdict never used, is worse than no message.
			backend := strings.TrimSpace(ir.ExpandWithDefault(llm.GetLLMFields().Backend, nil))
			if backend == "" {
				backend = strings.TrimSpace(ir.ExpandWithDefault(e.workflow.DefaultBackend, nil))
			}
			if r := e.backendResolver(); r != nil {
				if eff := strings.TrimSpace(r.EffectiveBackendName(node)); eff != "" {
					backend = eff
				}
			}
			if declarationIsNoBound(backend, true) { // the cause of a shared-worktree refusal
				return fmt.Sprintf("node %q runs on %s, where a `tools:` list does not bound what the node holds", nodeID, backend)
			}
		}
		return fmt.Sprintf("node %q", nodeID)
	}
	return ""
}

// llmDeclaredTools is the node's own `tools:` list, or nil for a kind that has
// none.
func llmDeclaredTools(node ir.Node) []string {
	switch n := node.(type) {
	case *ir.AgentNode:
		return n.Tools
	case *ir.JudgeNode:
		return n.Tools
	}
	return nil
}

// validateFanOutEachWorkspaceSafety applies the same shared-worktree mutation
// guard to data-driven fan-out. A fan_out_each has only one static template
// edge, so the static fan_out_all check cannot see that the template may be
// executed N times concurrently at runtime. Once cardinality and the effective
// parallelism cap are known, reject concurrent replays of a mutating template.
func (e *Engine) validateFanOutEachWorkspaceSafety(routerNodeID string, tmplEdge *ir.Edge, convergence string, itemCount, maxParallel int) error {
	if itemCount <= 1 || maxParallel <= 1 || tmplEdge == nil {
		return nil
	}
	// Fan_out_each template: a single node replayed over items, so a
	// `parallel_safe:` tool along the template is exempt (item-keyed disjoint
	// writes). Any other mutating node still trips the guard.
	if !e.branchContainsMutation(tmplEdge.To, convergence, true) {
		return nil
	}
	return &RuntimeError{
		Code:    ErrCodeWorkspaceSafety,
		Message: fmt.Sprintf("workspace safety violation: fan_out_each router %q would run mutating template branch %q concurrently for %d items", routerNodeID, tmplEdge.To, itemCount),
		Hint:    "mutating fan_out_each templates must run with max_parallel_branches=1 or be moved to sequential steps/read-only nodes",
	}
}
