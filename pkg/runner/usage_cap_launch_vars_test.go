package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/treeskip"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// varString declares a string var, with a default when def is non-empty — the
// shape review-pr's `mono_family` takes (`string [enum: …] = "claude"`).
func varString(name, def string) *ir.Var {
	v := &ir.Var{Name: name, Type: ir.VarString}
	if def != "" {
		v.HasDefault, v.Default = true, def
	}
	return v
}

// whenEdge is a `src -> dst when <expr>` edge, compiled the way the compiler
// compiles a `when` clause.
func whenEdge(from, to, src string) *ir.Edge {
	return &ir.Edge{From: from, To: to, Expression: expr.MustParse(src), ExpressionSrc: src}
}

// iterationBackEdge is a `src -> dst loop <name> when <expr>` back-edge.
func iterationBackEdge(from, to, loop, src string) *ir.Edge {
	e := whenEdge(from, to, src)
	e.LoopName = loop
	return e
}

// foreachBackEdge is a `src -> dst as foreach <name>(item in …) when <expr>`
// back-edge: the collection-bounded iteration beside the loop-bounded one.
func foreachBackEdge(from, to, foreach, src string) *ir.Edge {
	e := whenEdge(from, to, src)
	e.ForeachName = foreach
	return e
}

// topologyWorkflow is review-pr's mono topology reduced to what the park walk
// reads (bots/review-pr/main.bot): a condition router whose gpt branch is
// guarded by a launch var, and whose claude branch is the UNCONDITIONAL
// fallback taken when no guarded sibling matched.
func topologyWorkflow(vars map[string]*ir.Var, edges ...*ir.Edge) *ir.Workflow {
	wf := &ir.Workflow{
		Name: "review_pr", Entry: "topology", Vars: vars,
		Nodes: map[string]ir.Node{
			"topology":        &ir.RouterNode{BaseNode: ir.BaseNode{ID: "topology"}, RouterMode: ir.RouterCondition},
			"reviewer_claude": agentRoute("reviewer_claude", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"reviewer_gpt":    agentRoute("reviewer_gpt", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"done":            &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: append(edges,
			&ir.Edge{From: "reviewer_claude", To: "done"},
			&ir.Edge{From: "reviewer_gpt", To: "done"},
		),
	}
	return wf
}

// monoTopology is the mono half of that topology: the gpt branch behind
// `vars.mono_family == 'gpt'`, the claude branch as the bare fallback.
func monoTopology(vars map[string]*ir.Var) *ir.Workflow {
	return topologyWorkflow(vars,
		whenEdge("topology", "reviewer_gpt", "vars.mono_family == 'gpt'"),
		&ir.Edge{From: "topology", To: "reviewer_claude"},
	)
}

func monoFamilyVar(def string) map[string]*ir.Var {
	return map[string]*ir.Var{"mono_family": varString("mono_family", def)}
}

// softCappedForfait caps the five-hour window of the Claude forfait: the test
// policy reads that window SOFT, the mode whose park decision asks what the run
// may reach at all.
func softCappedForfait(t *testing.T) usagecap.Store {
	t.Helper()
	caps := usagecap.NewMemStore()
	if err := caps.Record(context.Background(), platformKey("fp-forfait"), usagecap.Reading{
		Window: usagecap.WindowFiveHour, Utilization: 0.95, Status: usagecap.StatusWarning,
		ResetsAt: time.Now().UTC().Add(2 * time.Hour), ObservedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return caps
}

func preflightWithVars(t *testing.T, caps usagecap.Store, ctx context.Context, wf *ir.Workflow, vars map[string]any) error {
	t.Helper()
	r := capRunner(capTestPolicy(), caps, &capStatusStore{})
	return r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-vars", Vars: vars}, iterlog.Nop())
}

// The run's own launch vars decide which branches it can take, so a capped
// route on a branch they exclude is not a route the run may reach. A soft cap
// is where this bites: it stops nothing in flight, so a route it covers parks
// the run as soon as the run may reach it — and before this reading, the
// unconditional claude fallback kept the claude reviewers reachable under a
// launch that had pinned the gpt family (#1828).
func TestUsageCapPreflight_LaunchVarsDecideWhichBranchesTheRunCanReach(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := softCappedForfait(t)
	ctx := forfaitBesidePinnedKeys()

	if err := preflightWithVars(t, caps, ctx, monoTopology(monoFamilyVar("claude")), map[string]any{"mono_family": "gpt"}); err != nil {
		t.Errorf("parked a mono-gpt run on the capped forfait: %v — its claude branch is behind a predicate the launch made false", err)
	}
	// The same topology under the family the cap covers: the claude reviewer IS
	// the route the run takes, and the cap holds.
	if err := preflightWithVars(t, caps, ctx, monoTopology(monoFamilyVar("claude")), map[string]any{"mono_family": "claude"}); err == nil {
		t.Error("started a mono-claude run on the soft-capped forfait — the route it selects spends it uninterrupted")
	}
	// No launch value: the declared default decides, the layer resolveVars
	// stacks under the overrides.
	if err := preflightWithVars(t, caps, ctx, monoTopology(monoFamilyVar("claude")), nil); err == nil {
		t.Error("started a run whose declared default selects the capped family")
	}
	// A var nothing declares a default for and no launch value sets is ABSENT
	// at the edge too: undecided, so the walk keeps both branches.
	if err := preflightWithVars(t, caps, ctx, monoTopology(monoFamilyVar("")), nil); err == nil {
		t.Error("started a run whose selecting var no launch value and no default answers")
	}
}

// What the launch vars cannot decide keeps today's conservative answer: a
// predicate reading a node's OUTPUT, and the `Condition` field form, are both
// settled at runtime by a value no pre-flight holds.
func TestUsageCapPreflight_AnUndecidablePredicateKeepsTheBranch(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := softCappedForfait(t)
	ctx := forfaitBesidePinnedKeys()
	vars := map[string]any{"mono_family": "gpt"}

	// outputs.<node> beside the var: the run decides it after `topology` ran.
	mixed := topologyWorkflow(monoFamilyVar("claude"),
		whenEdge("topology", "reviewer_gpt", "vars.mono_family == 'gpt'"),
		whenEdge("topology", "reviewer_claude", "vars.mono_family == 'gpt' && outputs.topology.ready"),
	)
	if err := preflightWithVars(t, caps, ctx, mixed, vars); err == nil {
		t.Error("started a run whose capped branch is guarded by a node output — nothing here decides that predicate")
	}
	// The `Condition` field form reads the source node's output schema.
	field := topologyWorkflow(monoFamilyVar("claude"),
		whenEdge("topology", "reviewer_gpt", "vars.mono_family == 'gpt'"),
		&ir.Edge{From: "topology", To: "reviewer_claude", Condition: "wants_claude"},
	)
	if err := preflightWithVars(t, caps, ctx, field, vars); err == nil {
		t.Error("started a run whose capped branch is guarded by an output FIELD — nothing here decides that condition")
	}
}

// A fan-out router takes EVERY outgoing edge, predicate or not
// (runtime.prepareFanOut), so a launch var cannot exclude one of its branches:
// review-pr's dual mode fans out to both families, and the claude reviewer runs.
func TestUsageCapPreflight_AFanOutBranchStaysReachableWhateverItsPredicate(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := softCappedForfait(t)
	ctx := forfaitBesidePinnedKeys()
	vars := map[string]any{"mono_family": "gpt", "review_mode": "dual"}

	dual := &ir.Workflow{
		Name: "review_pr", Entry: "topology",
		Vars: map[string]*ir.Var{
			"mono_family": varString("mono_family", "claude"),
			"review_mode": varString("review_mode", "mono"),
		},
		Nodes: map[string]ir.Node{
			"topology":        &ir.RouterNode{BaseNode: ir.BaseNode{ID: "topology"}, RouterMode: ir.RouterCondition},
			"fan":             &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fan"}, RouterMode: ir.RouterFanOutAll},
			"reviewer_claude": agentRoute("reviewer_claude", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"reviewer_gpt":    agentRoute("reviewer_gpt", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"merge":           &ir.ToolNode{BaseNode: ir.BaseNode{ID: "merge"}, Command: "true"},
			"done":            &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			whenEdge("topology", "fan", "vars.review_mode == 'dual'"),
			whenEdge("topology", "reviewer_gpt", "vars.mono_family == 'gpt'"),
			{From: "topology", To: "reviewer_claude"},
			// The fan's branches carry the family predicates too, and the
			// fan-out runs both regardless.
			whenEdge("fan", "reviewer_claude", "vars.mono_family == 'claude'"),
			whenEdge("fan", "reviewer_gpt", "vars.mono_family == 'gpt'"),
			{From: "reviewer_claude", To: "merge"},
			{From: "reviewer_gpt", To: "merge"},
			{From: "merge", To: "done"},
		},
	}
	if err := preflightWithVars(t, caps, ctx, dual, vars); err == nil {
		t.Error("started a dual run whose fan-out runs the capped claude reviewer — a fan-out takes every branch, whatever its `when` says")
	}
}

// A hard cap the launch's own vars make unavoidable parks the run in advance:
// with the gpt branch excluded, every execution crosses the capped claude
// route, and the pod the mid-run guard would have stopped buys nothing.
func TestUsageCapPreflight_AHardCapTheLaunchVarsMakeUnavoidableParks(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := usagecap.NewMemStore()
	resets := time.Now().UTC().Add(30 * time.Hour).Truncate(time.Second)
	weekCapped(t, caps, platformKey("fp-forfait"), resets)
	ctx := forfaitBesidePinnedKeys()

	// The claude branch is the fallback, and the gpt branch is excluded: no
	// path avoids the capped route.
	claudeOnly := monoTopology(monoFamilyVar("claude"))
	if at := parkedUntil(t, preflightWithVars(t, caps, ctx, claudeOnly, map[string]any{"mono_family": "claude"})); !at.Equal(resets) {
		t.Errorf("parked until %v, want the forfait's reopening %v", at, resets)
	}
	// The mirror: the launch pins gpt, so the hard-capped claude route is on no
	// path the run can take.
	if err := preflightWithVars(t, caps, ctx, monoTopology(monoFamilyVar("claude")), map[string]any{"mono_family": "gpt"}); err != nil {
		t.Errorf("parked a mono-gpt run on a hard-capped route no path of it crosses: %v", err)
	}
}

// An iteration is no exclusion: a loop back-edge whose `when` the launch vars
// make true is still declined at its cap, its fuel, a liveness stall or its
// budget (runtime.evaluateEdgesWithLoopsRS), and the run then takes the exit
// beside it. A capped route behind that exit is one the run reaches.
func TestUsageCapPreflight_ALoopIterationDoesNotExcludeItsExit(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := softCappedForfait(t)
	ctx := forfaitBesidePinnedKeys()

	// The judge iterates while its `when` holds, leaves for `done` on its own
	// verdict, and otherwise falls through to the capped wrap-up.
	loop := &ir.Workflow{
		Name: "review", Entry: "work", Vars: monoFamilyVar("claude"),
		Nodes: map[string]ir.Node{
			"work":   agentRoute("work", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"judge":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "judge"}, Command: "true"},
			"wrapup": agentRoute("wrapup", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "work", To: "judge"},
			iterationBackEdge("judge", "work", "review", "vars.mono_family == 'gpt'"),
			{From: "judge", To: "done", Condition: "converged"},
			{From: "judge", To: "wrapup"},
			{From: "wrapup", To: "done"},
		},
		Loops: map[string]*ir.Loop{"review": {Name: "review", MaxIterations: 3}},
	}
	if err := preflightWithVars(t, caps, ctx, loop, map[string]any{"mono_family": "gpt"}); err == nil {
		t.Error("started a run whose capped route sits on the loop's EXIT — the iteration is declined at its cap and the exit is taken")
	}
}

// The FOREACH half of that carve-out, on a graph where the per-graph refusal
// cannot answer for it: `escape`, behind a predicate the launch does not
// decide, keeps a path to a terminal whatever `judge` loses — so what the
// filter does with the foreach's exit is what this measures. A foreach
// back-edge whose `when` the launch made true is still skipped once the
// collection is spent (runtime.evaluateEdgesWithLoopsRS reads `idx+1 >= count`
// and falls through before the predicate can be the crossing), and the exit
// beside it is the crossing then.
func TestUsageCapPreflight_AForeachIterationDoesNotExcludeItsExit(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := softCappedForfait(t)
	ctx := forfaitBesidePinnedKeys()

	// `work` scans the items the judge hands back, and leaves for the capped
	// wrap-up once the collection is spent. `escape` is the branch the
	// router's own output decides, which the launch answers nothing about.
	scan := &ir.Workflow{
		Name: "scan", Entry: "pick", Vars: monoFamilyVar("claude"),
		Nodes: map[string]ir.Node{
			"pick":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "pick"}, RouterMode: ir.RouterCondition},
			"escape": agentRoute("escape", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"stale":  agentRoute("stale", delegate.BackendClaw, "openai", "openai/gpt-6-sol"),
			"work":   agentRoute("work", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"judge":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "judge"}, Command: "true"},
			"wrapup": agentRoute("wrapup", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			whenEdge("pick", "escape", "outputs.pick.hot"),
			whenEdge("pick", "work", "vars.mono_family == 'gpt'"),
			{From: "pick", To: "stale"},
			foreachBackEdge("judge", "work", "items", "vars.mono_family == 'gpt'"),
			{From: "judge", To: "wrapup"},
			{From: "work", To: "judge"},
			{From: "escape", To: "done"},
			{From: "stale", To: "done"},
			{From: "wrapup", To: "done"},
		},
		Foreaches: map[string]*ir.Foreach{"items": {Name: "items", CollectionRaw: "{{outputs.work.items}}"}},
	}
	gpt := map[string]any{"mono_family": "gpt"}
	open := scan.LaunchVarEdgeFilter(gpt)
	if open == nil {
		t.Fatal("premise: this launch prunes the topology (the fallback `stale`) and the escape keeps the graph-level refusal silent — without both, this case decides nothing")
	}
	if !reaches(scan, open, "wrapup") {
		t.Error("the capped wrap-up is off this run's topology: the foreach's exit was dropped as pre-empted, and the engine takes it once the collection is spent")
	}
	if err := preflightWithVars(t, caps, ctx, scan, gpt); err == nil {
		t.Error("started a run whose capped route sits on the foreach's EXIT — a soft cap stops nothing in flight, so that call is spent to the end")
	}
}

// A round-robin router cycles its GUARDLESS edges and reads no `when`
// (runtime.execRoundRobin): a guarded sibling the launch vars make true
// pre-empts none of them, so a capped route on one is a route the run takes.
func TestUsageCapPreflight_ARoundRobinTargetIsNotPreEmptedByAGuard(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := softCappedForfait(t)
	ctx := forfaitBesidePinnedKeys()

	cycle := &ir.Workflow{
		Name: "spread", Entry: "pick", Vars: monoFamilyVar("claude"),
		Nodes: map[string]ir.Node{
			"pick":            &ir.RouterNode{BaseNode: ir.BaseNode{ID: "pick"}, RouterMode: ir.RouterRoundRobin},
			"reviewer_gpt":    agentRoute("reviewer_gpt", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"reviewer_claude": agentRoute("reviewer_claude", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"fast":            agentRoute("fast", delegate.BackendClaw, "openai", "openai/gpt-6-sol"),
			"done":            &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			whenEdge("pick", "fast", "vars.mono_family == 'gpt'"),
			{From: "pick", To: "reviewer_gpt"},
			{From: "pick", To: "reviewer_claude"},
			{From: "reviewer_gpt", To: "done"},
			{From: "reviewer_claude", To: "done"},
			{From: "fast", To: "done"},
		},
	}
	if err := preflightWithVars(t, caps, ctx, cycle, map[string]any{"mono_family": "gpt"}); err == nil {
		t.Error("started a run whose round-robin cycles the capped claude reviewer — a guarded edge pre-empts nothing there")
	}
}

// inputStore is capStatusStore with a run document: what the pre-flight reads
// the launch inputs off when the message does not carry them. A store with no
// document for the run answers the way a wiped or unreachable record does.
type inputStore struct {
	capStatusStore
	run  *store.Run
	err  error
	load int
}

func (s *inputStore) LoadRun(_ context.Context, id string) (*store.Run, error) {
	s.load++
	if s.err != nil {
		return nil, s.err
	}
	if s.run == nil || s.run.ID != id {
		return nil, fmt.Errorf("no run %s", id)
	}
	return s.run, nil
}

// A REDELIVERED launch is the runner's own message with `Resume` stamped on it
// (resolveDeliveryPreconditions, queue.Admit's AsResume) — a launch message
// reaching a run that already persisted resumable state, which the usage-cap
// park's own status flip creates. It still carries this run's launch vars, and
// they still decide: keying the refusal on the shape rather than the vars
// re-opened #1828 on every redelivery.
func TestUsageCapPreflight_ARedeliveredLaunchPrunesOnTheVarsItCarries(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := softCappedForfait(t)
	r := capRunner(capTestPolicy(), caps, &capStatusStore{})
	vars := map[string]any{"mono_family": "gpt"}

	// The admission rule that produces the shape, on the status the park sets.
	launch := &queue.RunMessage{RunID: "run-vars", Vars: vars}
	parked := &store.Run{ID: "run-vars", Status: store.RunStatusFailedResumable, FailureCode: store.FailureUsageLimitBlocked}
	if adm := queue.Admit(launch, parked); !adm.AsResume {
		t.Fatalf("queue.Admit on a usage-cap park = %+v, want AsResume — the premise of this test", adm)
	}
	launch.Resume = &queue.ResumeSpec{}
	if err := r.usageCapPreflight(forfaitBesidePinnedKeys(), monoTopology(monoFamilyVar("claude")), launch, iterlog.Nop()); err != nil {
		t.Errorf("a redelivered mono-gpt launch parked: %v — the vars it still carries exclude the claude reviewers", err)
	}
}

// A resume PUBLICATION carries no vars (cloudpublisher's SubmitResume), and the
// attempt it continues resumes from the checkpoint the run's stored inputs
// built: those inputs are this run's launch vars, so the resumed walk reads
// the same topology the launch did. The usage-window retry comes back through
// exactly this path, and a resume that judged the whole graph would park again
// past the instant the park promised.
func TestUsageCapPreflight_AResumeFoldsTheRunsStoredInputs(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := softCappedForfait(t)
	ctx := forfaitBesidePinnedKeys()
	resume := func() *queue.RunMessage {
		return &queue.RunMessage{RunID: "run-vars", Resume: &queue.ResumeSpec{}}
	}
	run := func(inputs map[string]any) *store.Run {
		return &store.Run{ID: "run-vars", Status: store.RunStatusFailedResumable, Inputs: inputs}
	}
	for _, tc := range []struct {
		name   string
		rs     *inputStore
		parked bool
		loaded int
	}{
		{"the launch pinned gpt", &inputStore{run: run(map[string]any{"mono_family": "gpt"})}, false, 1},
		{"the launch pinned claude", &inputStore{run: run(map[string]any{"mono_family": "claude"})}, true, 1},
		{"the launch overrode nothing, so the declared default is its own", &inputStore{run: run(nil)}, true, 1},
		{"no document to read", &inputStore{}, true, 1},
		{"a store that cannot be read", &inputStore{err: errors.New("mongo down")}, true, 1},
		{"no store at all", nil, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rs store.RunStore
			if tc.rs != nil {
				rs = tc.rs
			}
			r := capRunner(capTestPolicy(), caps, rs)
			err := r.usageCapPreflight(ctx, monoTopology(monoFamilyVar("claude")), resume(), iterlog.Nop())
			if parked := err != nil; parked != tc.parked {
				t.Errorf("parked = %v (%v), want %v", parked, err, tc.parked)
			}
			if tc.rs != nil && tc.rs.load != tc.loaded {
				t.Errorf("LoadRun called %d time(s), want %d — the read is paid once, and only for a capped run", tc.rs.load, tc.loaded)
			}
			// A walk that fell back to the whole graph says why: the park it
			// may produce names a route the run's own vars could exclude.
			var buf bytes.Buffer
			r = capRunner(capTestPolicy(), caps, rs)
			r.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
			_ = r.usageCapPreflight(ctx, monoTopology(monoFamilyVar("claude")), resume(), r.cfg.Logger)
			said := strings.Contains(buf.String(), "launch inputs unreadable for the usage-cap walk")
			if want := tc.rs != nil && tc.rs.run == nil; said != want {
				t.Errorf("logged the fallback = %v, want %v; log: %s", said, want, buf.String())
			}
		})
	}
	// A fresh launch pays no read: only a resume publication reads the run
	// document, and it does so for the wire view itself — a run's routes are
	// judged on its own launch before the cap is known.
	rs := &inputStore{run: run(map[string]any{"mono_family": "gpt"})}
	r := capRunner(capTestPolicy(), usagecap.NewMemStore(), rs)
	if err := r.usageCapPreflight(ctx, monoTopology(monoFamilyVar("claude")), resume(), iterlog.Nop()); err != nil {
		t.Fatalf("parked with no cap recorded: %v", err)
	}
	if rs.load != 1 {
		t.Errorf("LoadRun called %d time(s) for an uncapped resume, want 1 — the wire view reads the stored inputs before the cap is known", rs.load)
	}
}

// twoWindowWorkflow is a graph whose two capped routes spend DIFFERENT
// credentials, reopening at different instants: `merge` on the forfait, on
// every path, and `reviewer_claude` — a claw route metered on the credential
// its `anthropic/…` model resolves — behind the fallback a mono-gpt launch
// pre-empts. The two walks therefore disagree about WHEN the run may start.
func twoWindowWorkflow(vars map[string]*ir.Var) *ir.Workflow {
	return &ir.Workflow{
		Name: "two_windows", Entry: "merge", Vars: vars,
		Nodes: map[string]ir.Node{
			"merge":           agentRoute("merge", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"topology":        &ir.RouterNode{BaseNode: ir.BaseNode{ID: "topology"}, RouterMode: ir.RouterCondition},
			"reviewer_gpt":    agentRoute("reviewer_gpt", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"reviewer_claude": agentRoute("reviewer_claude", delegate.BackendClaw, "", "anthropic/claude-opus-5-5"),
			"done":            &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "merge", To: "topology"},
			whenEdge("topology", "reviewer_gpt", "vars.mono_family == 'gpt'"),
			{From: "topology", To: "reviewer_claude"},
			{From: "reviewer_gpt", To: "done"},
			{From: "reviewer_claude", To: "done"},
		},
	}
}

// The retry is armed for the instant the PARK computed, and the woken resume
// must honour it: both walk the same topology, so the resume neither frees a
// run the launch parked nor parks it past what the launch promised. A resume
// judging the whole graph came back 28 hours later here, attempt after
// attempt, each one re-arming for the same distant instant.
func TestUsageCapPreflight_AResumeParksForTheSameInstantAsItsLaunch(t *testing.T) {
	blankAnthropicWireEnv(t)
	soon := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	late := time.Now().UTC().Add(30 * time.Hour).Truncate(time.Second)
	caps := usagecap.NewMemStore()
	for key, at := range map[string]time.Time{platformKey("fp-forfait"): soon, platformKey("fp-ant"): late} {
		if err := caps.Record(context.Background(), key, usagecap.Reading{
			Window: usagecap.WindowFiveHour, Utilization: 0.95, Status: usagecap.StatusWarning,
			ResetsAt: at, ObservedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := forfaitBesidePinnedKeys()
	// Precondition — the two walks must disagree at all: a run whose
	// selecting var nothing answers gets NO filter, and the whole-graph
	// answer is the 30h one. Two routes collapsed onto one credential would
	// make the rest of this test prove nothing.
	if at := parkedUntil(t, preflightWithVars(t, caps, ctx, twoWindowWorkflow(monoFamilyVar("")), nil)); !at.Equal(late) {
		t.Fatalf("the whole-graph walk parks until %v, want the far reopening %v — the two routes must spend different credentials", at, late)
	}
	wf := twoWindowWorkflow(monoFamilyVar("claude"))
	gpt := map[string]any{"mono_family": "gpt"}
	if !reaches(wf, nil, "reviewer_claude") || reaches(wf, wf.LaunchVarEdgeFilter(gpt), "reviewer_claude") {
		t.Fatal("precondition: the far route is on the graph and off this run's topology")
	}
	atLaunch := parkedUntil(t, preflightWithVars(t, caps, ctx, wf, gpt))
	if !atLaunch.Equal(soon) {
		t.Fatalf("the launch parked until %v, want the forfait's own reopening %v", atLaunch, soon)
	}
	rs := &inputStore{run: &store.Run{ID: "run-vars", Status: store.RunStatusFailedResumable, Inputs: gpt}}
	r := capRunner(capTestPolicy(), caps, rs)
	resumed := parkedUntil(t, r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-vars", Resume: &queue.ResumeSpec{}}, iterlog.Nop()))
	if !resumed.Equal(atLaunch) {
		t.Errorf("the resume parked until %v, want the instant the launch armed the retry for (%v) — %v further", resumed, atLaunch, resumed.Sub(atLaunch))
	}
}

// isAppendCall reports whether an expression is a call to the builtin
// `append`. The field such a call writes is a slice, which a run's
// `map[string]any` inputs cannot be.
func isAppendCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	return ok && fn.Name == "append"
}

// isRunInputs reports whether an expression names a run document's `Inputs`
// field — the selector `x.Inputs`, whatever x is.
func isRunInputs(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Inputs"
}

// buildsARunDocument reports whether a composite literal builds the run
// document whose inputs are a launch's: `Run{…}`, or `store.Run{…}` however
// the package is imported. A literal whose type is ELIDED — an element of a
// collection of them — is judged as one: no reading here can narrow it, so it
// falls on the side that judges.
func buildsARunDocument(typ ast.Expr) bool {
	switch t := typ.(type) {
	case nil:
		return true
	case *ast.Ident:
		return t.Name == "Run"
	case *ast.SelectorExpr:
		return t.Sel.Name == "Run"
	}
	return false
}

// typeName is how a composite literal's type is spelled, for the message and
// for the list of the other fields named `Inputs`.
func typeName(typ ast.Expr) string {
	switch t := typ.(type) {
	case nil:
		return "(an elided type)"
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok {
			return pkg.Name + "." + t.Sel.Name
		}
		return t.Sel.Name
	}
	return "(a type this reading cannot spell)"
}

// inPlaceMapWrite names the verb of a call that may rewrite its FIRST
// argument's map in place: the builtins `clear` and `delete`, and every
// function of the `maps` package. The ones that write take the destination
// first; one that only reads it answers here too, which is the side that
// shows the call rather than the side that hides it.
func inPlaceMapWrite(call *ast.CallExpr) (string, bool) {
	if len(call.Args) == 0 {
		return "", false
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if fun.Name == "clear" || fun.Name == "delete" {
			return fun.Name, true
		}
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "maps" {
			return "maps." + fun.Sel.Name, true
		}
	}
	return "", false
}

// Source witness for the other half of the premise: a run's launch inputs are
// written where the launch persists them and nowhere else, so the document the
// pre-flight reads on a resume still carries the attempt's own values. The
// launch surface and the engine's pickup are the two writers, and each writes
// the launch map ITSELF — a second builder, or a per-key edit, reddens here
// rather than in a pre-flight that silently disagrees with the run.
//
// The domain is the repository's whole non-test source, not the directories
// today's writers live in: the write that would break this premise is the one
// nobody thought of, and a per-directory scan hides it behind a path nobody
// listed. The trees it does NOT read are the repository's own set
// (internal/treeskip), so a tree added there is skipped here too.
//
// Five shapes write that map, and one rule judges them all — the value written
// is the launch map, or it is named here with what it is instead:
//
//   - `r.Inputs = …`, and `r.Inputs[k] = …` per key;
//   - a composite literal `Run{Inputs: …}`, which is how six of today's
//     writers write it — the store, its mongo twin and the cloud publisher.
//     Another type carrying a field of that name holds a DIFFERENT field, and
//     says below what it is: the type nobody listed reddens rather than
//     passing as a run's;
//   - an in-place rewrite — `clear`, `delete` or any `maps.…` on it;
//   - an ALIAS the same function writes through (`m := r.Inputs; m[k] = …`).
//     An alias handed to a READER is what five sites do today (ResolveVars, a
//     supervisor's specs, a title heuristic), and is not a rewrite.
//
// What keeps a scan this wide from reading a different field named `Inputs` as
// a run's is Go itself rather than a second file list: the type the literal
// names, and — for a field an `append(…)` writes — that such a field is a
// slice, which a run's `map[string]any` inputs cannot be.
//
// What it does not see, named rather than hidden: a function the map is handed
// to that writes through its own parameter, and an alias that escapes its
// function (returned, or stored in a struct) and is written elsewhere. Both
// write at a site that names no run, and both are beyond a reading of shapes —
// the sibling premise pin in pkg/dsl/ir has the same limit.
//
// Test files are not judged: a fixture that sets a run's inputs IS that
// test's own launch. The premise is about what rewrites them in a deployment,
// between a launch and the resume that reads them back.
func TestARunsLaunchInputsAreWrittenOnlyFromTheLaunchMap(t *testing.T) {
	// Each exception carries the right-hand side it may write: a changed
	// right-hand side, or a second write in the same file, is a new claim and
	// reddens here.
	exempt := map[string]string{
		// A redacted COPY for the read surface — a view of a run, never the
		// document the pre-flight and the engine read.
		"pkg/store/launch_secrets.go": "RedactLaunchVars(r.Inputs)",
		// The store conformance harness's own fixture: the document every
		// backend must round-trip.
		"pkg/store/storetest/conformance.go": `map[string]any{"config.path": "/etc/app.yml", "cmd": "$JAVA_HOME/bin/java"}`,
		// A contract's declared input PORTS (ast.Contract.Inputs), the other
		// field of that name in this tree.
		"pkg/dsl/parser/parser_contract.go": "p.parseContractPorts()",
		// The dry run's simulated CHILD launch: derived from the parent's
		// `with:` hand-off and what the parent was given, and carried on
		// dryrun.Options — persisted nowhere, and never the run document's
		// launch map (#1778).
		"pkg/dryrun/run.go": "subbotLaunch(wf, node, opts.Inputs)",
	}
	// The types whose `Inputs` is not a run's launch values, with what it is.
	// A composite literal of any OTHER type keyed `Inputs` is read as the run
	// document's: the field nobody classified reddens here rather than
	// escaping as somebody else's.
	otherInputsFields := map[string]string{
		"RunHeader":           "the read surface's header for a run, carrying a redacted copy (pkg/runview)",
		"cli.ValidateOptions": "the values a `validate` run is checked against, persisted nowhere",
		"dryrun.Options":      "the values a dry run is simulated with, persisted nowhere",
	}
	// The `append` carve-out holds only while a run's inputs are a map, which
	// the type checker answers for, here and at every change to that field.
	runInputsAreAStringKeyedMap := func(map[string]any) {}
	runInputsAreAStringKeyedMap((&store.Run{}).Inputs)
	seen := map[string]int{}
	seenType := map[string]int{}
	written, scanned := 0, 0
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	text := func(src []byte, e ast.Expr) string {
		from, to := fset.Position(e.Pos()).Offset, fset.Position(e.End()).Offset
		return strings.Join(strings.Fields(string(src[from:to])), " ")
	}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The trees this repository's sweeps never read: vendored and
			// installed third-party code, sibling worktrees, the engine's own
			// scratch, build output, and the deliberately broken fixtures
			// under testdata, which no toolchain compiles either.
			if treeskip.Dir(d.Name()) {
				return filepath.SkipDir
			}
			// A directory carrying its own .git is a NESTED CHECKOUT — a
			// worktree or a sibling clone an operator keeps on disk. Its
			// files belong to another tree, and an older copy of this one
			// would report writes that tree allowed.
			if path != root {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		// Every shape judged here names the field in the source, so a file
		// without that text holds none: the parse is paid only where it can
		// answer.
		if !bytes.Contains(src, []byte("Inputs")) {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, src, 0)
		if parseErr != nil {
			return parseErr
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		judge := func(line int, rhs ast.Expr) {
			switch rhsText := text(src, rhs); {
			case rhsText == "inputs":
				written++
			case isAppendCall(rhs):
			case exempt[rel] == rhsText:
				seen[rel]++
			default:
				t.Errorf("%s:%d writes a run's inputs from something other than the launch map: %s — write the launch map itself, or name this write in the exceptions with the reason it is not a launch", rel, line, rhsText)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					line := fset.Position(lhs.Pos()).Line
					if idx, isIndex := lhs.(*ast.IndexExpr); isIndex {
						if isRunInputs(idx.X) {
							t.Errorf("%s:%d writes a run's inputs PER KEY — what a run carries is the launch map itself", rel, line)
						}
						continue
					}
					if !isRunInputs(lhs) {
						continue
					}
					rhs := node.Rhs[0]
					if len(node.Rhs) == len(node.Lhs) {
						rhs = node.Rhs[i]
					}
					judge(line, rhs)
				}
			case *ast.CompositeLit:
				for _, el := range node.Elts {
					kv, isKeyed := el.(*ast.KeyValueExpr)
					if !isKeyed {
						continue
					}
					key, isIdent := kv.Key.(*ast.Ident)
					if !isIdent || key.Name != "Inputs" {
						continue
					}
					line := fset.Position(kv.Pos()).Line
					if !buildsARunDocument(node.Type) {
						typ := typeName(node.Type)
						if _, named := otherInputsFields[typ]; !named {
							t.Errorf("%s:%d builds a %s with an `Inputs` this pin cannot place: if those are a run's launch inputs, write the launch map itself; if it is another field of that name, name the type with what it is", rel, line, typ)
							continue
						}
						seenType[typ]++
						continue
					}
					judge(line, kv.Value)
				}
			case *ast.CallExpr:
				verb, inPlace := inPlaceMapWrite(node)
				if inPlace && isRunInputs(node.Args[0]) {
					t.Errorf("%s:%d rewrites a run's inputs in place (%s) — what a run carries is the launch map itself", rel, fset.Position(node.Pos()).Line, verb)
				}
			}
			return true
		})
		// An alias writes the same map without naming it, but only where the
		// function writes THROUGH it: the alias is collected from the
		// function's own body, and judged by what that body then does to it.
		for _, decl := range f.Decls {
			fd, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fd.Body == nil {
				continue
			}
			aliased := map[string]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, isAssign := n.(*ast.AssignStmt)
				if !isAssign || len(as.Lhs) != len(as.Rhs) {
					return true
				}
				for i, rhs := range as.Rhs {
					if !isRunInputs(rhs) {
						continue
					}
					if id, isIdent := as.Lhs[i].(*ast.Ident); isIdent && id.Name != "_" {
						aliased[id.Name] = true
					}
				}
				return true
			})
			if len(aliased) == 0 {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						idx, isIndex := lhs.(*ast.IndexExpr)
						if !isIndex {
							continue
						}
						if id, isIdent := idx.X.(*ast.Ident); isIdent && aliased[id.Name] {
							t.Errorf("%s:%d writes a run's inputs per key through the alias %q — what a run carries is the launch map itself", rel, fset.Position(lhs.Pos()).Line, id.Name)
						}
					}
				case *ast.CallExpr:
					verb, inPlace := inPlaceMapWrite(node)
					if !inPlace {
						return true
					}
					if id, isIdent := node.Args[0].(*ast.Ident); isIdent && aliased[id.Name] {
						t.Errorf("%s:%d rewrites a run's inputs in place (%s) through the alias %q", rel, fset.Position(node.Pos()).Line, verb, id.Name)
					}
				}
				return true
			})
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	// The walk read the tree, not an empty one: a premise nothing scanned
	// passes unchecked.
	if scanned < 500 {
		t.Fatalf("scanned %d non-test Go files — the walk no longer reads the tree this pin claims", scanned)
	}
	if written == 0 {
		t.Error("no write of the launch map itself found — the pin no longer reads what it claims")
	}
	for rel, rhs := range exempt {
		if seen[rel] != 1 {
			t.Errorf("%s: %d writes matched the exception %q, want exactly 1 — a stale exception covers the next writer", rel, seen[rel], rhs)
		}
	}
	// A list naming a field this tree no longer carries is a list nobody
	// maintains, and the next type of that name would join it unread.
	for typ, what := range otherInputsFields {
		if seenType[typ] == 0 {
			t.Errorf("no literal builds a %s keyed `Inputs` — the list still describes it as %q", typ, what)
		}
	}
}

// An unannotated cycle reaches neither a terminal nor a node's own end.
// The graph safeguard still refuses that prune: no path ending without a
// wall is proven, so the whole-graph answer stands. Declared bounded
// iterations are different (TestUsageCapPreflight_SpentIterationKeepsThePrune).
func TestUsageCapPreflight_AVacuousPruneDoesNotPark(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := usagecap.NewMemStore()
	resets := time.Now().UTC().Add(30 * time.Hour).Truncate(time.Second)
	weekCapped(t, caps, platformKey("fp-forfait"), resets)
	ctx := forfaitBesidePinnedKeys()

	// `warmup` puts a model call on every path, so the cap applies at all.
	// `route`'s three branches are all guarded by the same var: under
	// mode=loop only the one into `spin` survives, and `spin` comes back —
	// every node keeps a transition, and no terminal is reachable.
	wf := &ir.Workflow{
		Name: "vacuous", Entry: "warmup",
		Vars: map[string]*ir.Var{"mode": varString("mode", "exit")},
		Nodes: map[string]ir.Node{
			"warmup": agentRoute("warmup", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"route":  &ir.RouterNode{BaseNode: ir.BaseNode{ID: "route"}, RouterMode: ir.RouterCondition},
			"spin":   &ir.ToolNode{BaseNode: ir.BaseNode{ID: "spin"}, Command: "true"},
			"capped": agentRoute("capped", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "warmup", To: "route"},
			whenEdge("route", "spin", "vars.mode == 'loop'"),
			whenEdge("route", "done", "vars.mode == 'exit'"),
			whenEdge("route", "capped", "vars.mode == 'llm'"),
			{From: "spin", To: "route"},
			{From: "capped", To: "done"},
		},
	}
	if !wf.AlwaysReachesLLM() {
		t.Fatal("precondition: every path makes a model call, so the cap applies")
	}
	// The shape, read with the prune applied BY HAND: the two excluded
	// branches dropped, the entry reaches no terminal, and AlwaysReachesVia
	// answers "every path is walled" for a route CanReachVia cannot reach.
	isCapped := func(n ir.Node) bool { return n != nil && n.NodeID() == "capped" }
	hand := func(e *ir.Edge) bool {
		excluded := e.From == "route" && (e.To == "done" || e.To == "capped")
		return !excluded
	}
	if !wf.AlwaysReachesVia(hand, isCapped) || wf.CanReachVia(hand, isCapped) {
		t.Fatalf("precondition: the hand-pruned graph is the vacuous shape — always=%v, can=%v",
			wf.AlwaysReachesVia(hand, isCapped), wf.CanReachVia(hand, isCapped))
	}
	if wf.LaunchVarEdgeFilter(map[string]any{"mode": "loop"}) != nil {
		t.Error("the filter made that prune: a graph with no terminal left walls nothing, so there must be no filter at all")
	}
	if err := preflightWithVars(t, caps, ctx, wf, map[string]any{"mode": "loop"}); err != nil {
		t.Errorf("parked a run for a capped route it cannot reach: %v — the prune left no terminal, which walls nothing", err)
	}
	// Control: the same graph under a launch whose topology DOES cross the
	// capped route still parks.
	if err := preflightWithVars(t, caps, ctx, wf, map[string]any{"mode": "llm"}); err == nil {
		t.Error("started a run whose only branch is the hard-capped route")
	}
}

// W3 (#1828): a node whose launch excludes every exit is its own end,
// not a cap wall. The capped sibling keeps a terminal reachable, so the
// graph-level safeguard cannot hide a missing stopped annotation.
// Mutation: remove `out[id].stopped` from AlwaysReachesVia's free-end case.
func TestUsageCapPreflight_ANodeWithNoTransitionLeftDoesNotPark(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := usagecap.NewMemStore()
	resets := time.Now().UTC().Add(30 * time.Hour).Truncate(time.Second)
	weekCapped(t, caps, platformKey("fp-forfait"), resets)

	// `pick` offers the capped route behind its own output, the scan behind
	// the family var, and `stale` as the fallback the family guard pre-empts.
	// `pass` then routes on a family this launch is not: both its guards are
	// false, and the engine dies there — of its own missing edge.
	wf := &ir.Workflow{
		Name: "edgeless", Entry: "pick", Vars: monoFamilyVar("claude"),
		Nodes: map[string]ir.Node{
			"pick":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "pick"}, RouterMode: ir.RouterCondition},
			"pass":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "pass"}, RouterMode: ir.RouterCondition},
			"capped": agentRoute("capped", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"stale":  agentRoute("stale", delegate.BackendClaw, "openai", "openai/gpt-6-sol"),
			"scan":   agentRoute("scan", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"wrap":   agentRoute("wrap", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			whenEdge("pick", "capped", "outputs.pick.hot"),
			whenEdge("pick", "pass", "vars.mono_family == 'gpt'"),
			{From: "pick", To: "stale"},
			whenEdge("pass", "scan", "vars.mono_family == 'claude'"),
			whenEdge("pass", "wrap", "vars.mono_family == 'other'"),
			{From: "capped", To: "done"},
			{From: "stale", To: "done"},
			{From: "scan", To: "done"},
			{From: "wrap", To: "done"},
		},
	}
	if !wf.AlwaysReachesLLM() {
		t.Fatal("premise: every path makes a model call, so the cap applies")
	}
	gpt := map[string]any{"mono_family": "gpt"}
	open := wf.LaunchVarEdgeFilter(gpt)
	if open == nil {
		t.Fatal("premise: this launch prunes the topology (the fallback `stale`) and the entry still reaches a terminal — without both, this case decides nothing")
	}
	isCapped := func(n ir.Node) bool { return n != nil && n.NodeID() == "capped" }
	if !wf.CanReachVia(open, isCapped) {
		t.Fatal("premise: the capped route is on this run's paths, so a cap decision is being made here")
	}
	if wf.AlwaysReachesVia(open, isCapped) {
		t.Error("this run's topology reads the capped route as unavoidable: `pass` lost every transition, and a path that reaches no terminal counts as walled")
	}
	for _, id := range []string{"scan", "wrap"} {
		if reaches(wf, open, id) {
			t.Errorf("CanReachVia crosses the stopped node to %s", id)
		}
	}
	assertNodeDeathPark(t, caps, wf, gpt, false)
}

// emptiedNodeGraph puts the capped route behind the node whose exits the
// launch excludes. The side path keeps a terminal reachable independently,
// so the graph-level safeguard cannot conceal the per-node behavior.
func emptiedNodeGraph(vars map[string]*ir.Var) *ir.Workflow {
	return &ir.Workflow{
		Name: "capped_behind", Entry: "pick", Vars: vars,
		Nodes: map[string]ir.Node{
			"pick":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "pick"}, RouterMode: ir.RouterCondition},
			"pass":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "pass"}, RouterMode: ir.RouterCondition},
			"capped": agentRoute("capped", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"stale":  agentRoute("stale", delegate.BackendClaw, "openai", "openai/gpt-6-sol"),
			"wrap":   agentRoute("wrap", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"side":   agentRoute("side", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			whenEdge("pick", "side", "outputs.pick.hot"),
			whenEdge("pick", "pass", "vars.mono_family == 'gpt'"),
			{From: "pick", To: "stale"},
			whenEdge("pass", "capped", "vars.mono_family == 'claude'"),
			whenEdge("pass", "wrap", "vars.mono_family == 'other'"),
			{From: "capped", To: "done"},
			{From: "stale", To: "done"},
			{From: "wrap", To: "done"},
			{From: "side", To: "done"},
		},
	}
}

// W1 (#2468): a soft cap behind a node whose exits are all false cannot
// park this launch or arm a retry. Mutation: restore LaunchVarEdgeFilter's
// old `if len(excluded)+len(fallbacks) == len(edges) { continue }`.
func TestUsageCapPreflight_ASoftCapBehindAStoppedNodeDoesNotPark(t *testing.T) {
	blankAnthropicWireEnv(t)
	wf := emptiedNodeGraph(monoFamilyVar("claude"))
	gpt := map[string]any{"mono_family": "gpt"}
	if !wf.AlwaysReachesLLM() {
		t.Fatal("premise: every unfiltered path spends, so the cap applies")
	}
	open := wf.LaunchVarEdgeFilter(gpt)
	if open == nil || reaches(wf, open, "capped") || reaches(wf, open, "wrap") {
		t.Error("the launch must keep its prune and stop at pass")
	}
	assertNodeDeathPark(t, softCappedForfait(t), wf, gpt, false)
	// An undecided launch retains the whole-graph answer and the same cap
	// really does arm a retry: the no-retry witness has a live control.
	assertNodeDeathPark(t, softCappedForfait(t), emptiedNodeGraph(nil), nil, true)
}

// twoClaudeWorkflow carries two capped claude routes: `early_claude` behind
// the fallback a mono-gpt launch pre-empts, and `late_claude` after the gpt
// reviewer, on every path such a run takes.
func twoClaudeWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name: "two_claude", Entry: "topology", Vars: monoFamilyVar("claude"),
		Nodes: map[string]ir.Node{
			"topology":     &ir.RouterNode{BaseNode: ir.BaseNode{ID: "topology"}, RouterMode: ir.RouterCondition},
			"early_claude": agentRoute("early_claude", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"late_claude":  agentRoute("late_claude", delegate.BackendClaudeCode, "", "claude-opus-5-5"),
			"reviewer_gpt": agentRoute("reviewer_gpt", delegate.BackendClaw, "openai", "openai/gpt-6-astra"),
			"done":         &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			whenEdge("topology", "reviewer_gpt", "vars.mono_family == 'gpt'"),
			{From: "topology", To: "early_claude"},
			{From: "early_claude", To: "done"},
			{From: "reviewer_gpt", To: "late_claude"},
			{From: "late_claude", To: "done"},
		},
	}
}

// The park reason carries a count the operator reads in the run document: it
// must say what the narrowed walk accepted, not how many candidates the
// whole graph offered.
func TestUsageCapPreflight_TheParkReasonCountsOnlyTheRoutesOnTheRunsPaths(t *testing.T) {
	blankAnthropicWireEnv(t)
	caps := softCappedForfait(t)
	ctx := forfaitBesidePinnedKeys()

	wf := twoClaudeWorkflow()
	open := wf.LaunchVarEdgeFilter(map[string]any{"mono_family": "gpt"})
	if reaches(wf, open, "early_claude") || !reaches(wf, open, "late_claude") {
		t.Fatalf("precondition: early_claude off the run's paths (%v), late_claude on them (%v)",
			reaches(wf, open, "early_claude"), reaches(wf, open, "late_claude"))
	}
	err := preflightWithVars(t, caps, ctx, wf, map[string]any{"mono_family": "gpt"})
	if err == nil {
		t.Fatal("started a run whose gpt branch leads to a soft-capped claude route")
	}
	if strings.Contains(err.Error(), "capped routes on the run's paths") {
		t.Errorf("park reason = %q; one capped route is on this run's paths, so the clause must not appear at all", err)
	}
	// A capped SUPERVISOR counts too: it runs on every execution and is no
	// graph node, so no walk reaches it — one capped route on the paths
	// beside it still makes two.
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	// The pinned supervisor spends the key its `anthropic/…` model resolves,
	// not the forfait: cap that window too, at the same reopening.
	bothCapped := usagecap.NewMemStore()
	for _, fp := range []string{"fp-forfait", "fp-ant"} {
		if err := bothCapped.Record(context.Background(), platformKey(fp), usagecap.Reading{
			Window: usagecap.WindowFiveHour, Utilization: 0.95, Status: usagecap.StatusWarning,
			ResetsAt: time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second), ObservedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	supervised := twoClaudeWorkflow()
	supervised.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "anthropic/claude-opus-5-5"}}
	err = preflightWithVars(t, bothCapped, ctx, supervised, map[string]any{"mono_family": "gpt"})
	if err == nil || !strings.Contains(err.Error(), "2 capped routes on the run's paths") {
		t.Errorf("park reason = %v, want 2 — the supervisor and the one capped route on this run's paths", err)
	}
	// Both node routes on the run's paths, beside the supervisor: three.
	chain := twoClaudeWorkflow()
	chain.Supervisors = supervised.Supervisors
	chain.Edges[1] = &ir.Edge{From: "reviewer_gpt", To: "early_claude"}
	chain.Edges[3] = &ir.Edge{From: "early_claude", To: "late_claude"}
	err = preflightWithVars(t, bothCapped, ctx, chain, map[string]any{"mono_family": "gpt"})
	if err == nil || !strings.Contains(err.Error(), "3 capped routes on the run's paths") {
		t.Errorf("park reason = %v, want the 2 routes this run really crosses and its supervisor", err)
	}
}
