package dispatcher

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gateChildBot parks on a human gate the test deliberately never answers —
// the legitimate weekend-long review the stall watchdog must not mistake for
// a wedged engine. Tool-only otherwise, so the fixture needs no credential.
const gateChildBot = `
schema gate_out:
  approved: bool
  notes: string

schema child_out:
  verdict: string

human review:
  output: gate_out

compute wrap:
  output: child_out
  expr:
    verdict: "outputs.review.notes"

workflow gate_child:
  entry: review
  review -> wrap
  wrap -> done
`

const gateParentBot = `
schema pout:
  ready: bool

schema child_out:
  verdict: string

tool prep:
  command: ` + "`printf '{\"ready\":true}'`" + `
  output: pout

subbot run_child:
  source: "gate_child.bot"
  output: child_out

compute summarize:
  output: child_out
  expr:
    verdict: "outputs.run_child.verdict"

workflow gate_parent:
  entry: prep
  prep      -> run_child
  run_child -> summarize
  summarize -> done
`

// newStallTestDispatcher builds a dispatcher bound to storeDir, so
// reconcileStalled can consult the run records the engine wrote.
func newStallTestDispatcher(t *testing.T, storeDir string) *Dispatcher {
	t.Helper()
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	cfg := &Config{
		Name:      "test",
		Workflow:  filepath.Join(dir, "fake.bot"),
		Tracker:   TrackerConfig{Kind: "fake"},
		Polling:   PollingConfig{IntervalMS: 50},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000},
		Workspace: WorkspaceConfig{Root: wsDir},
		// A deliberately short stall window: the whole point is that a park
		// outlives it without being reaped.
		Stall: StallConfig{TimeoutMS: 50},
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(wsDir)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config:     cfg,
		Tracker:    newFakeTracker(),
		Runner:     &StubRunner{},
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
		StoreDir:   storeDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// seedStalledEntry registers runID as an in-flight dispatch whose event
// watermark is already older than the stall timeout, and returns a pointer
// that reports whether reconcileStalled cancelled it.
func seedStalledEntry(c *Dispatcher, issueID, runID string) *bool {
	cancelled := false
	c.state.running[issueID] = &runningEntry{
		IssueID:    issueID,
		Identifier: issueID,
		RunID:      runID,
		StartedAt:  time.Now().Add(-time.Hour),
		// Silent for an hour under a 50ms stall timeout.
		LastEventAt: time.Now().Add(-time.Hour),
		Cancel:      func(error) { cancelled = true },
	}
	return &cancelled
}

// writeRunDoc plants a run record with the given status and subbot children.
// The doc shape mirrors what the engine and runview.RecordSubbotChild write.
func writeRunDoc(t *testing.T, storeDir, runID string, status store.RunStatus, children map[string]string) {
	t.Helper()
	s, err := store.New(storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := s.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun(%s): %v", runID, err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun(%s): %v", runID, err)
	}
	r.Status = status
	if err := s.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("SaveRun(%s): %v", runID, err)
	}
	for key, child := range children {
		if err := s.SetSubbotChild(context.Background(), runID, key, child); err != nil {
			t.Fatalf("SetSubbotChild(%s→%s): %v", runID, child, err)
		}
	}
}

// TestReconcileStalled_ExemptsParentParkedOnPausedSubbotChild is the #558
// regression, driven end to end: the REAL dispatcher engine runs a parent
// whose subbot child parks on a human gate nobody answers, and the REAL stall
// watchdog then judges the parent. A parent blocked in AwaitSubbotTerminal
// emits no event, so its watermark ages past any stall timeout while nothing
// is wrong — and cancelling it burns a dispatcher attempt on a review the
// operator has simply not come back to yet.
func TestReconcileStalled_ExemptsParentParkedOnPausedSubbotChild(t *testing.T) {
	botDir := t.TempDir()
	parentPath := filepath.Join(botDir, "gate_parent.bot")
	if err := os.WriteFile(parentPath, []byte(gateParentBot), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "gate_child.bot"), []byte(gateChildBot), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}

	storeDir := t.TempDir()
	workspace := t.TempDir()
	runner, err := NewEngineRunner(parentPath, iterlog.Nop())
	if err != nil {
		t.Fatalf("NewEngineRunner: %v", err)
	}
	defer func() { _ = runner.Close() }()

	parentID, err := store.GenerateRunID()
	if err != nil {
		t.Fatalf("GenerateRunID: %v", err)
	}

	// The dispatch parks forever (nobody answers the child's gate); cancelling
	// its context at cleanup is what drains the goroutine.
	runCtx, cancelRun := context.WithCancel(context.Background())
	dispatchDone := make(chan error, 1)
	go func() {
		dispatchDone <- runner.Dispatch(runCtx, DispatchSpec{
			RunID:         parentID,
			WorkspacePath: workspace,
			StoreDir:      storeDir,
			Issue:         &IssueRef{ID: "native:" + parentID, Identifier: parentID, Title: "parked on a subbot gate"},
		})
	}()
	t.Cleanup(func() {
		cancelRun()
		select {
		case <-dispatchDone:
		case <-time.After(30 * time.Second):
			t.Error("dispatch goroutine did not drain after cancel")
		}
	})

	probe, err := store.New(storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	childID := waitForPausedSubbotChild(t, probe, parentID, dispatchDone)

	// The parent is genuinely parked: still `running`, and its child sits on
	// the human gate. This is the exact production shape of the ticket.
	parent, err := probe.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("LoadRun(parent): %v", err)
	}
	if parent.Status != store.RunStatusRunning {
		t.Fatalf("parent status = %q, want running (a parked parent stays running)", parent.Status)
	}

	c := newStallTestDispatcher(t, storeDir)
	cancelled := seedStalledEntry(c, "fake:parked", parentID)
	c.reconcileStalled(context.Background(), c.cfg.Load())

	if *cancelled {
		t.Fatalf("parent run %s was stall-reaped while its subbot child %s legitimately awaits human input", parentID, childID)
	}
	if entry := c.state.running["fake:parked"]; entry == nil || !entry.CancelIssuedAt.IsZero() {
		t.Fatalf("stall watchdog issued a cancel against a parked parent (entry=%+v)", entry)
	}

	// The parent kept executing (it is still parked, not torn down) and the
	// park produced no sibling child — the "no second child is created" half
	// of the acceptance list. A reap here would have failed the parent, its
	// retry would have re-run the subbot node, and the operator's pending
	// review would have been orphaned on a child nothing consumes.
	after, err := probe.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("LoadRun(parent) after the sweep: %v", err)
	}
	if after.Status != store.RunStatusRunning {
		t.Fatalf("parent status after the stall sweep = %q, want running", after.Status)
	}
	children := childRunsOf(t, probe, parentID)
	if len(children) != 1 || children[0] != childID {
		t.Fatalf("children of %s = %v, want exactly [%s] — a second child was spawned", parentID, children, childID)
	}
}

// childRunsOf lists every run in the store whose ParentRunID is parentID.
func childRunsOf(t *testing.T, probe *store.FilesystemRunStore, parentID string) []string {
	t.Helper()
	ids, err := probe.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var children []string
	for _, id := range ids {
		if id == parentID {
			continue
		}
		r, lerr := probe.LoadRun(context.Background(), id)
		if lerr == nil && r.ParentRunID == parentID {
			children = append(children, id)
		}
	}
	return children
}

// waitForPausedSubbotChild blocks until the parent has recorded a subbot child
// that is paused on its human gate, and returns that child's run id.
func waitForPausedSubbotChild(t *testing.T, probe *store.FilesystemRunStore, parentID string, dispatchDone <-chan error) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-dispatchDone:
			t.Fatalf("dispatch returned before the child parked on its gate: %v", err)
		default:
		}
		parent, err := probe.LoadRun(context.Background(), parentID)
		if err == nil {
			for _, childID := range parent.SubbotChildren {
				child, cerr := probe.LoadRun(context.Background(), childID)
				if cerr == nil && child.Status == store.RunStatusPausedWaitingHuman {
					return childID
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no subbot child of %s reached paused_waiting_human", parentID)
	return ""
}

// TestReconcileStalled_ReapsGenuinelySilentRun is the guard that keeps the
// exemption from disarming the watchdog: a run with no paused descendant is
// still cancelled the moment its watermark ages out. Without this assertion a
// green suite would prove nothing — an exemption that always fires is not a
// fix, it is the removal of stall detection.
func TestReconcileStalled_ReapsGenuinelySilentRun(t *testing.T) {
	cases := []struct {
		name     string
		children map[string]string
		// childStatus is the status given to every child in children.
		childStatus store.RunStatus
	}{
		{name: "no subbot children at all", children: nil},
		{
			name:        "child running — a wedged descendant is not a parked one",
			children:    map[string]string{"run_child#0": "child-running"},
			childStatus: store.RunStatusRunning,
		},
		{
			name:        "child finished — the park is over, silence is now a wedge",
			children:    map[string]string{"run_child#0": "child-finished"},
			childStatus: store.RunStatusFinished,
		},
		{
			name:        "child failed — the parent should surface the failure, not sit",
			children:    map[string]string{"run_child#0": "child-failed"},
			childStatus: store.RunStatusFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storeDir := t.TempDir()
			writeRunDoc(t, storeDir, "parent-silent", store.RunStatusRunning, tc.children)
			for _, childID := range tc.children {
				writeRunDoc(t, storeDir, childID, tc.childStatus, nil)
			}

			c := newStallTestDispatcher(t, storeDir)
			cancelled := seedStalledEntry(c, "fake:silent", "parent-silent")
			c.reconcileStalled(context.Background(), c.cfg.Load())

			if !*cancelled {
				t.Fatal("a genuinely silent run was NOT stall-reaped — the watchdog is disarmed")
			}
		})
	}
}

// TestReconcileStalled_ExemptsParentParkedOnPausedGrandchild covers the nested
// case: the parent parks on a child that is itself parked on ITS child's gate.
// Only the deepest run carries the paused status, so an oracle that looks one
// level down would reap the whole chain.
func TestReconcileStalled_ExemptsParentParkedOnPausedGrandchild(t *testing.T) {
	storeDir := t.TempDir()
	writeRunDoc(t, storeDir, "gp", store.RunStatusRunning, map[string]string{"a#0": "parent"})
	writeRunDoc(t, storeDir, "parent", store.RunStatusRunning, map[string]string{"b#0": "child"})
	writeRunDoc(t, storeDir, "child", store.RunStatusPausedWaitingHuman, nil)

	c := newStallTestDispatcher(t, storeDir)
	cancelled := seedStalledEntry(c, "fake:nested", "gp")
	c.reconcileStalled(context.Background(), c.cfg.Load())

	if *cancelled {
		t.Fatal("a root parked two levels above a paused grandchild was stall-reaped")
	}
}

// TestReconcileStalled_ExemptsParentParkedOnOperatorPausedChild pins the other
// pause status a child can carry: an operator pause from the run console is
// just as legitimate a park as a human gate.
func TestReconcileStalled_ExemptsParentParkedOnOperatorPausedChild(t *testing.T) {
	storeDir := t.TempDir()
	writeRunDoc(t, storeDir, "parent-op", store.RunStatusRunning, map[string]string{"c#0": "child-op"})
	writeRunDoc(t, storeDir, "child-op", store.RunStatusPausedOperator, nil)

	c := newStallTestDispatcher(t, storeDir)
	cancelled := seedStalledEntry(c, "fake:oppaused", "parent-op")
	c.reconcileStalled(context.Background(), c.cfg.Load())

	if *cancelled {
		t.Fatal("a parent parked on an operator-paused child was stall-reaped")
	}
}

// TestReconcileStalled_ReapsWhenDescendantCycles proves the descendant walk
// terminates on a corrupt store rather than spinning: a child recorded as its
// own parent must not hang the actor goroutine, and must not exempt the run.
func TestReconcileStalled_ReapsWhenDescendantCycles(t *testing.T) {
	storeDir := t.TempDir()
	writeRunDoc(t, storeDir, "cyc-a", store.RunStatusRunning, map[string]string{"x#0": "cyc-b"})
	writeRunDoc(t, storeDir, "cyc-b", store.RunStatusRunning, map[string]string{"y#0": "cyc-a"})

	c := newStallTestDispatcher(t, storeDir)
	cancelled := seedStalledEntry(c, "fake:cycle", "cyc-a")

	done := make(chan struct{})
	go func() {
		c.reconcileStalled(context.Background(), c.cfg.Load())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reconcileStalled did not terminate on a cyclic subbot-children graph")
	}
	if !*cancelled {
		t.Fatal("a cycle with no paused run anywhere must not exempt the run")
	}
}

// TestReconcileStalled_ReapsWhenRunRecordUnreadable fails CLOSED: a store the
// dispatcher cannot read must not silently become a blanket exemption. "No
// information" is not "legitimately parked".
func TestReconcileStalled_ReapsWhenRunRecordUnreadable(t *testing.T) {
	storeDir := t.TempDir()
	c := newStallTestDispatcher(t, storeDir)
	// No run record at all for this id — the pruned/never-written case.
	cancelled := seedStalledEntry(c, "fake:missing", "no-such-run")
	c.reconcileStalled(context.Background(), c.cfg.Load())

	if !*cancelled {
		t.Fatal("an unreadable/absent run record must not exempt a silent run from the stall watchdog")
	}
}

// TestReconcileStalled_StillReapsParkedRunAfterGrace pins the second half of
// the watchdog on the exempt path: a parked run is never cancelled at all, so
// it never enters the force-reap ladder either. Guards against a fix that
// merely delays the cancel.
func TestReconcileStalled_StillReapsParkedRunAfterGrace(t *testing.T) {
	t.Setenv("ITERION_DISPATCHER_STALL_REAP_GRACE", "1ms")
	storeDir := t.TempDir()
	writeRunDoc(t, storeDir, "parent-parked", store.RunStatusRunning, map[string]string{"d#0": "child-parked"})
	writeRunDoc(t, storeDir, "child-parked", store.RunStatusPausedWaitingHuman, nil)

	c := newStallTestDispatcher(t, storeDir)
	cancelled := seedStalledEntry(c, "fake:stillparked", "parent-parked")
	// Several ticks: a parked run must survive all of them.
	for i := 0; i < 5; i++ {
		c.reconcileStalled(context.Background(), c.cfg.Load())
	}
	if *cancelled {
		t.Fatal("a parked parent was reaped on a later tick")
	}
	if _, still := c.state.running["fake:stillparked"]; !still {
		t.Fatal("a parked parent lost its running entry — the slot was force-reaped")
	}
}
