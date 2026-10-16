package runtime

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
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
// The second vocabulary is advisory only where the ROUTE drops the list. codex
// does not drop it — it picks a sandbox mode from it — and reads none of those
// five names, nor `web_fetch`, as read-only: declaring them there buys the
// workspace-write mode. This map answers what a NAME means to the engine,
// never what a route makes of it; declarationIsNoBound asks the route.
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

// effectiveToolSurfaceResolver is the package-internal spelling of the seam.
type effectiveToolSurfaceResolver = EffectiveToolSurfaceResolver

// EffectiveToolSurfaceResolver reports which names a node's `tools:`
// DECLARATION cannot bound: the declared list after the runtime's own
// build-time appends, unioned over every route the node may take.
//
// It is NOT the node's whole tool surface, and an implementor must not report
// one — ambient `mcp:` servers and the ask_user/board/runs wiring are spliced
// in after this answer, and reporting them refuses branches for tools the
// author never declared. A nil or empty answer means "nothing to add", and
// every caller reads it as "the declaration is all there is": an implementor
// that UNDER-reports silently widens parallel-branch admission, which is the
// reading #1652 removed. `mayEscalateToUltracode` is the caller saying some
// edge into this node may carry `_reasoning_effort: "ultracode"`; the answer
// must then be taken over both values.
//
// An executor that does not implement the interface is not read as answering
// nil. Admission takes the worst case instead: every agent and judge it would
// run that is not `readonly:` counts as possibly writing to the run's shared
// workspace, exactly as a node declaring a write tool does, and its fan-outs
// are refused with WORKSPACE_SAFETY wherever such a node's would be — where
// nothing but the executor's silence made a branch count, the refusal says so
// and names the executor's type.
//
// Admission reads a second seam beside this one, EffectiveBackendResolver, and
// an executor has to answer both: one that answers only this one is still read
// at the worst case, for the route.
//
// A node's `tools:` list is not that list: the runtime folds its own opt-ins
// over it at build time, from state that is not in the IR (a launch-time
// --auto-memory, a per-node model override raising the node to ultracode). So
// the parallel-branch guard asks its executor instead of re-deriving the append
// rules — the same seam, and the same reason, as the backend resolver beside it.
//
// Admission runs when a fan-out router DISPATCHES, once per invocation, not
// before the run: the router's own output is already resolved by then, but the
// per-branch mappings are not read here, so a value an edge would inject is
// taken pessimistically rather than resolved. That is a deliberate
// simplification of a guard that must answer for a whole branch, not a claim
// that the value is unknowable.
//
// It is exported so an implementor in another package can pin itself against
// THE interface rather than against a structural copy of it. A copy catches a
// rename and misses the failure that actually produces a silent revert: the
// seam itself gaining a term. Measured — adding one method to the interface
// left `go build ./...` and `go vet ./pkg/dryrun/` green while dryrun's
// executor had stopped satisfying it.
type EffectiveToolSurfaceResolver interface {
	EffectiveToolNames(node ir.Node, mayEscalateToUltracode bool) []string
}

// toolSurfaceResolver is the ONE place the engine asks what a node will really
// hold. The production executor answers from the run — it alone sees the
// launch-time overrides — and it is reached through an OPTIONAL type assertion.
//
// An executor that does not implement the seam gets the WORST-CASE reading, not
// the declaration: the zero value of a guarantee is the refusal, never the
// passthrough. Falling back to the node's declared `tools:` list is precisely
// the reading #1652 removed, and a wrapper that forgets to forward the method
// would reopen it with every gate green.
//
// The seam is read by parallel-branch admission alone, so that is all the
// worst case costs: a node the executor cannot vouch for counts as possibly
// writing, exactly as a node declaring a write tool does, and gets the same
// typed WORKSPACE_SAFETY refusal wherever that node would — its reason naming
// the executor wherever nothing else made the branch count. Sequential nodes, agents and judges marked `readonly:`, tool
// and subbot nodes, and every executor that answers are untouched. Both
// executors this module builds answer it; an embedder's own — pkg/benchmark
// exports `ExecutorFactory` — or a wrapper around one of ours pays one method
// per seam.
//
// Reading the program alone (`model.NewProgramExecutor`) is no substitute, in
// either direction: it stops before the host, so where neither the node nor
// the workflow names a backend it adds none of the claw-gated appends that
// widen the workspace surface (auto_memory's file trio, ultracode's `agent`)
// though the run may land on claw; and it cannot see launch-time overrides,
// so it refuses what `--auto-memory off` admits.
func (e *Engine) toolSurfaceResolver() effectiveToolSurfaceResolver {
	if e == nil {
		return nil
	}
	if r, ok := e.executor.(effectiveToolSurfaceResolver); ok {
		return r
	}
	return unansweredSurface{executorType: fmt.Sprintf("%T", e.executor)}
}

// unansweredSurface stands in for an executor that cannot say what its nodes
// will hold. Its answer is one name no reader can call workspace-safe, so the
// stand-in refuses on its own: whoever asks it reads "this node may write",
// never the "nothing to add" a nil answer means. The refusal's reason
// recognises it by type, to name the executor and the method it lacks.
type unansweredSurface struct{ executorType string }

func (u unansweredSurface) EffectiveToolNames(ir.Node, bool) []string {
	return []string{"<unknown: " + u.executorType + " does not report the tools it grants>"}
}

// unansweredCause is the refusal's reason for a branch that counts as writing
// only because its executor cannot answer one or both of the seams admission
// reads, naming the first node it counts.
func unansweredCause(nodeID, executorType string, noTools, noBackend bool) string {
	missing, unread := "runtime.EffectiveToolSurfaceResolver", "the tools it will hold cannot be read"
	switch {
	case noTools && noBackend:
		missing, unread = "runtime.EffectiveToolSurfaceResolver or runtime.EffectiveBackendResolver", "neither the route it will take nor the tools it will hold can be read"
	case noBackend:
		missing, unread = "runtime.EffectiveBackendResolver", "the route it will take cannot be read"
	}
	return fmt.Sprintf("node %q counts as writing because executor %s does not implement %s, so %s — implement or forward the three seams admission reads (EffectiveToolNames, EffectiveBackendName, EffectiveFallbackBackends) on the executor, or mark the node `readonly:`", nodeID, executorType, missing, unread)
}

// warnExecutorLacksSeams says WHICH executor cannot answer, and which seams it
// lacks, once per engine, at its first parallel-branch admission — where the
// worst case is taken, and not at the sandbox's setup, which asks the backend
// seam on every run — so it is logged by the time any refusal it causes is
// returned: a run whose fan-outs are refused should not have to be read
// backwards from the refusal to learn why.
// It needs an engine given a logger (`WithLogger`; `runtime.New` sets no
// default) — the refusal's own reason names the executor too, so a logger-less
// engine still says it where it matters. Named by concrete type, because the
// point is to identify the executor that does not answer.
func (e *Engine) warnExecutorLacksSeams() {
	if e.logger == nil {
		return
	}
	e.seamWarnOnce.Do(func() {
		var missing []string
		if _, ok := e.executor.(effectiveToolSurfaceResolver); !ok {
			missing = append(missing, "runtime.EffectiveToolSurfaceResolver")
		}
		if _, ok := e.executor.(effectiveBackendResolver); !ok {
			missing = append(missing, "runtime.EffectiveBackendResolver")
		}
		// Said separately, because its silence costs something else: the two
		// seams above are read at the WORST case, this one at the reading
		// admission had before it existed. A route the run resolves onto a
		// backend the declaration bounds is refused, the routes it names
		// outright are read exactly as they are written.
		if _, ok := e.executor.(effectiveFallbackResolver); !ok {
			e.logger.Warn("runtime: executor %T does not implement runtime.EffectiveFallbackResolver — parallel-branch admission cannot resolve a node's `fallbacks:` routes with the run's vars, so a route spelled `{{vars.…}}` is read as a backend nothing resolved and the node counts as possibly writing, and a route the run's `when:` gate deactivates is counted anyway. Implement or forward EffectiveFallbackBackends on the executor; the two executors this module builds implement it, and a wrapper that embeds the NodeExecutor interface inherits none of the three seams", e.executor)
		}
		if len(missing) == 0 {
			return
		}
		e.logger.Warn("runtime: executor %T does not implement %s — parallel-branch admission cannot read what the nodes it runs will do, so it takes the worst case: every agent and judge not marked `readonly:` counts as possibly writing to the shared workspace, and its fan-outs are refused wherever a node declaring a write tool would be. Implement or forward EffectiveToolNames and EffectiveBackendName on the executor (a nil answer from EffectiveToolNames means \"the declaration is all there is\", an empty one from EffectiveBackendName \"the IR's backend is all there is\"), or mark the branch nodes `readonly:`. The two executors this module builds implement both; a wrapper that embeds the NodeExecutor interface inherits neither", e.executor, strings.Join(missing, " or "))
	})
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

// effectiveBackendResolver is the package-internal spelling of the seam.
type effectiveBackendResolver = EffectiveBackendResolver

// EffectiveBackendResolver reports the backend a node will run on, resolved
// the way DISPATCH resolves it: launch-time `--backend` overrides,
// the node's and the workflow's backend, then the host's default and its
// credential probe. Pre-run analyses that key on a node's backend ask it rather
// than re-deriving that chain from the IR, which would miss the overrides.
//
// An empty answer means "no route beyond the IR's own", and the caller then
// reads the node's backend from the IR: it is what a program-only resolver
// answers where the program names no backend. An implementor that routes a node
// elsewhere and answers "" silently widens parallel-branch admission.
//
// An executor that does not implement the interface is not read as answering
// "". Admission takes the worst case instead — a route no `tools:` list bounds
// — so every agent and judge it would run that is not `readonly:` counts as
// possibly writing, exactly as under EffectiveToolSurfaceResolver, and the
// refusal names the executor. Exported for the same reason as that seam: an
// implementor in another package pins itself against THE interface.
type EffectiveBackendResolver interface {
	EffectiveBackendName(ir.Node) string
}

// backendResolver is the ONE place the engine asks its executor for the
// dispatch-time backend resolution. Workspace-safety admission and the
// sandbox's claw bind-mount read it here rather than re-deriving from the IR,
// which would silently miss the launch-time `--backend` overrides.
// An executor that does not resolve backends gets a stand-in, never nil.
func (e *Engine) backendResolver() effectiveBackendResolver {
	if e == nil {
		return nil
	}
	if r, ok := e.executor.(effectiveBackendResolver); ok {
		return r
	}
	return unansweredBackend{executorType: fmt.Sprintf("%T", e.executor)}
}

// unansweredBackend stands in for an executor that cannot say where its nodes
// will run. Its answer is one name no reader can take for a backend that a
// `tools:` list bounds — it names no backend that receives a list at all — so
// admission reads every agent and judge as unbounded without a special case.
// The sandbox mounts the claw runner for a node the resolver routes to claw;
// this answer never names claw, so the sandbox reads the IR alone, as it does
// for a driver-level call. The refusal's reason recognises it by type.
type unansweredBackend struct{ executorType string }

func (u unansweredBackend) EffectiveBackendName(ir.Node) string {
	return "<unknown: " + u.executorType + " does not report the backend it routes to>"
}

// effectiveFallbackResolver is the package-internal spelling of the seam.
type effectiveFallbackResolver = EffectiveFallbackResolver

// EffectiveFallbackResolver reports the backends a node's `fallbacks:` routes
// resolve to, the way DISPATCH resolves them: the run's vars read into each
// route's `backend:`, the `when:` gates evaluated against those same vars, and
// a launch-time route materialised onto the node read like an authored one.
// The node's own route is NOT in the answer — that is EffectiveBackendResolver
// — and neither is a route that names no backend of its own, which inherits
// the node's and adds no route to read.
//
// A pre-run analysis that keys on a node's routes asks it rather than
// re-deriving them from the IR, which is wrong in both directions: a route
// spelled `{{vars.fb}}` is a backend NAME at dispatch and nothing in the IR,
// and a route the run's vars gate off is in the IR and never in the chain.
// Dropping a gated route is sound because the gate reads `vars.*` alone (C173)
// and a run's vars are launch-frozen — resolveVars stacks the declared
// defaults under the launch values once, at run start and again on resume, and
// nothing writes them afterwards — while admission is re-decided every time
// the router dispatches. So the gate the chain evaluates at dispatch is the
// one admission read. A gate the executor cannot evaluate is dropped for the
// same reason and not for its value: the executor deactivates that route in
// the chain dispatch walks, so a route admission does not count is one no run
// takes — and the compiler refuses a gate that does not parse at all (C173).
//
// It is a SEPARATE interface, not a term added to EffectiveBackendResolver:
// the engine reaches these seams through optional type assertions, so a term
// added to one would silently stop its implementors from satisfying it and
// take the whole node to the worst case instead of this one reading.
//
// An executor that does not implement it is not read as answering nil, which
// would mean "no route leaves the node's own backend". Admission then reads
// every route from the IR, by the spelling its author wrote, and one nothing
// resolves bounds nothing — the pessimistic side, which refuses rather than
// admits, and the one a guard takes where it cannot know. The engine says
// which executor cannot answer, once per engine, at its first admission
// (warnExecutorLacksSeams).
//
// Exported for the same reason as the two seams above: an implementor in
// another package pins itself against THE interface rather than a structural
// copy of it.
type EffectiveFallbackResolver interface {
	EffectiveFallbackBackends(node ir.Node) []string
}

// undecidedRouteReporter is the package-internal spelling of the seam.
type undecidedRouteReporter = UndecidedRouteReporter

// UndecidedRouteReporter is answered by an executor whose answer to the seams
// above is the PESSIMISTIC reading of where a node runs rather than the route
// the run takes, because the LAUNCH it answers for does not decide it: the dry
// run behind `iterion validate --exec` judges a program whose vars a launch
// still has to supply, and seeds a value of its own for each of them.
//
// What it returns is said to the operator, after the reason that names the
// route (routeReadingClause): what was read pessimistically, and what decides
// it. A refusal naming only the route would send an author to change a program
// that is not wrong — the reading is the launch's, not theirs. "" where the
// launch decides every route of the node; the executors a RUN uses implement
// none of this, because a run's vars are that run's.
//
// Optional, like the three seams above: an executor that does not implement it
// has its answers read exactly the same way and says nothing about why.
type UndecidedRouteReporter interface {
	UndecidedRouteReading(node ir.Node) string
}

// fallbackRouteNames asks the fallback seam for the backends a node's
// `fallbacks:` routes resolve to, and reports whether anything answered.
//
// The seam is read off the backend resolver's own VALUE, because at every site
// that asks "what will this run do" that value IS the engine's executor
// (backendResolver). The readings the engine substitutes for it — a nil
// resolver for the authored-defaults question, the worst-case stand-in, the
// per-route probes of the attribution walk — answer no fallback routes either,
// and each of them wants the IR reading it already had.
//
// A name no route takes is dropped: an empty one inherits the node's backend,
// and an explicit `auto` on a route is a NAME the registry does not have, so
// the route dies the moment the chain is needed (ir.backendReader.routeName)
// and widens nothing.
func fallbackRouteNames(node ir.Node, resolver effectiveBackendResolver) ([]string, bool) {
	fallbacks, ok := resolver.(effectiveFallbackResolver)
	if !ok {
		return nil, false
	}
	var out []string
	for _, b := range fallbacks.EffectiveFallbackBackends(node) {
		b = strings.TrimSpace(b)
		if routeIsUnnamed(b) {
			continue
		}
		out = append(out, b)
	}
	return out, true
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
		//
		// The engine honours the same assertion where it chooses the child's
		// workspace (runsInParentWorkspace): an isolated child keeps the
		// worktree its `worktree: auto` gives it, which is what lets the
		// branches admitted here overlap — a tree lent to a child takes one
		// writing run at a time.
		return !n.Isolated
	case *ir.AgentNode:
		if n.Readonly {
			return false
		}
		return llmToolSurfaceCanWrite(node, n.LLMFields, n.Tools, defaultBackend, resolver, surfaces, escalates, unnamedRouteUnread, nil)
	case *ir.JudgeNode:
		if n.Readonly {
			return false
		}
		return llmToolSurfaceCanWrite(node, n.LLMFields, n.Tools, defaultBackend, resolver, surfaces, escalates, unnamedRouteUnread, nil)
	}
	return false
}

// IsReadOnlyTool reports whether a declared tool name belongs to the
// read-only vocabulary above — the names an agent/judge may hold without
// being able to change the workspace.
func IsReadOnlyTool(name string) bool {
	return readOnlyTools[name]
}

// ToolSurfaceCanWrite reports whether an agent/judge node's DECLARATION, or
// any route it may take, lets it act on the workspace. It is
// ToolSurfaceWriteReason with the cause dropped: ONE walk answers both, so a
// caller's message can never name an arm the verdict did not use — and it
// carries the same blind spot, named there.
func ToolSurfaceCanWrite(node ir.Node, defaultBackend string, lookup func(string) string) bool {
	return ToolSurfaceWriteReason(node, defaultBackend, lookup) != ""
}

// ToolSurfaceWriteReason names what makes an agent/judge node's DECLARATION, or
// a ROUTE it may take, a surface that can act on the workspace: `full_access:`,
// a declared tool outside the read-only vocabulary, a route no `tools:` list
// bounds, or an omitted list on a backend where omission means the full native
// toolset (a CLI delegate, or claw when a `fallbacks:` route reaches one).
// Every other node kind answers "".
//
// It answers "" where neither the declaration nor any route it may take lets
// the node write — which is NOT the same as "the node cannot write". The
// runtime's own appends widen a claw node's surface past what it declared:
// `auto_memory: on` grants read_file/write_file/glob and ultracode grants the
// unbounded `agent` subagent tool, both over a RESTRICTED `tools:` list
// (pkg/backend/model.assembleEffectiveTools). On codex they add no writer and
// do something else: the list picks a sandbox MODE, so `interaction:` putting
// `ask_user` on a non-empty list, or `capabilities:` putting the board/runs
// MCP tools there, buys the workspace-write mode where codex's built-in shell
// acts on the worktree. They resolve outside the IR, so
// only the question that holds an executor reads them, through the
// effective-surface seam llmToolSurfaceCanWrite asks; a caller of this
// predicate that must know what a node HOLDS reads that seam, and a claw node
// restricting `tools:` with `auto_memory: on` — or a codex node restricting
// them with `interaction:` — joins a boundary class by hand.
//
// A DECLARED list is read as no bound wherever the route cannot enforce it: pi,
// kimi, grok and opencode never receive the list, and claude_code turns it into
// `--disallowedTools` over a closed native roster iterion does not own. A
// `fallbacks:` route onto one of those puts the node in whatever its primary
// backend bounds — a pre-run reading is pessimistic over the whole chain. A
// route no name resolves is read the same way — the compiled-in detection
// order's first choice is claude_code (pkg/backend/detect) — and that is the
// ONE cell where this question and shared-worktree admission differ, because
// being wrong costs them different things (see unnamedRouteReading).
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
func ToolSurfaceWriteReason(node ir.Node, defaultBackend string, lookup func(string) string) string {
	switch n := node.(type) {
	case *ir.AgentNode:
		return llmDeclaredSurfaceWriteReason(node, n.LLMFields, n.Tools, defaultBackend, nil, unnamedRouteIsNoBound, lookup)
	case *ir.JudgeNode:
		return llmDeclaredSurfaceWriteReason(node, n.LLMFields, n.Tools, defaultBackend, nil, unnamedRouteIsNoBound, lookup)
	}
	return ""
}

// unnamedRouteReading is what a classification makes of a route no name
// resolves — `backend: ""` or `backend: auto`, which dispatch resolves from the
// host's own preference order.
//
// The two questions this file answers about a node read the same facts and
// differ on that one cell, because being wrong there costs them different
// things. For the catalog's prompt-injection contract the cost is one paragraph
// in a system prompt, against a detection order whose first choice
// (pkg/backend/detect.DefaultPreferenceOrder) is claude_code, a route no
// `tools:` list bounds — so the route is read as unbounded. For shared-worktree
// admission the cost is a fan-out refused that would have been safe, so an
// unnamed route is left unread and the node is classified on what it declares.
//
// It is therefore a parameter of the QUESTION, passed by each entry point, and
// never derived from what the engine's executor happens to implement: keyed on
// the resolver, `iterion validate --exec --strict` — whose dry-run executor
// answers the seam from the PROGRAM alone — answered OK on a workflow the same
// tree kills at the router.
type unnamedRouteReading bool

const (
	// unnamedRouteIsNoBound reads a route no name resolves as one that bounds
	// no declaration.
	unnamedRouteIsNoBound unnamedRouteReading = true
	// unnamedRouteUnread leaves it unread: nothing here resolves it, and the
	// classification stands on what the node declares.
	unnamedRouteUnread unnamedRouteReading = false
)

// routeIsUnnamed reports whether a backend field names no route — the two
// spellings dispatch resolves from the host: absent, and the explicit `auto`.
func routeIsUnnamed(backend string) bool {
	b := strings.TrimSpace(backend)
	return b == "" || b == "auto"
}

// llmToolSurfaceCanWrite is the readonly-free agent/judge classification for
// shared-worktree admission: the declaration and the route first — read by
// llmDeclaredSurfaceWriteReason, shared with ToolSurfaceWriteReason so the two
// questions cannot drift on what "can write" means — then the EFFECTIVE surface
// the executor reports, an arm only this question has an answer for.
func llmToolSurfaceCanWrite(
	node ir.Node,
	fields ir.LLMFields,
	tools []string,
	defaultBackend string,
	resolver effectiveBackendResolver,
	surfaces effectiveToolSurfaceResolver,
	escalatesToUltracode bool,
	unnamedRoute unnamedRouteReading,
	lookup func(string) string,
) bool {
	if llmDeclaredSurfaceWriteReason(node, fields, tools, defaultBackend, resolver, unnamedRoute, lookup) != "" {
		return true
	}
	// The declared list is not the list the node holds. `assembleEffectiveTools`
	// folds the runtime's own opt-ins over it, and two of them widen the
	// workspace surface: `auto_memory: on` grants write_file on claw, and
	// ultracode grants claw's unbounded `agent` subagent tool — unbounded
	// wherever a host wires a subagent runner into the registry
	// (`ClawDefaults.Subagent`, as the e2e coverage test does); iterion's own
	// runs wire none, so there every `agent` call is typed-refused and this
	// arm reads the worst case over hosts rather than this run. A node that
	// declared `tools: [read_file]` and is admitted read-only on that basis
	// then shares one worktree with N siblings while holding a writer.
	//
	// Asked, never re-derived: both opt-ins resolve outside the IR.
	if surfaces == nil {
		return false
	}
	effective := surfaces.EffectiveToolNames(node, escalatesToUltracode)
	for _, t := range effective {
		if !toolIsWorkspaceSafe(t) {
			return true
		}
	}
	// That loop reads the names with the ENGINE's vocabulary — a question
	// channel and the board MCP tools touch no worktree — which is the right
	// reading on claw and not the one codex takes: there the list picks a
	// SANDBOX MODE, and one name its classifier does not know buys
	// workspace-write, where the built-in shell acts on the worktree. The
	// appends reach that reading: `interaction:` puts `ask_user` on a
	// non-empty list, `capabilities:` the board/runs MCP tools, an image
	// attachment `read_image`.
	//
	// So the route question is asked a second time over the list the route
	// RECEIVES — the same walk and the same arms as the declared reading
	// above, which is how a bound read from NAMES cannot be read from a list
	// the node will not hold. It only ever adds: every arm that answers for
	// the effective list answers for the declared one on a route whose bound
	// does not depend on the names.
	//
	// The seam's answer is a UNION over every route the node may take, while
	// this question is per route: a codex node with a `fallbacks:` route onto
	// claw is asked about claw's appends too. That is sound only while every
	// name the runtime appends on ONE route and treats as workspace-safe is
	// also one codex sandboxes read-only. Measured, the names a route appends
	// and the codex route does not receive are `todo_write`, and `read_file`
	// and `glob` under `auto_memory:` — all three reads to codex.
	//
	// Pinned where the condition lives, which is the FIELDS an append is
	// gated on, not a set of names: each field of the node is set in turn and
	// the producer asked what every route then holds, so a claw-only append
	// of a name codex sandboxes workspace-write fails instead of refusing a
	// fan-out whose codex route never receives it —
	// TestNoRouteAppendsAWorkspaceSafeNameTheCodexRouteDoesNotReceive.
	// TestANameNoCodexRouteReceivesMustBeReadOnlyToCodex carries the per-name
	// reading of nonWorkspaceTools — the names that reach this question at all.
	return unrestrictedRouteWriteReason(node, fields, effective, defaultBackend, resolver, lookup, unnamedRoute) != ""
}

// The engine's LEAF executors answer both seams admission reads. Asserted at
// compile time so dropping or renaming either method breaks the build, not the
// fan-outs: an executor that stops answering is read at the worst case (see
// toolSurfaceResolver and backendResolver), where every agent and judge it runs
// that is not `readonly:` counts as writing.
// The pin does not reach a wrapper around this type, which is a different type
// entirely; a wrapper that does not forward the method gets the same worst
// case, and the refusal names it.
// dryrun's executor asserts the same thing in its own package — pkg/runtime
// cannot import it, since dryrun imports pkg/runtime.
var (
	_ effectiveToolSurfaceResolver = (*model.ClawExecutor)(nil)
	_ effectiveBackendResolver     = (*model.ClawExecutor)(nil)
	_ effectiveFallbackResolver    = (*model.ClawExecutor)(nil)
)

// llmDeclaredSurfaceWriteReason names what makes a node's own DECLARATION, or
// the ROUTE it may take, a surface that can act on the workspace — and "" when
// neither does. It is the half both questions share, so neither the verdict nor
// the cause can drift between them.
func llmDeclaredSurfaceWriteReason(
	node ir.Node,
	fields ir.LLMFields,
	tools []string,
	defaultBackend string,
	resolver effectiveBackendResolver,
	unnamedRoute unnamedRouteReading,
	lookup func(string) string,
) string {
	if fields.FullAccess {
		return "full_access"
	}
	// The author's own line first where there is one: a declared writer is what
	// the node holds on every route, and naming a route rule instead sends the
	// reader after a bound the verdict never needed.
	var acting []string
	for _, t := range tools {
		if !readOnlyTools[t] {
			acting = append(acting, t)
		}
	}
	if len(acting) > 0 {
		return "tools: " + strings.Join(acting, ", ")
	}
	return unrestrictedRouteWriteReason(node, fields, tools, defaultBackend, resolver, lookup, unnamedRoute)
}

// unrestrictedRouteWriteReason names what the node's ROUTE leaves unbounded —
// the arms that hold whatever the declared list says, because the route cannot
// enforce it or resolves too late to be read.
func unrestrictedRouteWriteReason(
	node ir.Node,
	fields ir.LLMFields,
	tools []string,
	defaultBackend string,
	resolver effectiveBackendResolver,
	lookup func(string) string,
	unnamedRoute unnamedRouteReading,
) string {
	// `authored` is the spelling that produced `backend`, carried alongside it
	// for the message: a route read at an authored default that does not exist
	// resolves to nothing, and quoting `backend ""` at an author who wrote
	// `${ITERION_BACKEND}` sends them looking for a field they did not leave
	// blank.
	authored := strings.TrimSpace(fields.Backend)
	backend := strings.TrimSpace(ir.ExpandWithDefault(fields.Backend, lookup))
	// A `backend:` that NAMES a reference and resolves to nothing here has not
	// left the route to the workflow default: at dispatch that same spelling is
	// read against the host's environment FIRST and any name it answers wins,
	// `default_backend:` answering only on a host that names none
	// (model.ClawExecutor.resolveBackendName, steps 1-2). So the route is the
	// host's to decide — the cell a `fallbacks:` route of this spelling already
	// reaches, and the reading the compiler's own mirror of that precedence
	// takes (ir.backendReader.effective substitutes the workflow default for an
	// ABSENT or `auto` field, never for a reference it cannot answer). Reading
	// it as the workflow default would trust exactly the route this walk cannot
	// read: a `${VAR}` under `default_backend: claw` would be credited with
	// claw's bound while the host may name claude_code.
	unresolvedReference := backend == "" && authored != ""
	if backend == "" {
		if authored == "" {
			authored = strings.TrimSpace(defaultBackend)
		}
		backend = strings.TrimSpace(ir.ExpandWithDefault(defaultBackend, lookup))
	}
	if resolver != nil {
		if effective := strings.TrimSpace(resolver.EffectiveBackendName(node)); effective != "" {
			backend, authored, unresolvedReference = effective, effective, false
		}
	}
	// Answered before any arm below reads `backend`, which this spelling did
	// not name. What a pre-run reading of a route nothing here resolves is
	// worth is the caller's question (see unnamedRouteReading), and it is the
	// one the `fallbacks:` walk asks for the same spelling.
	if unresolvedReference && unnamedRoute == unnamedRouteIsNoBound {
		return unnamedRouteNoBoundReason(authored, "")
	}
	// A `{{vars.…}}` backend the resolver did not read (a nil resolver, a
	// stub) may be claw with a CLI route or a CLI backend outright: both
	// readings are decided once, before the run, so it is pessimistic —
	// writing — rather than read-only and eligible for a shared worktree.
	if strings.Contains(backend, "{{") {
		// The node's OWN field, which the fallback seam does not read: the
		// clause's reporter half is about this field, its unanswered half is
		// about routes this arm never reaches.
		return "backend " + backend + " is unresolved before the run, so no route it may take bounds what the node holds" + undecidedRouteClause(node, resolver)
	}
	// A DECLARED-EMPTY `tools: []` reaches exactly the verdict an undeclared
	// list reaches, and that is deliberate: it is not a proof that the node
	// holds nothing. On claude_code the bound is `--disallowedTools` over
	// `claudeNativeTools`, a hardcoded 14-name enumeration of a roster
	// iterion does not own — the package's own `orchestrationTools` names
	// `Agent`, `TaskOutput` and `Monitor` outside it, and MCP tools are not
	// on it either, so which of them survive is the CLI's call (`Monitor` and
	// every MCP tool do). On claw the runtime's own
	// `interaction:` append puts `ask_user` back, and the appends below it
	// `todo_write` and, under `auto_memory:`, `write_file` (C270 says so at
	// compile time). The declared-name loop the caller runs before this one
	// cannot see any of it: it iterates the declared names, and there are
	// none.
	//
	// A claw→CLI route un-restricts the node's tool set WHATEVER it
	// declared: a claw list is the node's bound only on claw, which resolves
	// its ToolDefs from it, and a route that leaves claw is read as taking
	// that bound with it. So this is checked BEFORE the list-based early
	// return — a `tools: [read_file]` claw node with a CLI route still gains
	// Edit/Write on fall-through, and the reading happens once, before the
	// run. What the route itself does with the list words the reason
	// (clawCLIRouteWriteReason); the route question below answers for every
	// claw node this arm does not.
	if backend == "claw" {
		if route := fallbackCLIBackendName(node, resolver, lookup); route != "" {
			return clawCLIRouteWriteReason(route, tools) + routeReadingClause(node, resolver)
		}
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
	// Whether an UNNAMED backend joins them is the caller's question to answer
	// (see unnamedRouteReading): nothing here resolves `backend: ""`, so what a
	// pre-run reading of it is worth depends on what being wrong costs.
	//
	// A DECLARED-EMPTY list is covered as well as a named one: reading the
	// stricter declaration as safer than the looser one would invert the guard.
	//
	// ONE site asks the route question, for every node that reaches it. A claw
	// node asks it whatever it declared — the spellings the arm above cannot
	// resolve name no backend to report there, and leaving them to a
	// list-shaped condition would answer "" for a claw node that declares NO
	// list while the same node declaring one joins the class, a declaration
	// coming out LOOSER than its absence. Asking twice is how the two arms
	// drift apart: the first answers for every node the second would.
	if backend == "claw" || len(tools) > 0 || toolcatalog.ToolsDeclared(tools) {
		if reason := routeDeclarationIsNoBound(node, backend, authored, tools, resolver, lookup, unnamedRoute); reason != "" {
			return reason
		}
	}
	if len(tools) > 0 {
		return ""
	}
	// An omitted list means the full native toolset on a CLI delegate and zero
	// tools on claw, so an unnamed route decides nothing by itself and the
	// caller's reading of it answers here too — the same cell as above, so a
	// declaration cannot come out STRICTER than the absence of one.
	if routeIsUnnamed(backend) {
		if unnamedRoute == unnamedRouteIsNoBound {
			return unnamedRouteNoBoundReason(authored, backend)
		}
		return ""
	}
	if backend != "claw" {
		// `tools: []` — the author declared a list, and "no `tools:` list
		// declared" denies their own line. The verdict is the one an
		// undeclared list reaches, deliberately (the walk above says why), so
		// only the wording parts: it names which of the two the node wrote.
		//
		// What the verdict RESTS ON differs by route, and the message says
		// which. On a route that reads omission as the full native toolset
		// the two lists run the same tools. On codex they do not: the
		// delegate sandboxes a DECLARED-empty list read-only
		// (delegate.CodexDeclaredListIsReadOnly), so naming the native
		// toolset there would state the opposite of the seam this walk reads
		// — what the reading refuses to credit is that sandbox, decided
		// before the runtime's own appends are.
		if toolcatalog.ToolsDeclared(tools) {
			if backend == delegate.BackendCodex {
				return "`tools: []` on codex names no tool, which is no proof the node holds none: it reaches the verdict an undeclared list reaches, deliberately"
			}
			return "`tools: []` on " + backend + " names no tool, which is no proof the node holds none: it reaches the verdict an undeclared list reaches, the full native toolset"
		}
		return "no `tools:` list declared on " + backend + ", the full native toolset"
	}
	// The node resolves to claw, where an omitted list means ZERO tools. The
	// one route that would un-restrict it — a `fallbacks:` entry (ADR-087) on a
	// CLI backend, which runs that same empty list as the full native toolset
	// under bypassPermissions — is answered at the top of this walk, for every
	// claw node and not only the tools-less ones.
	return ""
}

// routeDeclarationIsNoBound names the node's primary backend, or the first of
// its `fallbacks:` routes, that runs somewhere a `tools:` declaration does not
// bound what the node holds — and "" when every route it may take bounds one.
//
// The `fallbacks:` routes come from the executor where one answers the
// fallback seam: it resolves them with the run's vars and drops the ones that
// run's `when:` gates deactivate, which is the chain dispatch will walk. Where
// nothing answers, a route is read by the spelling its AUTHOR wrote, not only
// by what that spelling expanded to: the three shapes below answer
// differently, and collapsing them onto the expansion alone trusts the one
// route this walk cannot read.
//
// The declared list travels WITH the node onto every route it may take, so it
// is read against each of them: one route may bound those names and the next
// one not (declarationIsNoBound).
func routeDeclarationIsNoBound(node ir.Node, backend, authored string, tools []string, resolver effectiveBackendResolver, lookup func(string) string, unnamedRoute unnamedRouteReading) string {
	if unnamedRoute == unnamedRouteIsNoBound && routeIsUnnamed(backend) {
		return unnamedRouteNoBoundReason(authored, backend)
	}
	if clause := declarationNoBoundClause(backend, tools); clause != "" {
		return strings.TrimSpace(backend) + " " + clause
	}
	llm, ok := node.(ir.LLMNode)
	if !ok {
		return ""
	}
	if routes, answered := fallbackRouteNames(node, resolver); answered {
		for _, b := range routes {
			// A span the run's vars do not answer stays as written
			// (`{{outputs.…}}` names a namespace that does not exist before the
			// node runs), and it is the same cell as the unread template below:
			// the route may be any backend, so the reading is pessimistic —
			// without naming the template as a backend and crediting it with
			// what it does to the list, which nothing has decided yet.
			if strings.Contains(b, "{{") {
				return unresolvedFallbackRouteReason(node, b, resolver)
			}
			if clause := declarationNoBoundClause(b, tools); clause != "" {
				return "a `fallbacks:` route onto " + b + " " + clause + routeReadingClause(node, resolver)
			}
		}
		return ""
	}
	for _, fb := range llm.GetFallbacks() {
		raw := strings.TrimSpace(fb.Backend)
		b := strings.TrimSpace(ir.ExpandWithDefault(fb.Backend, lookup))
		switch {
		case raw == "":
			// A `fallbacks:` entry that names no backend INHERITS the node's,
			// which the reading above already answered — the unnamed-route
			// question is about the route the node takes, and this is not
			// another one.
			continue
		case strings.Contains(raw, "{{"):
			// Nothing here resolves a template, so the route may be any
			// backend and the reading is pessimistic — WITHOUT naming the
			// template as a backend and crediting it with what it does to the
			// list, which no executor has decided yet.
			return unresolvedFallbackRouteReason(node, raw, resolver)
		case b == "":
			// A `${VAR}` reference read at an authored default that does not
			// exist: the route is the host's to decide, which is the same cell
			// as the node's own unnamed route, so the caller's reading answers
			// here too. Reading it as "inherits the node's backend" would trust
			// exactly the route this walk cannot read.
			if unnamedRoute == unnamedRouteIsNoBound {
				return "a `fallbacks:` route onto " + raw + " " + unresolvedReferenceClause
			}
			continue
		case routeIsUnnamed(b):
			// The one spelling left is an explicit `auto`, which on a ROUTE is
			// a NAME and not "the chain decides": the registry has no backend
			// called `auto`, so the route dies the moment the chain is needed
			// (ir.backendReader.routeName) and widens nothing.
			continue
		}
		if clause := declarationNoBoundClause(b, tools); clause != "" {
			return "a `fallbacks:` route onto " + b + " " + clause
		}
	}
	return ""
}

// unresolvedFallbackRouteReason words a `fallbacks:` route still spelled as a
// template when the verdict is taken: either nothing resolved it — no executor
// answered the fallback seam — or the executor's own reading left it as
// written, which is what a span over a namespace that does not exist before
// the node runs resolves to, and what a pre-run reading owes a span the LAUNCH
// decides. All of those are the same fact for this question: no backend the
// route may take bounds what the node holds. One wording, so the readings
// cannot drift apart on the cell they share — and the one that is the
// executor's to answer says so, because the author's template is not the thing
// to change there.
func unresolvedFallbackRouteReason(node ir.Node, route string, resolver effectiveBackendResolver) string {
	return "a `fallbacks:` route still spelled as " + strings.TrimSpace(route) +
		" is unresolved before the run, so no backend it may take bounds what the node holds" +
		routeReadingClause(node, resolver)
}

// routeReadingClause says, after a reason that names where a node runs, why
// that is not the route the run takes — and nothing where it is.
//
// Two things leave a pre-run reading short of the run, and each is the
// executor's to report: nothing resolved the node's `fallbacks:` routes,
// because the executor does not answer the fallback seam at all
// (unreadRouteClause); or the routing fields were read pessimistically, because
// the LAUNCH the verdict was taken on does not decide them
// (undecidedRouteClause). The two cannot hold at once, so one clause carries
// both and no reason can run the two together.
//
// A reason whose subject is NOT a `fallbacks:` route calls the second half
// alone: the fallback seam decides nothing there, so naming it would name the
// cause the verdict did not use — and its remedy, followed to the letter,
// leaves the refusal word for word the same.
func routeReadingClause(node ir.Node, resolver effectiveBackendResolver) string {
	if clause := unreadRouteClause(resolver); clause != "" {
		return clause
	}
	return undecidedRouteClause(node, resolver)
}

// unreadRouteClause names the executor that left a node's `fallbacks:` routes
// unread, and the method to implement — and nothing where one answered.
//
// Only something that is the executor admission runs on is named: this
// package's own reading stand-ins answer no route either, and printing a Go
// type the operator does not have would name the wrong thing — the cause the
// verdict did not use. They answer one seam, never the node work an executor
// does, so the executor interface is the discriminator and a stand-in added
// later is silent here by default.
//
// The warning says this half too (warnExecutorLacksSeams) and needs a logger,
// which `runtime.New` does not install — so the refusal carries it, as it does
// for the two older seams (unansweredCause).
func unreadRouteClause(resolver effectiveBackendResolver) string {
	if _, answers := resolver.(effectiveFallbackResolver); answers {
		return ""
	}
	executor, ok := resolver.(NodeExecutor)
	if !ok {
		return ""
	}
	return fmt.Sprintf(" (executor %T does not implement runtime.EffectiveFallbackResolver, so nothing resolved it — implement or forward EffectiveFallbackBackends on the executor)", executor)
}

// undecidedRouteClause says why the executor's answer about where this node
// runs is the PESSIMISTIC reading rather than the route the run takes, and what
// ends that reading — which is what the dry run behind `iterion validate
// --exec` answers for a program whose vars a launch still has to supply
// (UndecidedRouteReporter). Nothing from an executor that does not report it,
// and nothing where the launch decides every routing field of the node.
func undecidedRouteClause(node ir.Node, resolver effectiveBackendResolver) string {
	reporter, ok := resolver.(undecidedRouteReporter)
	if !ok {
		return ""
	}
	if why := strings.TrimSpace(reporter.UndecidedRouteReading(node)); why != "" {
		return " (" + why + ")"
	}
	return ""
}

// declarationIsNoBound answers for ONE named backend and the list a node
// declares, and names no tool.
//
// Two routes NEVER bound a declaration, for two different reasons, and a third
// bounds it only by the names in it. pi, kimi, grok and opencode never receive
// the list at all (C270's subject), so the agent keeps its own toolset
// whatever the author wrote. claude_code does
// receive it and still is not bounded by it: the list becomes
// `--disallowedTools` over a CLOSED native roster this project does not own, so
// it removes the names iterion happens to enumerate and nothing else — measured
// on CLI 2.1.220, a node declaring `tools: [read_file]` still registers
// `EnterWorktree` and `ExitWorktree`, which move the worktree the session acts
// in.
//
// codex receives the list and bounds the node by what the NAMES buy: the list
// picks a sandbox mode and narrows nothing else, since codex runs its built-in
// shell outside the SDK's per-name callbacks (codex.go's own note at the
// WithSandbox call; toolcatalog.ConstrainsTools stays claw-only). So the
// delegate's own classifier answers — `read_file`, `glob` and `grep` buy the
// read-only mode, while `web_fetch` and the five unregistered advisory names
// of the vocabulary above buy workspace-write, where that shell can act on the
// worktree. A name-blind reading of codex left a node declaring the advisory
// idiom out of the prompt-injection class and admitted it to a shared worktree
// (#1727).
//
// Both questions this file answers read it, so neither can drift from the other
// on a NAMED route. They part on an unnamed one, which this predicate leaves to
// its callers: see unnamedRouteReading.
func declarationIsNoBound(backend string, tools []string) bool {
	b := strings.TrimSpace(backend)
	if routeIsUnnamed(b) {
		return false
	}
	if !toolcatalog.ReceivesToolList(b) || b == delegate.BackendClaudeCode {
		return true
	}
	if b == delegate.BackendCodex {
		return !delegate.CodexDeclaredListIsReadOnly(tools, toolcatalog.ToolsDeclared(tools))
	}
	return false
}

// declarationNoBoundClause words declarationIsNoBound for the reader, and is
// empty wherever that predicate is false — the verdict stays single-sourced, so
// a message can never claim a route the classification bounds. The clause reads
// after the backend's own name, so one wording serves the node's route and a
// `fallbacks:` one.
func declarationNoBoundClause(backend string, tools []string) string {
	b := strings.TrimSpace(backend)
	if !declarationIsNoBound(b, tools) {
		return ""
	}
	if !toolcatalog.ReceivesToolList(b) {
		return "never receives the `tools:` list, so a declared bound is dropped (#1652)"
	}
	if b == delegate.BackendCodex {
		return "maps the `tools:` list onto a sandbox mode, and what this node declares buys the workspace-write one, where codex's built-in shell acts on the worktree (#1727)"
	}
	return "takes the `tools:` list as --disallowedTools over a closed native roster iterion does not own, so a declared list bounds nothing (#1652)"
}

// unresolvedReferenceClause words a route spelled as a `${VAR}` reference and
// read at an authored default that does not exist. It reads after the route's
// own spelling, so one wording serves the node's route and a `fallbacks:` one.
//
// It is NOT the unnamed-route clause below: nothing was left to the detection
// order here, a reference was written and this reading does not resolve it —
// the run may put any backend there, including one a declared list bounds.
const unresolvedReferenceClause = "is read at its authored default, which names no route, so nothing here bounds what the node holds"

// unnamedRouteNoBoundReason words the unnamedRouteIsNoBound reading of a route
// dispatch resolves, naming the spelling the AUTHOR wrote rather than what it
// expanded to.
//
// Two shapes reach it and they are not the same fact. A route left to dispatch
// (`backend:` absent, or `auto`) is resolved from the host's preference order,
// whose COMPILED-IN first choice (detect.DefaultPreferenceOrder) is
// claude_code — a host that sets ITERION_BACKEND_PREFERENCE, or that has no
// claude_code CLI at all, resolves it elsewhere, so the compiled-in order is
// the most a pre-run reading may state. A route written as a `${VAR}`
// reference that resolves to nothing named no route at all.
func unnamedRouteNoBoundReason(authored, backend string) string {
	if spelling := strings.TrimSpace(authored); spelling != "" && strings.TrimSpace(backend) == "" {
		return "backend " + spelling + " " + unresolvedReferenceClause
	}
	return fmt.Sprintf("backend %q resolves at dispatch, where the compiled-in detection order's first choice is claude_code, which does not bound what the node holds (#1652)", strings.TrimSpace(backend))
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

// clawCLIRouteWriteReason words a claw node's `fallbacks:` route onto another
// backend, where the list the node declared stops being its bound.
//
// Where the route's own clause answers, the route runs the full native toolset
// whatever the node declared: claude_code ignores the lowercase list under the
// always-on bypassPermissions, and the CLI-agent seam backends never receive
// it. codex is the one readable route that is NOT that — it maps the list onto
// a sandbox mode — so where those names buy its read-only mode the reason says
// what this reading actually refuses to do: credit the route's own sandbox to
// a node whose primary is claw. The verdict is the claw→CLI one either way,
// which is what admission has always answered for that chain
// (TestTheOwnAccountIsReadOnEveryRouteADeclarationBounds pins it) — and on a
// non-empty list the `interaction:` append puts `ask_user` there, which buys
// codex workspace-write anyway (model.buildTask).
func clawCLIRouteWriteReason(route string, tools []string) string {
	if declarationIsNoBound(route, tools) {
		return "a `fallbacks:` route onto " + route + " runs the full native toolset, which nothing the node declares bounds"
	}
	return "a `fallbacks:` route onto " + route + " leaves claw, where the `tools:` list is the node's bound, and a pre-run reading does not credit the route's own sandbox mode as one"
}

// fallbackCLIBackendName names the first of a node's `fallbacks:` routes that
// takes it off claw — and "" when no route it may take does.
//
// It answers for the spellings it can READ, and names the one it read: a
// message that called an unresolved route "a CLI backend" would credit a
// toolset to a route nothing has resolved yet. What the named route makes of
// the node's list is clawCLIRouteWriteReason's question, not this one's: an
// empty list means the full native toolset on claude_code and on the
// CLI-agent seam, while codex reads it as a sandbox mode.
//
// Four spellings are not that. A route left unnamed inherits the node's own
// backend, claw here. An explicit `auto` on a route is a NAME the registry
// does not have — resolveChain normalises `auto` only on a route's `provider:`
// and delegate.DefaultRegistry has no `auto` entry — so the route dies the
// moment the chain is needed (ir.backendReader.routeName) and widens nothing.
// A `{{…}}` template, and a reference read at an authored default that does
// not exist, name no backend at all: routeDeclarationIsNoBound words those
// two, pessimistically and without naming a backend.
//
// `auto` apart, any OTHER name no registry has is named here, and credited
// with a toolset the route will never run: it dies at the same registry, one
// spelling later.
//
// Where an executor answers the fallback seam the routes are ITS resolution:
// the run's vars read into each `backend:`, and the routes that run's `when:`
// gates deactivate already dropped. A template the run itself does not resolve
// is still not a backend, and is still left to routeDeclarationIsNoBound.
func fallbackCLIBackendName(node ir.Node, resolver effectiveBackendResolver, lookup func(string) string) string {
	llm, ok := node.(ir.LLMNode)
	if !ok {
		return ""
	}
	if routes, answered := fallbackRouteNames(node, resolver); answered {
		for _, b := range routes {
			if strings.Contains(b, "{{") || b == delegate.BackendClaw {
				continue
			}
			return b
		}
		return ""
	}
	for _, fb := range llm.GetFallbacks() {
		raw := strings.TrimSpace(fb.Backend)
		b := strings.TrimSpace(ir.ExpandWithDefault(fb.Backend, lookup))
		switch {
		case raw == "", strings.Contains(raw, "{{"), b == "", routeIsUnnamed(b):
			continue
		case b != "claw":
			return b
		}
	}
	return ""
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
	e.warnExecutorLacksSeams()
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
				mutatingCount, mutatingBranches, e.mutationCauses(mutatingBranches, globalConvergence, false)),
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
// Recomputed on the error path alone, in the context the verdict was reached
// in: a fan_out_each template exempts its `parallel_safe:` tools.
func (e *Engine) mutationCauses(branches []string, convergence string, fanOutEachTemplate bool) string {
	var parts []string
	for _, b := range branches {
		if cause := e.branchMutationCause(b, convergence, fanOutEachTemplate); cause != "" {
			parts = append(parts, cause)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " — " + strings.Join(parts, "; ")
}

func (e *Engine) branchMutationCause(startNodeID, globalConvergence string, fanOutEachTemplate bool) string {
	backends, surfaces := e.backendResolver(), e.toolSurfaceResolver()
	_, noBackend := backends.(unansweredBackend)
	_, noTools := surfaces.(unansweredSurface)
	if noBackend || noTools {
		// Blame the executor only when its silence is what made the BRANCH
		// count. A branch that writes on its own account anywhere along it —
		// a declared writer, `full_access:`, an authored CLI fallback, a tool
		// or subbot node, or what an answered seam says — is reported for
		// that, read at the most permissive answer each silent seam could
		// give, since implementing them would not change its verdict.
		ownBackends, ownSurfaces := backends, surfaces
		if noBackend {
			ownBackends = mostPermissiveRoute{}
		}
		if noTools {
			ownSurfaces = nil
		}
		if own := e.branchMutationCauseFrom(ownBackends, ownSurfaces, startNodeID, globalConvergence, fanOutEachTemplate); own != "" {
			return own
		}
	}
	return e.branchMutationCauseFrom(backends, surfaces, startNodeID, globalConvergence, fanOutEachTemplate)
}

func (e *Engine) branchMutationCauseFrom(backends effectiveBackendResolver, surfaces effectiveToolSurfaceResolver, startNodeID, globalConvergence string, fanOutEachTemplate bool) string {
	visited := map[string]bool{}
	queue := []string{startNodeID}
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
		if !e.countsInPass(node, backends, surfaces, fanOutEachTemplate) {
			for _, edge := range e.workflow.Edges {
				if edge.From == nodeID {
					queue = append(queue, edge.To)
				}
			}
			continue
		}
		ub, noBackend := backends.(unansweredBackend)
		us, noTools := surfaces.(unansweredSurface)
		if noBackend || noTools {
			// Reached only once the branch writes on no account of its own:
			// the executor's silence is the whole reason this node counts.
			typ := us.executorType
			if noBackend {
				typ = ub.executorType
			}
			return unansweredCause(nodeID, typ, noTools, noBackend)
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
				// The subject is a TOOL, and the surface it sits on is the
				// EXECUTOR's own answer, taken over the routes that executor
				// resolved for itself (EffectiveToolNames →
				// EffectiveToolNamesOnRoutes): the engine's fallback seam
				// decides nothing about it, so only the reporter half speaks
				// here — the unread half would name a cause the verdict did
				// not use, and its remedy, followed to the letter, leaves this
				// refusal word for word the same.
				//
				// The reporter half does belong: an executor answering the
				// surface from its OWN pessimistic reading of the routes — the
				// dry run behind `iterion validate --exec` — puts a route's
				// appends on this surface, and this arm runs FIRST, so a
				// refusal silent here is silent for the whole cell.
				return fmt.Sprintf("node %q holds %q, which its `tools:` list does not declare%s", nodeID, t, undecidedRouteClause(node, backends))
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
			if backends != nil {
				if eff := strings.TrimSpace(backends.EffectiveBackendName(node)); eff != "" {
					backend = eff
				}
			}
			// A route still spelled as a template was read pessimistically
			// because nothing resolved it; naming it as a backend would be
			// the wrong reason, so it is left to the bare node below.
			// Read with the list the classifier read: on codex the names
			// decide whether the route bounds them at all, and the list that
			// decided there is the one the route RECEIVES — the verdict was
			// taken over it (llmToolSurfaceCanWrite), so a cause read over
			// the AUTHORED list alone would name no route for a node whose
			// bound moved with the runtime's own appends. Where no executor
			// answers, the node's own list is all there is.
			list := llmDeclaredTools(node)
			if surfaces != nil {
				if held := surfaces.EffectiveToolNames(node, edgeMayEscalateEffort(e.workflow, node)); len(held) > 0 {
					list = held
				}
			}
			if !strings.Contains(backend, "{{") && declarationIsNoBound(backend, list) { // the cause of a shared-worktree refusal
				return fmt.Sprintf("node %q runs on %s, where a `tools:` list does not bound what the node holds", nodeID, backend)
			}
			// Read exactly as the classifier read them: the executor's
			// resolution where it answers the fallback seam — so the cause
			// names the route the run takes and not the spelling it was
			// written as — and the IR's own expansion where nothing does.
			routes, answered := fallbackRouteNames(node, backends)
			if !answered {
				for _, fb := range llm.GetFallbacks() {
					routes = append(routes, strings.TrimSpace(ir.ExpandWithDefault(fb.Backend, nil)))
				}
			}
			for _, b := range routes {
				if !strings.Contains(b, "{{") && declarationIsNoBound(b, list) {
					return fmt.Sprintf("node %q falls back to %s, where a `tools:` list does not bound what the node holds%s", nodeID, b, routeReadingClause(node, backends))
				}
			}
			// The arms above name a ROUTE, which is the first thing an author
			// can act on. Every other reason this node counts is worded by the
			// very walk the verdict was taken over — asked here with the same
			// arguments countsInPass gives it — so a cause is never absent
			// where a refusal is, and never names a rule the verdict did not
			// use: a declared writer, `full_access:`, a `tools: []` the route
			// does not credit, an unresolved route.
			if reason := llmDeclaredSurfaceWriteReason(node, *llm.GetLLMFields(), llmDeclaredTools(node), e.workflow.DefaultBackend, backends, unnamedRouteUnread, nil); reason != "" {
				return fmt.Sprintf("node %q: %s", nodeID, reason)
			}
			// The walk above reads the node's own list, which is the first
			// half of the verdict. The second reads the list the route
			// RECEIVES, and the two do not cover the same cells: the route
			// question is gated on the list being non-empty, so a node that
			// declares NO list skips it entirely on the declared reading
			// while one runtime append makes the same walk answer. So it is
			// asked here too, over the list the verdict took it over.
			//
			// Asked directly, never through llmDeclaredSurfaceWriteReason:
			// that walk's first arm reads the names LITERALLY as the author's
			// own line, and an appended `read_image` would be reported as
			// `tools: read_image` — a line the author never wrote.
			if reason := unrestrictedRouteWriteReason(node, *llm.GetLLMFields(), list, e.workflow.DefaultBackend, backends, nil, unnamedRouteUnread); reason != "" {
				return fmt.Sprintf("node %q: %s", nodeID, reason)
			}
		}
		return fmt.Sprintf("node %q", nodeID)
	}
	return ""
}

// mostPermissiveRoute stands in for the backend seam in the own-account pass
// of an executor that cannot answer it. That seam REPLACES a node's IR backend
// — a launch override may route a `backend: claude_code` node onto claw, and a
// `{{vars.…}}` one is read by nothing else — so while the executor is silent,
// no route the IR names is the branch's own account. A node counts there only
// if it writes on EVERY route a declaration bounds — claw and codex, see
// countsInPass — so what stays the branch's own is what writes on any route: a
// declared writer, `full_access:`, a fallback no declaration bounds, a tool or
// subbot node. Its own answer, claw, only words the reason.
type mostPermissiveRoute struct{}

func (mostPermissiveRoute) EffectiveBackendName(ir.Node) string { return delegate.BackendClaw }

// declarationBoundRoutes are the backends on which a node's `tools:` list CAN
// be the bound on what it holds — on claw always, on codex for the names its
// sandbox reads as read-only (declarationIsNoBound answers per route and per
// list, which is what the probe below asks).
var declarationBoundRoutes = []string{delegate.BackendClaw, delegate.BackendCodex}

// answersRoute resolves every node to one backend.
type answersRoute string

func (r answersRoute) EffectiveBackendName(ir.Node) string { return string(r) }

// countsInPass is the attribution walk's classifier. In the own-account pass of
// a silent backend seam a node counts only if it writes on every route a
// declaration bounds: neither route is more permissive than the other for
// every node — claw un-restricts a list the moment a CLI fallback exists, codex
// holds its full toolset when no list is declared and its workspace-write
// sandbox when the names it declares buy one.
func (e *Engine) countsInPass(node ir.Node, backends effectiveBackendResolver, surfaces effectiveToolSurfaceResolver, fanOutEachTemplate bool) bool {
	if _, own := backends.(mostPermissiveRoute); own {
		for _, route := range declarationBoundRoutes {
			if !isMutatingNodeIn(e.workflow, node, e.workflow.DefaultBackend, answersRoute(route), surfaces, fanOutEachTemplate) {
				return false
			}
		}
		return true
	}
	return isMutatingNodeIn(e.workflow, node, e.workflow.DefaultBackend, backends, surfaces, fanOutEachTemplate)
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
		Message: fmt.Sprintf("workspace safety violation: fan_out_each router %q would run mutating template branch %q concurrently for %d items%s", routerNodeID, tmplEdge.To, itemCount, e.mutationCauses([]string{tmplEdge.To}, convergence, true)),
		Hint:    "mutating fan_out_each templates must run with max_parallel_branches=1 or be moved to sequential steps/read-only nodes",
	}
}
