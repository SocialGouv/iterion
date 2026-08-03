package runview

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// linearBot: survey -> plan -> implement -> verify -> done.
const linearBot = `schema out:
  value: string

agent survey:
  model: "claude-opus-4-7"
  output: out

agent plan:
  model: "claude-opus-4-7"
  output: out

agent implement:
  model: "claude-opus-4-7"
  output: out

agent verify:
  model: "claude-opus-4-7"
  output: out

workflow linear:
  entry: survey
  survey -> plan
  plan -> implement
  implement -> verify
  verify -> done
`

// loopedBot: implement -> verify -> implement as fix(3). `verify` is both
// downstream AND upstream of `implement`, which is the case the ancestor
// subtraction in downstreamOf exists for.
const loopedBot = `schema out:
  value: string
  ok: bool

agent survey:
  model: "claude-opus-4-7"
  output: out

agent implement:
  model: "claude-opus-4-7"
  output: out

agent verify:
  model: "claude-opus-4-7"
  output: out

workflow looped:
  entry: survey
  survey -> implement
  implement -> verify
  verify -> done when ok
  verify -> implement as fix(3)
`

// fanOutBot: two parallel branches converging on merge.
const fanOutBot = `schema out:
  value: string

agent survey:
  model: "claude-opus-4-7"
  output: out

router split:
  mode: fan_out_all

agent branch_a:
  model: "claude-opus-4-7"
  output: out

agent branch_b:
  model: "claude-opus-4-7"
  output: out

agent merge:
  model: "claude-opus-4-7"
  output: out
  await: wait_all

workflow fanout:
  entry: survey
  survey -> split
  split -> branch_a
  split -> branch_b
  branch_a -> merge
  branch_b -> merge
  merge -> done
`

// seedRun writes a bot fixture plus a run parked with the given
// checkpoint, and returns the service, store, and run id.
func seedRun(t *testing.T, botSrc string, cp *store.Checkpoint, status store.RunStatus) (*Service, store.RunStore, string) {
	t.Helper()
	dir := t.TempDir()
	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(botSrc), 0o644); err != nil {
		t.Fatalf("write bot fixture: %v", err)
	}
	logger := iterlog.Nop()
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	runID := "run-rewind-subject"
	if _, err := st.CreateRun(context.Background(), runID, "wf", map[string]any{"x": 1}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	run.FilePath = botPath
	run.WorkflowHash = "hash-original"
	run.Status = status
	run.Error = "boom: verify failed"
	run.Checkpoint = cp
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save run: %v", err)
	}
	svc, err := NewService(storeDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, st, runID
}

func outputsOf(ids ...string) map[string]map[string]any {
	m := map[string]map[string]any{}
	for _, id := range ids {
		m[id] = map[string]any{"value": id + "-output"}
	}
	return m
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRewind_LinearDropsDownstream is the MVP acceptance path: rewind a
// failed run to a middle node, and assert the run keeps its id while the
// pivot and everything after it is invalidated and everything before it
// survives.
func TestRewind_LinearDropsDownstream(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID:           "verify",
		Outputs:          outputsOf("survey", "plan", "implement", "verify"),
		Vars:             map[string]any{"workflow_var": "v"},
		ArtifactVersions: map[string]int{"implement": 2},
		NodeAttempts:     map[string]map[string]int{"verify": {"EXECUTION_FAILED": 2}, "survey": {"TIMEOUT": 1}},
		BudgetTokensUsed: 4321,
		BudgetCostUSD:    1.25,
		CostUSDTotal:     1.25,
	}
	svc, st, runID := seedRun(t, linearBot, cp, store.RunStatusFailedResumable)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "implement"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if result.RunID != runID {
		t.Errorf("result.RunID = %q, want the SAME run id %q (rewind must not mint a run)", result.RunID, runID)
	}
	if result.FromNode != "verify" || result.NodeID != "implement" {
		t.Errorf("re-anchor = %q → %q, want verify → implement", result.FromNode, result.NodeID)
	}
	wantDropped := []string{"implement", "verify"}
	if got := result.DroppedNodes; len(got) != 2 || got[0] != wantDropped[0] || got[1] != wantDropped[1] {
		t.Errorf("DroppedNodes = %v, want %v", got, wantDropped)
	}

	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load rewound run: %v", err)
	}
	if run.Status != store.RunStatusCancelled {
		t.Errorf("status = %q, want cancelled (the one resumable status the cloud runner will not auto-resume)", run.Status)
	}
	if run.Error != "" {
		t.Errorf("run.Error = %q, want cleared — the run is parked at the pivot, not failed", run.Error)
	}
	got := run.Checkpoint
	if got == nil {
		t.Fatal("checkpoint must survive the rewind")
	}
	if got.NodeID != "implement" {
		t.Errorf("checkpoint.NodeID = %q, want implement", got.NodeID)
	}
	for _, dropped := range []string{"implement", "verify"} {
		if _, ok := got.Outputs[dropped]; ok {
			t.Errorf("output %q survived the rewind; want it invalidated", dropped)
		}
	}
	for _, kept := range []string{"survey", "plan"} {
		if _, ok := got.Outputs[kept]; !ok {
			t.Errorf("upstream output %q was dropped; rewind must not force a replay of already-paid work", kept)
		}
	}
	// Budget is NOT refunded — otherwise rewind+resume is an unbounded
	// way around max_cost_usd.
	if got.BudgetTokensUsed != 4321 || got.BudgetCostUSD != 1.25 || got.CostUSDTotal != 1.25 {
		t.Errorf("budget accounting was reset (tokens=%d cost=%v total=%v), want it preserved",
			got.BudgetTokensUsed, got.BudgetCostUSD, got.CostUSDTotal)
	}
	// Artifact versions keep counting up so re-execution appends a new
	// version instead of overwriting the old one.
	if got.ArtifactVersions["implement"] != 2 {
		t.Errorf("ArtifactVersions[implement] = %d, want 2 preserved", got.ArtifactVersions["implement"])
	}
	// Recovery attempts reset for replayed nodes, survive for the rest.
	if _, ok := got.NodeAttempts["verify"]; ok {
		t.Error("NodeAttempts[verify] survived; a replayed node should get a fresh recovery budget")
	}
	if _, ok := got.NodeAttempts["survey"]; !ok {
		t.Error("NodeAttempts[survey] was dropped; only replayed nodes reset")
	}

	// The audit marker must be appended, not a truncation of history.
	events, err := st.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var found *store.Event
	for _, evt := range events {
		if evt.Type == store.EventRunRewound {
			found = evt
		}
	}
	if found == nil {
		t.Fatal("no run_rewound event appended")
	}
	if found.NodeID != "implement" {
		t.Errorf("run_rewound NodeID = %q, want implement", found.NodeID)
	}
	if found.Data["from_node"] != "verify" || found.Data["to_node"] != "implement" {
		t.Errorf("run_rewound data = %v, want from verify to implement", found.Data)
	}
}

// TestRewind_LoopKeepsCycleAncestor is the reason downstreamOf subtracts
// ancestors. `verify` is forward-reachable from `implement` yet also
// reaches it back through the fix() loop, and `implement` reads its
// output on re-entry — dropping it would break the first replay.
func TestRewind_LoopKeepsCycleAncestor(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID:       "verify",
		Outputs:      outputsOf("survey", "implement", "verify"),
		LoopCounters: map[string]int{"fix": 2},
	}
	svc, st, runID := seedRun(t, loopedBot, cp, store.RunStatusFailedResumable)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "implement"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if len(result.DroppedNodes) != 1 || result.DroppedNodes[0] != "implement" {
		t.Fatalf("DroppedNodes = %v, want only [implement] — verify is a cycle ancestor and feeds {{loop.fix.previous_output}}",
			result.DroppedNodes)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if _, ok := run.Checkpoint.Outputs["verify"]; !ok {
		t.Error("verify's output was dropped; the pivot re-reads it on loop re-entry")
	}
	// Loop counters are not refunded: rewind must not become a way past
	// max_iterations.
	if run.Checkpoint.LoopCounters["fix"] != 2 {
		t.Errorf("LoopCounters[fix] = %d, want 2 preserved", run.Checkpoint.LoopCounters["fix"])
	}
}

// TestRewind_FanOutRewindIsAllOrNothing: naming one branch of a fan-out
// invalidates the WHOLE fan-out, because promotion to the router is the
// only way to replay every parallel execution. Partial-branch rewind was
// the hazard, not the feature: at convergence the checkpoint keeps one
// output per node id, so a surviving sibling's entry is a single
// arbitrary branch's value, not an aggregate.
func TestRewind_FanOutRewindIsAllOrNothing(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID:  "merge",
		Outputs: outputsOf("survey", "split", "branch_a", "branch_b", "merge"),
	}
	svc, st, runID := seedRun(t, fanOutBot, cp, store.RunStatusCancelled)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "branch_a"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	for _, id := range []string{"split", "branch_a", "branch_b", "merge"} {
		if !contains(result.DroppedNodes, id) {
			t.Errorf("%q survived; the fan-out replays as a unit", id)
		}
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if _, ok := run.Checkpoint.Outputs["survey"]; !ok {
		t.Error("survey is upstream of the router and must survive")
	}
}

// TestRewind_ClearsPendingInteractionAndBackendState: a run parked on a
// human question must not carry that question — or the pivot's old
// conversation — into the replay.
func TestRewind_ClearsPendingInteractionAndBackendState(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID:                  "verify",
		Outputs:                 outputsOf("survey", "plan", "implement"),
		InteractionID:           "int-42",
		InteractionQuestions:    map[string]any{"approved": "Ship it?"},
		BackendName:             "claw",
		BackendSessionID:        "sess-1",
		BackendConversation:     json.RawMessage(`[{"role":"assistant"}]`),
		BackendPendingToolUseID: "tool-7",
	}
	svc, st, runID := seedRun(t, linearBot, cp, store.RunStatusPausedWaitingHuman)

	if _, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "implement"}); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	got := run.Checkpoint
	if got.InteractionID != "" || got.InteractionQuestions != nil {
		t.Errorf("pending interaction survived (%q / %v); the rewound run never asked it",
			got.InteractionID, got.InteractionQuestions)
	}
	if got.BackendName != "" || got.BackendSessionID != "" || got.BackendConversation != nil || got.BackendPendingToolUseID != "" {
		t.Error("backend rehydration survived; the pivot must replay against the edited prompt, not the old conversation")
	}
}

// TestRewind_RejectsRunningRun: mutating the checkpoint of a run whose
// engine owns it would be overwritten at the next node boundary.
func TestRewind_RejectsRunningRun(t *testing.T) {
	cp := &store.Checkpoint{NodeID: "verify", Outputs: outputsOf("survey", "plan", "implement")}
	svc, _, runID := seedRun(t, linearBot, cp, store.RunStatusRunning)

	_, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "implement"})
	if !errors.Is(err, ErrRewindNotRewindable) {
		t.Fatalf("Rewind on a running run: err = %v, want ErrRewindNotRewindable", err)
	}
}

// TestRewind_RejectsUnreachedNode guards the typo case: parking a run on
// a node it never executed defers the failure into the engine.
func TestRewind_RejectsUnreachedNode(t *testing.T) {
	cp := &store.Checkpoint{NodeID: "plan", Outputs: outputsOf("survey")}
	svc, _, runID := seedRun(t, linearBot, cp, store.RunStatusFailedResumable)

	_, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "verify"})
	if !errors.Is(err, ErrRewindNodeNotReached) {
		t.Fatalf("Rewind to an unreached node: err = %v, want ErrRewindNodeNotReached", err)
	}
}

// TestRewind_RejectsNodeAbsentFromWorkflow: after an edit that removed
// the node, the operator gets a clear message instead of a run parked on
// a node the graph no longer has.
func TestRewind_RejectsNodeAbsentFromWorkflow(t *testing.T) {
	cp := &store.Checkpoint{NodeID: "verify", Outputs: outputsOf("survey", "plan", "implement", "verify")}
	svc, _, runID := seedRun(t, linearBot, cp, store.RunStatusFailedResumable)

	_, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "nonexistent"})
	if err == nil {
		t.Fatal("expected an error for a node absent from the workflow")
	}
}

// TestRewind_PivotIsCurrentCheckpointNode: rewinding to the node the run
// is already parked on is a legal no-op-ish call (it still clears that
// node's stale output and backend state), so a "retry this node clean"
// gesture does not need a special case.
func TestRewind_PivotIsCurrentCheckpointNode(t *testing.T) {
	cp := &store.Checkpoint{NodeID: "verify", Outputs: outputsOf("survey", "plan", "implement")}
	svc, st, runID := seedRun(t, linearBot, cp, store.RunStatusFailedResumable)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "verify"})
	if err != nil {
		t.Fatalf("Rewind to the current checkpoint node: %v", err)
	}
	if len(result.DroppedNodes) != 1 || result.DroppedNodes[0] != "verify" {
		t.Errorf("DroppedNodes = %v, want [verify]", result.DroppedNodes)
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if got := keysOf(run.Checkpoint.Outputs); len(got) != 3 {
		t.Errorf("upstream outputs = %v, want survey/plan/implement preserved", got)
	}
}

// TestRewind_ReleasesSubbotChildPointers is the subbot correctness case.
//
// ReattachSubbotChild consults ONLY run.SubbotChildren and the child's
// status — never the parent's checkpoint — and it runs before the child
// .bot is compiled. So dropping the subbot node's OUTPUT is not enough:
// without releasing the pointer, a rewound subbot node adopts the
// pre-rewind child and the edited child workflow never executes.
func TestRewind_ReleasesSubbotChildPointers(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID:  "verify",
		Outputs: outputsOf("survey", "plan", "implement", "verify"),
	}
	svc, st, runID := seedRun(t, linearBot, cp, store.RunStatusPausedWaitingHuman)

	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	// Pointers shaped like the engine's subbotReattachKey: bare, loop-
	// qualified, and fan-out-branch-qualified. Plus one belonging to an
	// UPSTREAM node, which must survive.
	run.SubbotChildren = map[string]string{
		"implement":                "child-implement",
		"implement@fix=2":          "child-implement-iter2",
		"implement#branch_split_0": "child-implement-branch",
		"plan":                     "child-plan-upstream",
		"implementation_helper":    "child-lookalike",
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save run: %v", err)
	}

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "implement"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}

	got, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, key := range []string{"implement", "implement@fix=2", "implement#branch_split_0"} {
		if _, still := got.SubbotChildren[key]; still {
			t.Errorf("subbot pointer %q survived; the replay would re-attach to the stale child", key)
		}
	}
	// An upstream node's child is not downstream of the pivot.
	if got.SubbotChildren["plan"] != "child-plan-upstream" {
		t.Error("upstream subbot pointer was released; only dropped nodes lose theirs")
	}
	// Prefix matching must key on the '@'/'#' delimiter, not a bare
	// prefix, or "implement" would steal "implementation_helper".
	if got.SubbotChildren["implementation_helper"] != "child-lookalike" {
		t.Error("a node whose id merely starts with the pivot's id lost its pointer")
	}

	wantOrphans := map[string]bool{"child-implement": true, "child-implement-iter2": true, "child-implement-branch": true}
	if len(result.OrphanedChildRuns) != len(wantOrphans) {
		t.Fatalf("OrphanedChildRuns = %v, want the three released children", result.OrphanedChildRuns)
	}
	for _, id := range result.OrphanedChildRuns {
		if !wantOrphans[id] {
			t.Errorf("unexpected orphaned child %q", id)
		}
	}
}

// TestRewind_PromotesFanOutBodyToRouter: the checkpoint holds one output
// per node id, so a body node's N parallel executions collapse to one
// entry. Anchoring there would replay it once, linearly — testing a
// single iteration instead of all of them, with no signal. The rewind
// promotes to the router so the whole fan-out replays.
func TestRewind_PromotesFanOutBodyToRouter(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID:  "merge",
		Outputs: outputsOf("survey", "split", "branch_a", "branch_b", "merge"),
	}
	svc, st, runID := seedRun(t, fanOutBot, cp, store.RunStatusFailedResumable)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "branch_a"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if result.NodeID != "split" {
		t.Fatalf("pivot = %q, want split (the fan-out router)", result.NodeID)
	}
	if result.PromotedFrom != "branch_a" {
		t.Errorf("PromotedFrom = %q, want branch_a", result.PromotedFrom)
	}
	// Promoting to the router means BOTH branches replay, not just the
	// one that was named.
	for _, id := range []string{"split", "branch_a", "branch_b", "merge"} {
		if !contains(result.DroppedNodes, id) {
			t.Errorf("%q survived; promoting to the router must invalidate the whole fan-out", id)
		}
	}
	if contains(result.DroppedNodes, "survey") {
		t.Error("survey is upstream of the router and must survive")
	}
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if run.Checkpoint.NodeID != "split" {
		t.Errorf("checkpoint anchored on %q, want split", run.Checkpoint.NodeID)
	}
}

// TestRewind_NoPromotionOutsideFanOut: a node on the main path keeps the
// pivot it was given.
func TestRewind_NoPromotionOutsideFanOut(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID:  "merge",
		Outputs: outputsOf("survey", "split", "branch_a", "branch_b", "merge"),
	}
	svc, _, runID := seedRun(t, fanOutBot, cp, store.RunStatusFailedResumable)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "merge"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if result.NodeID != "merge" || result.PromotedFrom != "" {
		t.Errorf("pivot = %q (promoted from %q), want merge unpromoted — the convergence is outside the body",
			result.NodeID, result.PromotedFrom)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestRewind_ReleasesSubbotPointerOfUncompletedNode is the case the first
// version of this feature got wrong, and that its own test masked.
//
// A surviving SubbotChildren entry means the subbot did NOT complete
// (ClearSubbotChild fires on every successful consumption), so that node
// has no output. Keying the cleanup on the output-filtered `dropped` list
// therefore skipped exactly the entries that needed releasing — the
// original test happened to seed the one configuration where pivot ==
// subbot node, which is the only overlap between the two sets.
func TestRewind_ReleasesSubbotPointerOfUncompletedNode(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID: "verify",
		// `implement` is downstream of the pivot but never completed: no
		// output, yet a parked child.
		Outputs:      outputsOf("survey", "plan"),
		NodeAttempts: map[string]map[string]int{"implement": {"EXECUTION_FAILED": 3}},
	}
	svc, st, runID := seedRun(t, linearBot, cp, store.RunStatusPausedWaitingHuman)
	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.SubbotChildren = map[string]string{"implement": "child-parked"}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "plan"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	got, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, still := got.SubbotChildren["implement"]; still {
		t.Error("the parked child's pointer survived; the replay would re-attach to it and never run the edited child workflow")
	}
	if !contains(result.OrphanedChildRuns, "child-parked") {
		t.Errorf("OrphanedChildRuns = %v, want the released child reported", result.OrphanedChildRuns)
	}
	// Same filter bug affected the recovery budget of the node that failed.
	if _, still := got.Checkpoint.NodeAttempts["implement"]; still {
		t.Error("NodeAttempts survived for the node that actually failed — its budget must reset on replay")
	}
}
