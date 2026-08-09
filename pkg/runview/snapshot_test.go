package runview

import (
	"strconv"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// evt is a tiny helper to build a store.Event with a fixed timestamp
// so tests don't have to thread time through every line.
func evt(seq int64, t store.EventType, branch, node string, data map[string]any) *store.Event {
	return &store.Event{
		Seq:       seq,
		Timestamp: time.Unix(seq, 0).UTC(),
		Type:      t,
		BranchID:  branch,
		NodeID:    node,
		Data:      data,
	}
}

// ---------------------------------------------------------------------------
// Reducer tests
// ---------------------------------------------------------------------------

func TestSnapshotReducer_LinearRun(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		evt(1, store.EventNodeStarted, "", "analyze", map[string]any{"kind": "agent"}),
		evt(2, store.EventNodeFinished, "", "analyze", nil),
		evt(3, store.EventNodeStarted, "", "verify", map[string]any{"kind": "judge"}),
		evt(4, store.EventNodeFinished, "", "verify", nil),
		evt(5, store.EventRunFinished, "", "", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 2 {
		t.Fatalf("Executions = %d, want 2", got)
	}
	if snap.Executions[0].IRNodeID != "analyze" || snap.Executions[0].LoopIteration != 0 {
		t.Errorf("first exec = %+v, want analyze/0", snap.Executions[0])
	}
	if snap.Executions[0].Kind != "agent" {
		t.Errorf("first exec Kind = %q, want agent", snap.Executions[0].Kind)
	}
	if snap.Executions[0].Status != ExecStatusFinished {
		t.Errorf("first exec Status = %q, want finished", snap.Executions[0].Status)
	}
	if snap.LastSeq != 5 {
		t.Errorf("LastSeq = %d, want 5", snap.LastSeq)
	}
}

func TestSnapshotReducer_LoopIterations(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	// Loop body: same node fires three times in main branch.
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "fix", map[string]any{"kind": "agent"}),
		evt(1, store.EventNodeFinished, "", "fix", nil),
		evt(2, store.EventEdgeSelected, "", "", map[string]any{"loop": "until_green", "iteration": 1}),
		evt(3, store.EventNodeStarted, "", "fix", map[string]any{"kind": "agent"}),
		evt(4, store.EventNodeFinished, "", "fix", nil),
		evt(5, store.EventEdgeSelected, "", "", map[string]any{"loop": "until_green", "iteration": 2}),
		evt(6, store.EventNodeStarted, "", "fix", map[string]any{"kind": "agent"}),
		evt(7, store.EventNodeFinished, "", "fix", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 3 {
		t.Fatalf("Executions = %d, want 3", got)
	}
	for i, ex := range snap.Executions {
		if ex.IRNodeID != "fix" {
			t.Errorf("[%d] IRNodeID = %q, want fix", i, ex.IRNodeID)
		}
		if ex.LoopIteration != i {
			t.Errorf("[%d] LoopIteration = %d, want %d", i, ex.LoopIteration, i)
		}
		if ex.Status != ExecStatusFinished {
			t.Errorf("[%d] Status = %q, want finished", i, ex.Status)
		}
	}
	// Each iteration must have a distinct execution_id.
	seen := make(map[string]bool)
	for _, ex := range snap.Executions {
		if seen[ex.ExecutionID] {
			t.Errorf("duplicate execution_id %q", ex.ExecutionID)
		}
		seen[ex.ExecutionID] = true
	}
}

func TestSnapshotReducer_MonotonicGuardAgainstDuplicateNodeStarted(t *testing.T) {
	// Mirror of studio T2.4 (9bcccff). The runtime can re-emit
	// node_started for the same (branch, node, iteration) when the
	// node is not on a loop edge directly and currentLoopIteration
	// returns the same value for successive runs (observed live as
	// family_upgrade#0 flickering finished -> running -> finished).
	// A duplicate node_started must NOT downgrade an already-terminal
	// execution back to running, and must NOT add a second entry to
	// the order slice (otherwise Snapshot() emits the same exec twice).
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "family_upgrade",
			map[string]any{"kind": "compute", "iteration": 0}),
		evt(1, store.EventNodeFinished, "", "family_upgrade", nil),
		// Stale or runtime re-emission for the same (node, iter) —
		// must be ignored at status level, must not duplicate order.
		evt(2, store.EventNodeStarted, "", "family_upgrade",
			map[string]any{"kind": "compute", "iteration": 0}),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 1 {
		t.Fatalf("Executions = %d, want 1 (duplicate node_started must not append)", got)
	}
	if snap.Executions[0].Status != ExecStatusFinished {
		t.Errorf("Status = %q, want finished (terminal must not downgrade)", snap.Executions[0].Status)
	}
	// FirstSeq is anchored on the original start, not the duplicate.
	if got := snap.Executions[0].FirstSeq; got != 0 {
		t.Errorf("FirstSeq = %d, want 0 (preserved across duplicate)", got)
	}
}

func TestSnapshotReducer_MonotonicGuardAgainstStaleStartAfterFailure(t *testing.T) {
	// A node that failed must not be flipped back to running by a
	// duplicate node_started event. Same rule as Finished above; this
	// covers the path where the runtime emits a retry node_started
	// without first emitting a fresh exec id.
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "build", map[string]any{"iteration": 0}),
		evt(1, store.EventRunFailed, "", "build", map[string]any{"error": "boom"}),
		evt(2, store.EventNodeStarted, "", "build", map[string]any{"iteration": 0}),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 1 {
		t.Fatalf("Executions = %d, want 1", got)
	}
	ex := snap.Executions[0]
	if ex.Status != ExecStatusFailed {
		t.Errorf("Status = %q, want failed (terminal must not downgrade)", ex.Status)
	}
	if ex.Error != "boom" {
		t.Errorf("Error = %q, want boom (preserved)", ex.Error)
	}
}

func TestSnapshotReducer_PostResumeReExecutionFlipsBackToRunning(t *testing.T) {
	// Long-standing recurring bug: after a `failed_resumable` resume,
	// the runtime re-executes the failed checkpoint node with the SAME
	// (branch, node, iter) — therefore the same exec_id. The monotonic
	// guard in handleNodeStarted refuses to downgrade terminal → running
	// on a duplicate, which is correct for WS-history replay but locks
	// the canvas on the pre-resume terminal status when a true re-
	// execution arrives. Operators report it as "pipeline running but
	// no node currently running" across many sessions.
	//
	// The fix tracks lastResumedSeq: an existing terminal exec whose
	// LastSeq predates the latest run_resumed is a pre-resume artefact
	// and a node_started with seq > lastResumedSeq must flip it back
	// to running with fresh started_at / cleared finished_at / cleared
	// error.
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "commit_changes", map[string]any{"iteration": 5}),
		evt(1, store.EventRunFailed, "", "commit_changes", map[string]any{"error": "git add failed"}),
		evt(2, store.EventRunResumed, "", "", nil),
		evt(3, store.EventNodeStarted, "", "commit_changes", map[string]any{"iteration": 5}),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 1 {
		t.Fatalf("Executions = %d, want 1 (post-resume re-run keeps the same exec_id)", got)
	}
	ex := snap.Executions[0]
	if ex.Status != ExecStatusRunning {
		t.Errorf("Status = %q, want running (post-resume re-execution must flip back)", ex.Status)
	}
	if ex.Error != "" {
		t.Errorf("Error = %q, want empty (cleared on fresh attempt)", ex.Error)
	}
	if ex.FinishedAt != nil {
		t.Errorf("FinishedAt = %v, want nil (cleared on fresh attempt)", ex.FinishedAt)
	}
	// FirstSeq stays anchored on the original event (the pre-resume
	// attempt) so scrubbing / log-window calculations still find the
	// historical execution.
	if ex.FirstSeq != 0 {
		t.Errorf("FirstSeq = %d, want 0 (anchored on first attempt)", ex.FirstSeq)
	}
	// LastSeq is bumped to the new attempt's seq.
	if ex.LastSeq != 3 {
		t.Errorf("LastSeq = %d, want 3 (latest event)", ex.LastSeq)
	}
}

func TestSnapshotReducer_PreResumeDuplicateStillGuarded(t *testing.T) {
	// Defense-in-depth for the post-resume fix: a duplicate
	// node_started that arrives BEFORE any run_resumed (e.g. classic
	// WS history replay) must STILL preserve the terminal status.
	// The lastResumedSeq comparison handles both cases with one rule.
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "build", map[string]any{"iteration": 0}),
		evt(1, store.EventNodeFinished, "", "build", nil),
		// Stale duplicate from a WS replay — no resume in between.
		evt(2, store.EventNodeStarted, "", "build", map[string]any{"iteration": 0}),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if len(snap.Executions) != 1 {
		t.Fatalf("Executions = %d, want 1", len(snap.Executions))
	}
	if snap.Executions[0].Status != ExecStatusFinished {
		t.Errorf("Status = %q, want finished (no run_resumed between, guard still applies)",
			snap.Executions[0].Status)
	}
}

func TestSnapshotReducer_NestedLoopIterationPathDisambiguates(t *testing.T) {
	// Recurring "no node running" bug, nested-loop manifestation:
	// validate_upgrade lives in fix_loop ⊂ package_loop ⊂ family_loop.
	// The runtime's currentLoopIteration returns max() across all
	// containing loops, so every package's attempt at fix=0,pkg=0
	// resolved to whatever family_loop's counter was (e.g. 5),
	// collapsing N package attempts onto ONE exec_id. The monotonic
	// terminal guard then locked the canvas on the first attempt's
	// finished status.
	//
	// Option 3 (this test): the runtime additionally stamps a stable
	// `iteration_path` string encoding all containing loops' counters
	// onto the node_started payload. makeExecutionIDFromEvent prefers
	// the path over the scalar, so each (loop counters tuple) gets a
	// strictly distinct exec_id even when the scalar iteration is the
	// same. Two sequential package attempts with the same `iteration`
	// must therefore produce TWO executions, not one.
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "validate_upgrade", map[string]any{
			"kind":           "judge",
			"iteration":      5,
			"iteration_path": "family_loop=5;fix_loop=0;package_loop=0",
		}),
		evt(1, store.EventNodeFinished, "", "validate_upgrade", nil),
		evt(2, store.EventNodeStarted, "", "validate_upgrade", map[string]any{
			"kind":           "judge",
			"iteration":      5,
			"iteration_path": "family_loop=5;fix_loop=0;package_loop=1",
		}),
		evt(3, store.EventNodeFinished, "", "validate_upgrade", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 2 {
		t.Fatalf("Executions = %d, want 2 (distinct iteration_path must not collapse)", got)
	}
	seen := make(map[string]bool)
	for i, ex := range snap.Executions {
		if seen[ex.ExecutionID] {
			t.Errorf("duplicate execution_id %q at index %d", ex.ExecutionID, i)
		}
		seen[ex.ExecutionID] = true
		if ex.Status != ExecStatusFinished {
			t.Errorf("[%d] Status = %q, want finished", i, ex.Status)
		}
	}
}

func TestSnapshotReducer_LegacyEventsWithoutIterationPathFallBack(t *testing.T) {
	// Backward compat: historical event streams emitted by runtimes
	// older than Option 3 don't carry `iteration_path`. They must
	// still replay deterministically through the scalar `iteration`
	// path so cold replays of archived runs render correctly.
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "build", map[string]any{"iteration": 0}),
		evt(1, store.EventNodeFinished, "", "build", nil),
		evt(2, store.EventNodeStarted, "", "build", map[string]any{"iteration": 1}),
		evt(3, store.EventNodeFinished, "", "build", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 2 {
		t.Fatalf("Executions = %d, want 2 (legacy int-iter must still disambiguate)", got)
	}
	if snap.Executions[0].ExecutionID == snap.Executions[1].ExecutionID {
		t.Errorf("legacy exec_ids collided: %q", snap.Executions[0].ExecutionID)
	}
}

func TestSnapshotReducer_MonotonicGuardAgainstStaleStartAfterPause(t *testing.T) {
	// A node paused waiting for human input must not flip back to
	// running on a stale node_started — only run_resumed transitions
	// out of paused (handleRunResumed).
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "ask", map[string]any{"kind": "human", "iteration": 0}),
		evt(1, store.EventHumanInputRequested, "", "ask", nil),
		// Spurious replay of node_started while still awaiting input.
		evt(2, store.EventNodeStarted, "", "ask", map[string]any{"kind": "human", "iteration": 0}),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 1 {
		t.Fatalf("Executions = %d, want 1", got)
	}
	if snap.Executions[0].Status != ExecStatusPaused {
		t.Errorf("Status = %q, want paused_waiting_human", snap.Executions[0].Status)
	}
}

func TestSnapshotReducer_FanOutBranches(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	// Fan-out: same node ID runs in two different branches in parallel.
	events := []*store.Event{
		evt(0, store.EventBranchStarted, "br_a", "", nil),
		evt(1, store.EventBranchStarted, "br_b", "", nil),
		evt(2, store.EventNodeStarted, "br_a", "review", map[string]any{"kind": "judge"}),
		evt(3, store.EventNodeStarted, "br_b", "review", map[string]any{"kind": "judge"}),
		evt(4, store.EventNodeFinished, "br_a", "review", nil),
		evt(5, store.EventNodeFinished, "br_b", "review", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 2 {
		t.Fatalf("Executions = %d, want 2", got)
	}
	branchSet := map[string]bool{}
	for _, ex := range snap.Executions {
		branchSet[ex.BranchID] = true
		if ex.LoopIteration != 0 {
			t.Errorf("branch %s LoopIteration = %d, want 0", ex.BranchID, ex.LoopIteration)
		}
		if ex.Status != ExecStatusFinished {
			t.Errorf("branch %s Status = %q, want finished", ex.BranchID, ex.Status)
		}
	}
	if !branchSet["br_a"] || !branchSet["br_b"] {
		t.Errorf("branches = %v, want br_a + br_b", branchSet)
	}
}

func TestSnapshotReducer_HumanPauseResume(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "ask", map[string]any{"kind": "human"}),
		evt(1, store.EventHumanInputRequested, "", "ask", nil),
		evt(2, store.EventRunPaused, "", "", nil),
		evt(3, store.EventRunResumed, "", "", nil),
		evt(4, store.EventNodeFinished, "", "ask", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if len(snap.Executions) != 1 {
		t.Fatalf("Executions = %d, want 1", len(snap.Executions))
	}
	if snap.Executions[0].Status != ExecStatusFinished {
		t.Errorf("Status after resume+finish = %q, want finished", snap.Executions[0].Status)
	}
}

func TestSnapshotReducer_NodeFailure(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "build", map[string]any{"kind": "tool"}),
		evt(1, store.EventRunFailed, "", "build", map[string]any{"error": "exit 1"}),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if len(snap.Executions) != 1 {
		t.Fatalf("Executions = %d, want 1", len(snap.Executions))
	}
	ex := snap.Executions[0]
	if ex.Status != ExecStatusFailed {
		t.Errorf("Status = %q, want failed", ex.Status)
	}
	if ex.Error != "exit 1" {
		t.Errorf("Error = %q, want exit 1", ex.Error)
	}
}

func TestSnapshotReducer_RunCancelledClosesInflight(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "build", map[string]any{"kind": "tool"}),
		evt(1, store.EventNodeFinished, "", "build", nil),
		evt(2, store.EventNodeStarted, "", "deploy", map[string]any{"kind": "agent"}),
		// User hits cancel while "deploy" is still running. The event
		// carries no node_id (run-level), so the old code left deploy
		// stuck in ExecStatusRunning and the spinner never cleared.
		evt(3, store.EventRunCancelled, "", "", map[string]any{"reason": "user cancelled"}),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if len(snap.Executions) != 2 {
		t.Fatalf("Executions = %d, want 2", len(snap.Executions))
	}
	for _, ex := range snap.Executions {
		if ex.Status == ExecStatusRunning {
			t.Errorf("execution %q still running after cancel", ex.IRNodeID)
		}
		if ex.FinishedAt == nil {
			t.Errorf("execution %q has no FinishedAt after cancel", ex.IRNodeID)
		}
	}
	deploy := snap.Executions[1]
	if deploy.IRNodeID != "deploy" {
		t.Fatalf("Executions[1] = %q, want deploy", deploy.IRNodeID)
	}
	if deploy.Status != ExecStatusFailed {
		t.Errorf("deploy.Status = %q, want failed (cancelled inflight)", deploy.Status)
	}
	if deploy.Error == "" {
		t.Errorf("deploy.Error empty, want a cancellation reason")
	}
}

func TestSnapshotReducer_RunFailedClosesInflight(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "fetch", map[string]any{"kind": "agent"}),
		// run-level failure with no node_id (e.g. budget exceeded) —
		// in-flight node must still be closed.
		evt(1, store.EventRunFailed, "", "", map[string]any{"error": "budget exhausted"}),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if len(snap.Executions) != 1 {
		t.Fatalf("Executions = %d, want 1", len(snap.Executions))
	}
	ex := snap.Executions[0]
	if ex.Status != ExecStatusFailed {
		t.Errorf("Status = %q, want failed", ex.Status)
	}
	if ex.Error != "budget exhausted" {
		t.Errorf("Error = %q, want budget exhausted", ex.Error)
	}
}

func TestSnapshotReducer_RunningNodeTouchesLastSeqForStructuredEvents(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "agent", map[string]any{"kind": "agent"}),
		evt(1, store.EventLLMPrompt, "", "agent", map[string]any{"user_message": "hello"}),
		evt(2, store.EventLLMRequest, "", "agent", map[string]any{"model": "m"}),
		evt(3, store.EventToolCalled, "", "agent", map[string]any{"tool_name": "Read"}),
		evt(4, store.EventBudgetWarning, "", "agent", map[string]any{"message": "near limit"}),
		// A node-scoped event for another branch must not advance the main
		// branch execution window.
		evt(5, store.EventToolCalled, "other", "agent", map[string]any{"tool_name": "Write"}),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if len(snap.Executions) != 1 {
		t.Fatalf("Executions = %d, want 1", len(snap.Executions))
	}
	ex := snap.Executions[0]
	if ex.Status != ExecStatusRunning {
		t.Errorf("Status = %q, want running", ex.Status)
	}
	if ex.CurrentEventSeq != 4 || ex.LastSeq != 4 {
		t.Errorf("seqs = current %d last %d, want 4/4", ex.CurrentEventSeq, ex.LastSeq)
	}
}

func TestSnapshotReducer_ArtifactVersion(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "build", nil),
		evt(1, store.EventArtifactWritten, "", "build", map[string]any{"version": 0}),
		evt(2, store.EventNodeFinished, "", "build", nil),
		evt(3, store.EventNodeStarted, "", "build", nil),
		evt(4, store.EventArtifactWritten, "", "build", map[string]any{"version": 1}),
		evt(5, store.EventNodeFinished, "", "build", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if len(snap.Executions) != 2 {
		t.Fatalf("Executions = %d, want 2", len(snap.Executions))
	}
	if v := snap.Executions[0].LastArtifactVersion; v == nil || *v != 0 {
		t.Errorf("first artifact version = %v, want 0", v)
	}
	if v := snap.Executions[1].LastArtifactVersion; v == nil || *v != 1 {
		t.Errorf("second artifact version = %v, want 1", v)
	}
}

func TestSnapshotReducer_OutOfOrderEventIgnored(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	b.Apply(evt(0, store.EventNodeStarted, "", "a", nil))
	b.Apply(evt(2, store.EventNodeFinished, "", "a", nil))
	// Stale event from before LastSeq — should be ignored.
	b.Apply(evt(1, store.EventNodeStarted, "", "stale", nil))
	snap := b.Snapshot()
	if len(snap.Executions) != 1 {
		t.Fatalf("Executions = %d, want 1", len(snap.Executions))
	}
	if snap.Executions[0].IRNodeID != "a" {
		t.Errorf("IRNodeID = %q, want a", snap.Executions[0].IRNodeID)
	}
	if snap.LastSeq != 2 {
		t.Errorf("LastSeq = %d, want 2", snap.LastSeq)
	}
}

func TestSnapshotReducer_DeterministicReplay(t *testing.T) {
	// The reducer is the foundation of the time-travel scrubber:
	// folding events 0..N must produce the same snapshot regardless of
	// how many times we call it. Verify by comparing two independent
	// builds.
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "a", nil),
		evt(1, store.EventNodeFinished, "", "a", nil),
		evt(2, store.EventNodeStarted, "br_x", "b", nil),
		evt(3, store.EventNodeFinished, "br_x", "b", nil),
	}

	build := func() *RunSnapshot {
		b := NewSnapshotBuilder(&store.Run{ID: "r1"})
		for _, e := range events {
			b.Apply(e)
		}
		return b.Snapshot()
	}
	a := build()
	b2 := build()
	if len(a.Executions) != len(b2.Executions) {
		t.Fatalf("len mismatch: %d vs %d", len(a.Executions), len(b2.Executions))
	}
	for i := range a.Executions {
		if a.Executions[i].ExecutionID != b2.Executions[i].ExecutionID {
			t.Errorf("[%d] mismatch: %q vs %q", i, a.Executions[i].ExecutionID, b2.Executions[i].ExecutionID)
		}
	}
}

func TestParseExecutionID_RoundTrip(t *testing.T) {
	cases := []struct {
		branch, node string
		iteration    int
	}{
		{"main", "analyze", 0},
		{"br_a", "review", 3},
		{"main", "compute_until_green", 12},
	}
	for _, c := range cases {
		id := MakeExecutionID(c.branch, c.node, c.iteration)
		gotBranch, gotNode, gotIter, err := ParseExecutionID(id)
		if err != nil {
			t.Errorf("ParseExecutionID(%q): %v", id, err)
			continue
		}
		if gotBranch != c.branch || gotNode != c.node || gotIter != c.iteration {
			t.Errorf("ParseExecutionID(%q) = (%q,%q,%d), want (%q,%q,%d)",
				id, gotBranch, gotNode, gotIter, c.branch, c.node, c.iteration)
		}
	}
}

func TestParseExecutionID_Invalid(t *testing.T) {
	cases := []string{"", "notexec:foo", "exec:onlyone", "exec:a:b:c", "exec:a:b:notanumber"}
	for _, in := range cases {
		_, _, _, err := ParseExecutionID(in)
		if err == nil {
			t.Errorf("ParseExecutionID(%q) returned nil error, want error", in)
		}
	}
}

// ---------------------------------------------------------------------------
// Active-duration timer reducer tests
// ---------------------------------------------------------------------------
//
// The evt helper anchors timestamps on `seq` (Unix seconds), so the tests
// below pick seqs that double as the wall-clock the run reached.

func TestSnapshotReducer_Timer(t *testing.T) {
	cases := []struct {
		name             string
		events           []*store.Event
		wantActiveMs     int64
		wantAnchorUnix   int64 // 0 means expect nil anchor
		anchorIsExpected bool
	}{
		{
			name: "start_then_finish",
			events: []*store.Event{
				evt(0, store.EventRunStarted, "", "", nil),
				evt(10, store.EventRunFinished, "", "", nil),
			},
			wantActiveMs: 10_000,
		},
		{
			name: "pause_resume_finish_excludes_pause_gap",
			events: []*store.Event{
				evt(0, store.EventRunStarted, "", "", nil),
				evt(10, store.EventRunPaused, "", "", nil),
				evt(40, store.EventRunResumed, "", "", nil),
				evt(45, store.EventRunFinished, "", "", nil),
			},
			wantActiveMs: 15_000,
		},
		{
			// run_failed terminates the active window without an explicit
			// node id (engine emits a run-level failure on budget exceeded,
			// for example). The subsequent run_resumed must re-anchor.
			name: "failed_resumable_excludes_offline_gap",
			events: []*store.Event{
				evt(0, store.EventRunStarted, "", "", nil),
				evt(5, store.EventRunFailed, "", "", map[string]any{"error": "boom"}),
				evt(100, store.EventRunResumed, "", "", nil),
				evt(108, store.EventRunFinished, "", "", nil),
			},
			wantActiveMs: 13_000,
		},
		{
			name: "interrupted_freezes_like_pause",
			events: []*store.Event{
				evt(0, store.EventRunStarted, "", "", nil),
				evt(7, store.EventRunInterrupted, "", "", nil),
			},
			wantActiveMs: 7_000,
		},
		{
			// No terminal event after resume — CurrentRunStart must be
			// left anchored so the live frontend ticker keeps accruing.
			name: "resume_without_terminal_keeps_anchor",
			events: []*store.Event{
				evt(0, store.EventRunStarted, "", "", nil),
				evt(10, store.EventRunPaused, "", "", nil),
				evt(30, store.EventRunResumed, "", "", nil),
			},
			wantActiveMs:     10_000,
			wantAnchorUnix:   30,
			anchorIsExpected: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
			for _, e := range tc.events {
				b.Apply(e)
			}
			snap := b.Snapshot()
			if got := snap.Run.ActiveDurationMs; got != tc.wantActiveMs {
				t.Errorf("ActiveDurationMs = %d, want %d", got, tc.wantActiveMs)
			}
			if !tc.anchorIsExpected {
				if snap.Run.CurrentRunStart != nil {
					t.Errorf("CurrentRunStart = %v, want nil", snap.Run.CurrentRunStart)
				}
				return
			}
			if snap.Run.CurrentRunStart == nil {
				t.Fatalf("CurrentRunStart = nil, want anchor at t=%d", tc.wantAnchorUnix)
			}
			if got := snap.Run.CurrentRunStart.Unix(); got != tc.wantAnchorUnix {
				t.Errorf("CurrentRunStart unix = %d, want %d", got, tc.wantAnchorUnix)
			}
		})
	}
}

func TestSnapshotReducer_TimerColdLoadRunningFallback(t *testing.T) {
	// Cold-load: header status=running but no events flushed yet.
	// headerFromRun must seed CurrentRunStart from CreatedAt so the
	// live ticker starts immediately rather than reading 0.
	created := time.Unix(100, 0).UTC()
	b := NewSnapshotBuilder(&store.Run{
		ID:        "r1",
		Status:    store.RunStatusRunning,
		CreatedAt: created,
	})
	snap := b.Snapshot()
	if snap.Run.CurrentRunStart == nil {
		t.Fatalf("CurrentRunStart = nil, want fallback to CreatedAt")
	}
	if !snap.Run.CurrentRunStart.Equal(created) {
		t.Errorf("CurrentRunStart = %v, want CreatedAt %v", snap.Run.CurrentRunStart, created)
	}
	if snap.Run.ActiveDurationMs != 0 {
		t.Errorf("ActiveDurationMs = %d, want 0", snap.Run.ActiveDurationMs)
	}
}

func TestSnapshotReducer_WorktreeFinalizationFieldsPropagate(t *testing.T) {
	// The Commits-tab merge UI keys off run.worktree, run.final_branch,
	// and run.merge_status to decide between "no merge needed" and the
	// merge form. Regression for run_1778021294883: every field a
	// finalized worktree run carries on store.Run must round-trip into
	// the snapshot's RunHeader untouched.
	finished := time.Unix(100, 0).UTC()
	b := NewSnapshotBuilder(&store.Run{
		ID:            "r-finalized",
		Status:        store.RunStatusFinished,
		FinishedAt:    &finished,
		Worktree:      true,
		WorkDir:       "/some/path/.iterion/worktrees/r-finalized",
		FinalCommit:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		FinalBranch:   "iterion/run/swift-cedar-a3f2",
		MergeStrategy: store.MergeStrategySquash,
		MergeStatus:   store.MergeStatusPending,
	})
	h := b.Snapshot().Run
	if !h.Worktree {
		t.Errorf("RunHeader.Worktree = false, want true")
	}
	if h.FinalBranch != "iterion/run/swift-cedar-a3f2" {
		t.Errorf("RunHeader.FinalBranch = %q, want iterion/run/swift-cedar-a3f2", h.FinalBranch)
	}
	if h.MergeStatus != store.MergeStatusPending {
		t.Errorf("RunHeader.MergeStatus = %q, want pending", h.MergeStatus)
	}
	if h.MergeStrategy != store.MergeStrategySquash {
		t.Errorf("RunHeader.MergeStrategy = %q, want squash", h.MergeStrategy)
	}
}

func TestSnapshotReducer_WorktreeAvailableReflectsOnDiskDir(t *testing.T) {
	// The studio gates its inline file-editor affordances on
	// WorktreeAvailable so a click can never 409. It must be true for a
	// worktree that exists on this server's disk (a live local run) and
	// false for one that doesn't (a cloud run's runner-pod worktree, or a
	// finalized/gc'd local run).
	live := t.TempDir()
	for _, tc := range []struct {
		name    string
		workDir string
		want    bool
	}{
		{"on-disk worktree", live, true},
		{"absent worktree", "/nonexistent/runner/pod/worktree", false},
		{"empty workdir", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewSnapshotBuilder(&store.Run{
				ID:       "r",
				Status:   store.RunStatusRunning,
				WorkDir:  tc.workDir,
				Worktree: true,
			}).Snapshot().Run
			if h.WorktreeAvailable != tc.want {
				t.Errorf("WorktreeAvailable = %v, want %v", h.WorktreeAvailable, tc.want)
			}
		})
	}
}

func TestSnapshotReducer_TimerSetRunPreservesCounters(t *testing.T) {
	// SetRun is invoked on terminal-event paths to refresh the header
	// from run.json. It must not clobber the accumulated active
	// duration that the events already taught the reducer.
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	for _, e := range []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		evt(12, store.EventRunPaused, "", "", nil),
	} {
		b.Apply(e)
	}
	if got := b.Snapshot().Run.ActiveDurationMs; got != 12000 {
		t.Fatalf("pre-SetRun ActiveDurationMs = %d, want 12000", got)
	}
	finished := time.Unix(20, 0).UTC()
	b.SetRun(&store.Run{
		ID:         "r1",
		Status:     store.RunStatusPausedWaitingHuman,
		FinishedAt: &finished,
	})
	snap := b.Snapshot()
	if got := snap.Run.ActiveDurationMs; got != 12000 {
		t.Errorf("post-SetRun ActiveDurationMs = %d, want 12000 preserved", got)
	}
	if snap.Run.CurrentRunStart != nil {
		t.Errorf("CurrentRunStart = %v, want nil preserved (run is paused)", snap.Run.CurrentRunStart)
	}
}

// evtAt builds an event with an explicit timestamp + monotonic ActiveMs
// stamp, so the active-duration tests can decouple wall-clock from the
// engine's monotonic clock (the whole point of BUG A).
func evtAt(seq int64, t store.EventType, node string, tsSec int64, activeMs int64, data map[string]any) *store.Event {
	return &store.Event{
		Seq:       seq,
		Timestamp: time.Unix(tsSec, 0).UTC(),
		Type:      t,
		NodeID:    node,
		ActiveMs:  activeMs,
		Data:      data,
	}
}

// TestSnapshotReducer_ActiveDurationMonotonicExcludesSuspend is the BUG A
// regression: when events carry the engine's monotonic Event.ActiveMs,
// the reducer must report that value and MUST NOT inflate it by a large
// wall-clock timestamp gap between two events (an OS suspend). Here the
// machine "slept" ~6h between two events but the monotonic clock only
// advanced 500ms — the displayed active duration must track the 500ms,
// not the 6h.
func TestSnapshotReducer_ActiveDurationMonotonicExcludesSuspend(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	const t0 = int64(1_000_000)
	events := []*store.Event{
		evtAt(0, store.EventRunStarted, "", t0, 0, nil),
		evtAt(1, store.EventNodeStarted, "work", t0+1, 1000,
			map[string]any{"kind": "agent"}),
		// 6h wall-clock gap (suspend) but only +500ms monotonic active.
		evtAt(2, store.EventLLMRequest, "work", t0+1+6*3600, 1500, nil),
		evtAt(3, store.EventRunFinished, "", t0+2+6*3600, 2000, nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := snap.Run.ActiveDurationMs; got != 2000 {
		t.Fatalf("ActiveDurationMs = %d, want 2000 (monotonic) — a wall-clock derivation would report ~%d", got, 6*3600*1000)
	}
	if snap.Run.CurrentRunStart != nil {
		t.Errorf("CurrentRunStart = %v, want nil after run_finished", snap.Run.CurrentRunStart)
	}
}

// TestSnapshotReducer_ActiveDurationWallClockFallbackLegacy proves the
// pre-fix behaviour is preserved for OLD runs whose events carry no
// ActiveMs (all zero): the reducer still sums the wall-clock event
// windows so historical runs render sensibly.
func TestSnapshotReducer_ActiveDurationWallClockFallbackLegacy(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		evt(1, store.EventNodeStarted, "", "a", map[string]any{"kind": "agent"}),
		evt(5, store.EventRunFinished, "", "", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	// evt() stamps ts = seq seconds; run_started@0 → run_finished@5 = 5s.
	if got := snap.Run.ActiveDurationMs; got != 5000 {
		t.Fatalf("legacy ActiveDurationMs = %d, want 5000 (wall-clock fallback)", got)
	}
}

// TestSnapshotReducer_LoopIndicatorSemanticNotExecCount is the addendum
// regression: the run-level Loops indicator must report the SEMANTIC max
// loop iteration (e.g. 48, matching the runtime's node#N log label), NOT
// the count of node executions — resume re-executes mid-loop iterations,
// so the physical execution count drifts above the true loop counter.
func TestSnapshotReducer_LoopIndicatorSemanticNotExecCount(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	// run_started carries the declared bound (as float64, mimicking the
	// JSON round-trip events undergo on replay).
	b.Apply(&store.Event{
		Seq:  0,
		Type: store.EventRunStarted,
		Data: map[string]any{"loops": map[string]any{"review_loop": float64(50)}},
	})
	seq := int64(1)
	fire := func(iter int) {
		b.Apply(&store.Event{
			Seq:    seq,
			Type:   store.EventNodeStarted,
			NodeID: "reviewer",
			Data: map[string]any{
				"kind":           "judge",
				"iteration":      iter,
				"iteration_path": "review_loop=" + strconv.Itoa(iter),
			},
		})
		seq++
	}
	// Iterations 0..48 (49 distinct), THEN a resume re-runs 45..48 again
	// (4 re-executions). Total node_started events = 53, but the true
	// loop counter only ever reaches 48.
	for i := 0; i <= 48; i++ {
		fire(i)
	}
	for i := 45; i <= 48; i++ {
		fire(i)
	}
	loops := b.Snapshot().Run.Loops
	p, ok := loops["review_loop"]
	if !ok {
		t.Fatalf("Loops missing review_loop; got %+v", loops)
	}
	if p.Current != 48 {
		t.Errorf("Loops[review_loop].Current = %d, want 48 (semantic max, not the 53 node_started events)", p.Current)
	}
	if p.Max != 50 {
		t.Errorf("Loops[review_loop].Max = %d, want 50 (declared bound)", p.Max)
	}
}

// nodeFinishedWithMeta builds a node_finished event whose output carries
// the runtime-stamped _backend / _model observability keys.
func nodeFinishedWithMeta(seq int64, node, backend, model string) *store.Event {
	out := map[string]any{}
	if backend != "" {
		out["_backend"] = backend
	}
	if model != "" {
		out["_model"] = model
	}
	return evt(seq, store.EventNodeFinished, "", node, map[string]any{"output": out})
}

func TestSnapshotReducer_BackendsUsedAggregation(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusFinished})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		// analyze: claw · openai/gpt-5.4-mini
		evt(1, store.EventNodeStarted, "", "analyze", map[string]any{"kind": "agent"}),
		nodeFinishedWithMeta(2, "analyze", "claw", "openai/gpt-5.4-mini"),
		// implement: claude_code · sonnet
		evt(3, store.EventNodeStarted, "", "implement", map[string]any{"kind": "agent"}),
		nodeFinishedWithMeta(4, "implement", "claude_code", "sonnet"),
		// review: claw · openai/gpt-5.4-mini (same pair as analyze → node_count 2)
		evt(5, store.EventNodeStarted, "", "review", map[string]any{"kind": "judge"}),
		nodeFinishedWithMeta(6, "review", "claw", "openai/gpt-5.4-mini"),
		evt(7, store.EventRunFinished, "", "", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	used := b.Snapshot().Run.BackendsUsed
	if len(used) != 2 {
		t.Fatalf("BackendsUsed = %d pairs, want 2: %+v", len(used), used)
	}
	// First-seen order: claw pair first, claude_code second.
	if used[0].Backend != "claw" || used[0].Model != "openai/gpt-5.4-mini" {
		t.Errorf("used[0] = %+v, want claw/openai/gpt-5.4-mini", used[0])
	}
	if used[0].NodeCount != 2 {
		t.Errorf("used[0].NodeCount = %d, want 2 (analyze + review)", used[0].NodeCount)
	}
	if used[1].Backend != "claude_code" || used[1].Model != "sonnet" {
		t.Errorf("used[1] = %+v, want claude_code/sonnet", used[1])
	}
	if used[1].NodeCount != 1 {
		t.Errorf("used[1].NodeCount = %d, want 1", used[1].NodeCount)
	}
}

// A loop re-runs the same node many times against the same pair; the
// distinct-node dedup keeps NodeCount at 1, not the iteration count.
func TestSnapshotReducer_BackendsUsedDedupesLoopIterations(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		evt(1, store.EventNodeStarted, "", "fix", map[string]any{"kind": "agent", "iteration": 0}),
		nodeFinishedWithMeta(2, "fix", "claw", "anthropic/claude-sonnet-4-6"),
		evt(3, store.EventNodeStarted, "", "fix", map[string]any{"kind": "agent", "iteration": 1}),
		nodeFinishedWithMeta(4, "fix", "claw", "anthropic/claude-sonnet-4-6"),
		evt(5, store.EventNodeStarted, "", "fix", map[string]any{"kind": "agent", "iteration": 2}),
		nodeFinishedWithMeta(6, "fix", "claw", "anthropic/claude-sonnet-4-6"),
	}
	for _, e := range events {
		b.Apply(e)
	}
	used := b.Snapshot().Run.BackendsUsed
	if len(used) != 1 {
		t.Fatalf("BackendsUsed = %d, want 1: %+v", len(used), used)
	}
	if used[0].NodeCount != 1 {
		t.Errorf("NodeCount = %d, want 1 (one distinct node despite 3 loop iterations)", used[0].NodeCount)
	}
}

// Tool/compute-only runs stamp no _backend, so BackendsUsed stays nil
// and the studio renders no backend chip.
func TestSnapshotReducer_BackendsUsedEmptyForNonLLMRun(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		evt(1, store.EventNodeStarted, "", "build", map[string]any{"kind": "tool"}),
		// A tool node's output carries no _backend key.
		evt(2, store.EventNodeFinished, "", "build", map[string]any{"output": map[string]any{"text": "ok"}}),
		evt(3, store.EventRunFinished, "", "", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	if used := b.Snapshot().Run.BackendsUsed; used != nil {
		t.Errorf("BackendsUsed = %+v, want nil for a tool-only run", used)
	}
}

// ---------------------------------------------------------------------------
// Deployment report (delivery + traceability output contract)
// ---------------------------------------------------------------------------

// deployFinished builds a node_finished carrying a delivery-group output.
func deployFinished(seq int64, node, url, image string, deployed, healthy bool, notes string) *store.Event {
	return evt(seq, store.EventNodeFinished, "", node, map[string]any{"output": map[string]any{
		"deployed":     deployed,
		"healthy":      healthy,
		"deployed_url": url,
		"image_ref":    image,
		"notes":        notes,
	}})
}

// traceFinished builds a node_finished carrying a traceability-group output.
func traceFinished(seq int64, node string, verifiable, pushed, fromRepo, fromHead bool, commit, log string) *store.Event {
	return evt(seq, store.EventNodeFinished, "", node, map[string]any{"output": map[string]any{
		"verifiable":      verifiable,
		"pushed":          pushed,
		"image_from_repo": fromRepo,
		"built_from_head": fromHead,
		"commit":          commit,
		"trace_log":       log,
	}})
}

// The two groups are recognized independently, so a bot that splits the
// delivery (agent) from the traceability gate (deterministic tool) across
// two nodes still yields ONE report.
func TestSnapshotReducer_DeploymentTraceable(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusFinished})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		evt(1, store.EventNodeStarted, "", "deploy", map[string]any{"kind": "agent"}),
		deployFinished(2, "deploy", "https://app.example.test", "ghcr.io/acme/app:abc1234", true, true, "released r1"),
		evt(3, store.EventNodeStarted, "", "deploy_trace", map[string]any{"kind": "tool"}),
		traceFinished(4, "deploy_trace", true, true, true, true, "abc1234def567", "image=ghcr.io/acme/app:abc1234"),
		evt(5, store.EventRunFinished, "", "", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	d := b.Snapshot().Run.Deployment
	if d == nil {
		t.Fatal("Deployment = nil, want a report")
	}
	if d.URL != "https://app.example.test" || d.ImageRef != "ghcr.io/acme/app:abc1234" {
		t.Errorf("URL/ImageRef = %q/%q", d.URL, d.ImageRef)
	}
	if !d.Deployed || !d.Healthy {
		t.Errorf("Deployed/Healthy = %v/%v, want true/true", d.Deployed, d.Healthy)
	}
	if d.NodeID != "deploy" {
		t.Errorf("NodeID = %q, want deploy", d.NodeID)
	}
	// The gate resolves the commit git actually reports, so it wins over
	// the (here absent) agent-reported one.
	if d.Commit != "abc1234def567" {
		t.Errorf("Commit = %q, want the gate-resolved SHA", d.Commit)
	}
	if d.Trace == nil {
		t.Fatal("Trace = nil, want the traceability verdict attached")
	}
	if !d.Trace.Traceable() {
		t.Errorf("Traceable() = false, want true: %+v", d.Trace)
	}
	if d.Trace.NodeID != "deploy_trace" {
		t.Errorf("Trace.NodeID = %q, want deploy_trace", d.Trace.NodeID)
	}
}

// The ConfigMap-on-a-stock-base-image failure: a live, honest URL with
// nothing pushed and nothing reproducible. The report must carry the URL
// AND the verdict that sinks it.
func TestSnapshotReducer_DeploymentNotTraceable(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		deployFinished(1, "deploy", "https://app.example.test", "node:22-slim", true, true, "served from a ConfigMap"),
		traceFinished(2, "deploy_trace", true, false, false, false, "", "NOT PUSHED | IMAGE NOT FROM THIS REPO"),
	}
	for _, e := range events {
		b.Apply(e)
	}
	d := b.Snapshot().Run.Deployment
	if d == nil || d.Trace == nil {
		t.Fatalf("Deployment/Trace = %+v, want both", d)
	}
	if !d.Trace.Verifiable {
		t.Error("Verifiable = false, want true — the gate DID establish the facts")
	}
	if d.Trace.Traceable() {
		t.Error("Traceable() = true, want false: nothing pushed, stock base image")
	}
	if d.Trace.Log == "" {
		t.Error("Trace.Log is empty — the operator loses the reason")
	}
}

// verifiable:false is an environment fault, not a verdict on the deploy.
// It must stay distinguishable from a verified failure.
func TestSnapshotReducer_DeploymentUnverified(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		deployFinished(1, "deploy", "https://app.example.test", "ghcr.io/acme/app:abc1234", true, true, ""),
		traceFinished(2, "deploy_trace", false, false, false, false, "", "CANNOT VERIFY: git is unavailable in this workspace"),
	}
	for _, e := range events {
		b.Apply(e)
	}
	d := b.Snapshot().Run.Deployment
	if d == nil || d.Trace == nil {
		t.Fatalf("Deployment/Trace = %+v, want both", d)
	}
	if d.Trace.Verifiable {
		t.Error("Verifiable = true, want false")
	}
	if d.Trace.Traceable() {
		t.Error("Traceable() = true, want false — unverifiable is never traceable")
	}
}

// A redeploy loop re-reports both groups; the LAST attempt is the run's
// actual outcome.
func TestSnapshotReducer_DeploymentLastAttemptWins(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		deployFinished(1, "deploy", "", "", false, false, "registry auth failed"),
		traceFinished(2, "deploy_trace", true, false, false, false, "", "NOT PUSHED"),
		deployFinished(3, "deploy", "https://app.example.test", "ghcr.io/acme/app:abc1234", true, true, "released r2"),
		traceFinished(4, "deploy_trace", true, true, true, true, "abc1234def567", "ok"),
	}
	for _, e := range events {
		b.Apply(e)
	}
	d := b.Snapshot().Run.Deployment
	if d == nil || d.URL != "https://app.example.test" {
		t.Fatalf("Deployment = %+v, want the second attempt's URL", d)
	}
	if !d.Trace.Traceable() {
		t.Errorf("Traceable() = false, want the second attempt's verdict: %+v", d.Trace)
	}
}

// A deploy that never came up still reports — "attempted and failed" is
// not the same as "no deployment", and hiding it hides the blocker.
func TestSnapshotReducer_DeploymentFailedStillReported(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	b.Apply(evt(0, store.EventRunStarted, "", "", nil))
	b.Apply(deployFinished(1, "deploy", "", "", false, false, "no deploy-target skill attached"))
	d := b.Snapshot().Run.Deployment
	if d == nil {
		t.Fatal("Deployment = nil, want the failed attempt reported")
	}
	if d.Deployed || d.URL != "" {
		t.Errorf("Deployed/URL = %v/%q, want false/empty", d.Deployed, d.URL)
	}
	if d.Notes == "" {
		t.Error("Notes is empty — the operator loses the blocker")
	}
	if d.Trace != nil {
		t.Errorf("Trace = %+v, want nil (no gate ran)", d.Trace)
	}
}

// The overwhelming majority of runs deploy nothing: no reserved key, no
// report, and the studio renders exactly what it renders today.
func TestSnapshotReducer_DeploymentAbsentForOrdinaryRun(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		evt(1, store.EventNodeFinished, "", "implement", map[string]any{"output": map[string]any{
			"summary": "shipped", "commits_this_pass": 3, "pushed": true,
		}}),
		evt(2, store.EventNodeFinished, "", "verify", map[string]any{"output": map[string]any{
			"passed": true, "verifiable": true,
		}}),
		evt(3, store.EventRunFinished, "", "", nil),
	}
	for _, e := range events {
		b.Apply(e)
	}
	if d := b.Snapshot().Run.Deployment; d != nil {
		t.Errorf("Deployment = %+v, want nil for a run that deployed nothing", d)
	}
}

// SetRun rebuilds the header from run.json (terminal-event refresh); the
// event-derived deployment must survive it, like Loops and BackendsUsed.
func TestSnapshotReducer_DeploymentSurvivesSetRun(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	b.Apply(evt(0, store.EventRunStarted, "", "", nil))
	b.Apply(deployFinished(1, "deploy", "https://app.example.test", "ghcr.io/acme/app:abc1234", true, true, ""))
	b.SetRun(&store.Run{ID: "r1", Status: store.RunStatusFinished})
	if d := b.Snapshot().Run.Deployment; d == nil || d.URL != "https://app.example.test" {
		t.Errorf("Deployment = %+v, want it preserved across SetRun", d)
	}
}

// The two groups may arrive in either order, and the gate's git-resolved
// commit outranks the deploying agent's own claim.
func TestSnapshotReducer_DeploymentTraceBeforeDelivery(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	b.Apply(evt(0, store.EventRunStarted, "", "", nil))
	b.Apply(traceFinished(1, "deploy_trace", true, true, true, true, "abc1234def567", "ok"))
	b.Apply(evt(2, store.EventNodeFinished, "", "deploy", map[string]any{"output": map[string]any{
		"deployed": true, "healthy": true,
		"deployed_url": "https://app.example.test",
		"image_ref":    "ghcr.io/acme/app:abc1234",
		"commit":       "0000000deadbee",
	}}))
	d := b.Snapshot().Run.Deployment
	if d == nil || d.Trace == nil {
		t.Fatalf("Deployment/Trace = %+v, want both regardless of arrival order", d)
	}
	if d.Commit != "abc1234def567" {
		t.Errorf("Commit = %q, want the gate-resolved SHA to outrank the agent's claim", d.Commit)
	}
}

// A traceability verdict with nothing to qualify is dropped: the row
// exists to qualify a URL.
func TestSnapshotReducer_DeploymentTraceAloneIsDropped(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	b.Apply(evt(0, store.EventRunStarted, "", "", nil))
	b.Apply(traceFinished(1, "deploy_trace", true, true, true, true, "abc1234def567", "ok"))
	if d := b.Snapshot().Run.Deployment; d != nil {
		t.Errorf("Deployment = %+v, want nil when no delivery was reported", d)
	}
}

// A node inside a declared loop that falls back on each iteration must
// produce ONE FallbacksUsed entry, not N — otherwise the studio header
// renders N identical chips with colliding React keys (R546ddc).
func TestSnapshotReducer_FallbacksUsedDedupesByNode(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	fallbackOut := map[string]any{
		"output": map[string]any{
			"_backend":       "claw",
			"_model":         "openai/gpt-5.5",
			"_fallback_used": true,
			"_served_by":     "api",
		},
	}
	// Three loop iterations of the same node, each stamped as served by
	// a fallback route — the dominant shape in review/fix loops.
	for i := int64(0); i < 3; i++ {
		b.Apply(evt(i*2, store.EventNodeStarted, "", "review", map[string]any{"kind": "agent"}))
		b.Apply(evt(i*2+1, store.EventNodeFinished, "", "review", fallbackOut))
	}
	// A different node falling back must still get its own entry.
	b.Apply(evt(10, store.EventNodeFinished, "", "implement", map[string]any{
		"output": map[string]any{
			"_backend": "claw", "_model": "openai/gpt-5.5",
			"_fallback_used": true, "_served_by": "run-fallback",
		},
	}))

	got := b.Snapshot().Run.FallbacksUsed
	if len(got) != 2 {
		t.Fatalf("FallbacksUsed = %d entries, want 2 (one per node, not per iteration): %+v", len(got), got)
	}
	if got[0].NodeID != "review" || got[0].ServedBy != "api" {
		t.Errorf("first = %+v, want review/api (first-seen route)", got[0])
	}
	if got[1].NodeID != "implement" || got[1].ServedBy != "run-fallback" {
		t.Errorf("second = %+v, want implement/run-fallback", got[1])
	}
}

func TestSnapshotReducer_RunRewoundResetsDroppedNodes(t *testing.T) {
	// The run_rewound event must erase the execution state of the nodes
	// the rewind invalidated: the event log is append-only, so without a
	// dedicated fold the canvas keeps rendering the dropped nodes with
	// their pre-rewind status / duration / error (pkg/runview/rewind.go
	// only touches the checkpoint).
	b := NewSnapshotBuilder(&store.Run{ID: "r1", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", nil),
		evt(1, store.EventNodeStarted, "", "analyze", map[string]any{"kind": "agent"}),
		evt(2, store.EventNodeFinished, "", "analyze", nil),
		evt(3, store.EventNodeStarted, "", "verify", map[string]any{"kind": "judge"}),
		evt(4, store.EventNodeFinished, "", "verify", nil),
		evt(5, store.EventNodeStarted, "", "report", map[string]any{"kind": "agent"}),
		evt(6, store.EventNodeFinished, "", "report", nil),
		// Rewind to "verify": verify (pivot) and everything downstream
		// of it is invalidated; analyze survives.
		evt(7, store.EventRunRewound, "", "verify", map[string]any{
			"from_node":     "report",
			"to_node":       "verify",
			"dropped_nodes": []string{"report", "verify"},
		}),
	}
	for _, e := range events {
		b.Apply(e)
	}
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 1 {
		t.Fatalf("Executions = %d, want 1 (only the pre-pivot node survives): %+v", got, snap.Executions)
	}
	if snap.Executions[0].IRNodeID != "analyze" || snap.Executions[0].Status != ExecStatusFinished {
		t.Errorf("surviving exec = %+v, want analyze/finished", snap.Executions[0])
	}

	// The post-rewind resume re-executes the pivot: its node_started
	// must recreate a clean running exec — and exactly once in the
	// order slice (no duplicate emission from Snapshot()).
	b.Apply(evt(8, store.EventRunResumed, "", "", nil))
	b.Apply(evt(9, store.EventNodeStarted, "", "verify", map[string]any{"kind": "judge"}))
	snap = b.Snapshot()
	if got := len(snap.Executions); got != 2 {
		t.Fatalf("Executions after resume = %d, want 2: %+v", got, snap.Executions)
	}
	replayed := snap.Executions[1]
	if replayed.IRNodeID != "verify" || replayed.Status != ExecStatusRunning {
		t.Errorf("replayed exec = %+v, want verify/running", replayed)
	}
	if replayed.FinishedAt != nil {
		t.Errorf("replayed exec FinishedAt = %v, want nil (fresh attempt)", replayed.FinishedAt)
	}
	// And the replayed node finishes normally — the rewound exec must
	// not linger anywhere to swallow the node_finished.
	b.Apply(evt(10, store.EventNodeFinished, "", "verify", nil))
	snap = b.Snapshot()
	if snap.Executions[1].Status != ExecStatusFinished {
		t.Errorf("replayed exec Status = %q, want finished", snap.Executions[1].Status)
	}
}

func TestSnapshotReducer_RunRewoundLoopNodeDropsEveryIteration(t *testing.T) {
	// A dropped node that ran N loop iterations produced N execs; the
	// rewind invalidates the NODE, so every one of them goes.
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	for i := int64(0); i < 3; i++ {
		b.Apply(evt(i*2, store.EventNodeStarted, "", "fix", map[string]any{"kind": "agent"}))
		b.Apply(evt(i*2+1, store.EventNodeFinished, "", "fix", nil))
	}
	if got := len(b.Snapshot().Executions); got != 3 {
		t.Fatalf("pre-rewind Executions = %d, want 3", got)
	}
	b.Apply(evt(6, store.EventRunRewound, "", "fix", map[string]any{
		"dropped_nodes": []string{"fix"},
	}))
	if got := len(b.Snapshot().Executions); got != 0 {
		t.Fatalf("post-rewind Executions = %d, want 0: %+v", got, b.Snapshot().Executions)
	}
	// Re-execution numbers iterations from a clean slate (legacy
	// no-`iteration`-field path): nodeCount was reset too.
	b.Apply(evt(7, store.EventNodeStarted, "", "fix", map[string]any{"kind": "agent"}))
	snap := b.Snapshot()
	if got := len(snap.Executions); got != 1 {
		t.Fatalf("post-resume Executions = %d, want 1", got)
	}
	if snap.Executions[0].LoopIteration != 0 {
		t.Errorf("replayed LoopIteration = %d, want 0 (nodeCount reset)", snap.Executions[0].LoopIteration)
	}
}

func TestSnapshotReducer_RunRewoundJSONDecodedPayload(t *testing.T) {
	// After a round-trip through events.jsonl / Mongo, dropped_nodes
	// decodes as []any, not []string — the fold must handle both.
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	b.Apply(evt(0, store.EventNodeStarted, "", "verify", map[string]any{"kind": "judge"}))
	b.Apply(evt(1, store.EventNodeFinished, "", "verify", nil))
	b.Apply(evt(2, store.EventRunRewound, "", "verify", map[string]any{
		"dropped_nodes": []any{"verify"},
	}))
	if got := len(b.Snapshot().Executions); got != 0 {
		t.Fatalf("Executions = %d, want 0 (dropped_nodes decoded as []any)", got)
	}
}

func TestSnapshotReducer_RunRewoundWithoutDroppedNodesIsANoOp(t *testing.T) {
	// Defensive: a malformed or legacy run_rewound payload must not
	// wipe unrelated executions.
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	b.Apply(evt(0, store.EventNodeStarted, "", "analyze", map[string]any{"kind": "agent"}))
	b.Apply(evt(1, store.EventNodeFinished, "", "analyze", nil))
	b.Apply(evt(2, store.EventRunRewound, "", "analyze", nil))
	if got := len(b.Snapshot().Executions); got != 1 {
		t.Fatalf("Executions = %d, want 1 (no dropped_nodes → no-op)", got)
	}
}
