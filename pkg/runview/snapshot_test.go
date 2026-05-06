package runview

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// evt is a tiny helper to build a store.Event with a fixed timestamp
// so tests don't have to thread time through every line.
func evt(seq int64, t store.EventType, branch, node string, data map[string]interface{}) *store.Event {
	return &store.Event{
		Seq:       seq,
		Timestamp: time.Unix(int64(seq), 0).UTC(),
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
		evt(1, store.EventNodeStarted, "", "analyze", map[string]interface{}{"kind": "agent"}),
		evt(2, store.EventNodeFinished, "", "analyze", nil),
		evt(3, store.EventNodeStarted, "", "verify", map[string]interface{}{"kind": "judge"}),
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
		evt(0, store.EventNodeStarted, "", "fix", map[string]interface{}{"kind": "agent"}),
		evt(1, store.EventNodeFinished, "", "fix", nil),
		evt(2, store.EventEdgeSelected, "", "", map[string]interface{}{"loop": "until_green", "iteration": 1}),
		evt(3, store.EventNodeStarted, "", "fix", map[string]interface{}{"kind": "agent"}),
		evt(4, store.EventNodeFinished, "", "fix", nil),
		evt(5, store.EventEdgeSelected, "", "", map[string]interface{}{"loop": "until_green", "iteration": 2}),
		evt(6, store.EventNodeStarted, "", "fix", map[string]interface{}{"kind": "agent"}),
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

func TestSnapshotReducer_FanOutBranches(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	// Fan-out: same node ID runs in two different branches in parallel.
	events := []*store.Event{
		evt(0, store.EventBranchStarted, "br_a", "", nil),
		evt(1, store.EventBranchStarted, "br_b", "", nil),
		evt(2, store.EventNodeStarted, "br_a", "review", map[string]interface{}{"kind": "judge"}),
		evt(3, store.EventNodeStarted, "br_b", "review", map[string]interface{}{"kind": "judge"}),
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
		evt(0, store.EventNodeStarted, "", "ask", map[string]interface{}{"kind": "human"}),
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
		evt(0, store.EventNodeStarted, "", "build", map[string]interface{}{"kind": "tool"}),
		evt(1, store.EventRunFailed, "", "build", map[string]interface{}{"error": "exit 1"}),
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

func TestSnapshotReducer_RunningNodeTouchesLastSeqForStructuredEvents(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r1"})
	events := []*store.Event{
		evt(0, store.EventNodeStarted, "", "agent", map[string]interface{}{"kind": "agent"}),
		evt(1, store.EventLLMPrompt, "", "agent", map[string]interface{}{"user_message": "hello"}),
		evt(2, store.EventLLMRequest, "", "agent", map[string]interface{}{"model": "m"}),
		evt(3, store.EventToolCalled, "", "agent", map[string]interface{}{"tool_name": "Read"}),
		evt(4, store.EventBudgetWarning, "", "agent", map[string]interface{}{"message": "near limit"}),
		// A node-scoped event for another branch must not advance the main
		// branch execution window.
		evt(5, store.EventToolCalled, "other", "agent", map[string]interface{}{"tool_name": "Write"}),
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
		evt(1, store.EventArtifactWritten, "", "build", map[string]interface{}{"version": 0}),
		evt(2, store.EventNodeFinished, "", "build", nil),
		evt(3, store.EventNodeStarted, "", "build", nil),
		evt(4, store.EventArtifactWritten, "", "build", map[string]interface{}{"version": 1}),
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
				evt(5, store.EventRunFailed, "", "", map[string]interface{}{"error": "boom"}),
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
