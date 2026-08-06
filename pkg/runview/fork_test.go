package runview

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestFork_HappyPath exercises Service.Fork end-to-end against a
// filesystem-backed run with one captured turn checkpoint. Asserts
// the child run is minted with the expected fork anchor, status,
// and rehydrated backend conversation.
func TestFork_HappyPath(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	parentID := "run-fork-parent"
	if _, err := st.CreateRun(context.Background(), parentID, "wf", map[string]any{"x": 1}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	// Park the parent with a checkpoint shaped like the engine would
	// have left after running a couple of nodes.
	parent, err := st.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	parent.Checkpoint = &store.Checkpoint{
		NodeID: "step2",
		Outputs: map[string]map[string]any{
			"step1": {"value": "alpha"},
		},
		Vars: map[string]any{"workflow_var": "v"},
	}
	parent.WorkflowHash = "hash-abc"
	parent.Status = store.RunStatusCancelled
	// The parent was dispatched from a board issue: the fork must carry
	// the same source edge, or the pipeline card keeps pointing at the
	// dead parent with no way to re-attach the live fork. The schedule
	// fields are deliberately set too — to prove they do NOT travel:
	// ScheduleID feeds the schedgate overlap gate, so inheriting it
	// would wire the recovery fork into the schedule's skip/supersede
	// decisions.
	parent.Source = &store.RunSource{
		Kind:            store.RunSourceKindDispatcher,
		IssueID:         "native:issue-1",
		IssueIdentifier: "issue-1",
		IssueTitle:      "Ship it",
		ScheduleID:      "nightly",
		ScheduleName:    "Nightly",
	}
	if err := st.SaveRun(context.Background(), parent); err != nil {
		t.Fatalf("save parent: %v", err)
	}
	// Write a turn checkpoint that the Fork resolver picks up.
	turnCP := &store.TurnCheckpoint{
		RunID:        parentID,
		NodeID:       "step2",
		LoopIter:     0,
		TurnIndex:    3,
		Backend:      "claw",
		FinishReason: "tool_use",
		MessagesRef:  "step2/0/3.messages.json",
		Messages:     json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`),
		WrittenAt:    time.Now().UTC(),
	}
	if err := st.WriteTurn(context.Background(), turnCP); err != nil {
		t.Fatalf("write turn: %v", err)
	}

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := svc.Fork(context.Background(), ForkSpec{
		RunID:     parentID,
		NodeID:    "step2",
		TurnIndex: 3,
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if result.NewRunID == "" {
		t.Fatal("expected non-empty new_run_id")
	}
	if result.ParentRunID != parentID {
		t.Errorf("parent_run_id = %q, want %q", result.ParentRunID, parentID)
	}
	if result.ForkAnchor == nil || result.ForkAnchor.NodeID != "step2" || result.ForkAnchor.TurnIndex != 3 {
		t.Errorf("fork_anchor = %+v, want node=step2 turn=3", result.ForkAnchor)
	}
	child, err := st.LoadRun(context.Background(), result.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.ForkedFrom != parentID {
		t.Errorf("child.ForkedFrom = %q, want %q", child.ForkedFrom, parentID)
	}
	if child.ParentRunID != parentID {
		t.Errorf("child.ParentRunID = %q, want %q", child.ParentRunID, parentID)
	}
	if child.Source == nil {
		t.Fatal("child.Source = nil, want inherited from the parent so the board card follows the fork")
	}
	if child.Source.IssueID != "native:issue-1" {
		t.Errorf("child.Source = %+v, want issue provenance on native:issue-1", child.Source)
	}
	if child.Source.Kind != "" {
		t.Errorf("child.Source.Kind = %q, want empty — Kind is the parent's trigger classification; the fork's own is \"fork\" via ForkedFrom", child.Source.Kind)
	}
	if child.Source.ScheduleID != "" || child.Source.ScheduleName != "" {
		t.Errorf("child.Source inherited the schedule identity (%q/%q) — the schedgate overlap gate must not see the fork",
			child.Source.ScheduleID, child.Source.ScheduleName)
	}
	// The parent's own record must survive the inheritance untouched.
	// Comparing pointers here could never fail — Fork loads its own copy
	// of the parent from the store — so re-read the persisted parent.
	parentAfter, err := st.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if parentAfter.Source == nil || parentAfter.Source.Kind != store.RunSourceKindDispatcher ||
		parentAfter.Source.IssueID != "native:issue-1" || parentAfter.Source.ScheduleID != "nightly" {
		t.Errorf("parent.Source = %+v, want the parent's own source left intact", parentAfter.Source)
	}
	children, err := svc.ListChildren(context.Background(), parentID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 1 || children[0].ID != child.ID {
		t.Fatalf("ListChildren(%q) = %+v, want child %q", parentID, children, child.ID)
	}
	if child.SourceHash != "hash-abc" {
		t.Errorf("child.SourceHash = %q, want hash-abc", child.SourceHash)
	}
	if child.Status != store.RunStatusCancelled {
		t.Errorf("child.Status = %q, want cancelled (ready for Resume)", child.Status)
	}
	if child.Checkpoint == nil {
		t.Fatal("expected non-nil checkpoint on child")
	}
	if child.Checkpoint.NodeID != "step2" {
		t.Errorf("child checkpoint NodeID = %q, want step2", child.Checkpoint.NodeID)
	}
	if len(child.Checkpoint.BackendConversation) == 0 {
		t.Error("expected child.Checkpoint.BackendConversation populated from turn messages")
	}
	// step2's stale output (parent had none here) should be absent so
	// re-execution starts fresh.
	if _, ok := child.Checkpoint.Outputs["step2"]; ok {
		t.Error("expected child checkpoint Outputs to not carry the anchor node's stale output")
	}
	// step1's upstream output is preserved.
	if v := child.Checkpoint.Outputs["step1"]["value"]; v != "alpha" {
		t.Errorf("child upstream output step1.value = %v, want alpha", v)
	}
}

// TestFork_LatestTurn confirms that passing turn_index=-1 picks the
// most-recent turn captured for the node.
func TestFork_LatestTurn(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	parentID := "run-fork-latest"
	if _, err := st.CreateRun(context.Background(), parentID, "wf", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	parent, _ := st.LoadRun(context.Background(), parentID)
	parent.Checkpoint = &store.Checkpoint{NodeID: "nodeA", Outputs: map[string]map[string]any{}}
	parent.Status = store.RunStatusCancelled
	if err := st.SaveRun(context.Background(), parent); err != nil {
		t.Fatalf("save: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		t.Helper()
		if err := st.WriteTurn(context.Background(), &store.TurnCheckpoint{
			RunID:     parentID,
			NodeID:    "nodeA",
			LoopIter:  0,
			TurnIndex: i,
			Backend:   "claw",
			WrittenAt: now.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("write turn %d: %v", i, err)
		}
	}
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := svc.Fork(context.Background(), ForkSpec{
		RunID:     parentID,
		NodeID:    "nodeA",
		TurnIndex: -1,
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if result.ForkAnchor.TurnIndex != 2 {
		t.Errorf("latest turn anchor = %d, want 2", result.ForkAnchor.TurnIndex)
	}
}

// TestFork_IssueLessParentMintsNoSource: a parent whose Source carries no
// IssueID (e.g. schedule-launched: Kind + ScheduleID only) gives the fork
// NO Source at all. Inheriting a Kind-only shell would relabel the fork
// as a scheduled run — provenance it does not have — and buy nothing for
// the board fix, which keys on IssueID alone.
func TestFork_IssueLessParentMintsNoSource(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	parentID := "run-fork-sched"
	if _, err := st.CreateRun(context.Background(), parentID, "wf", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	parent, _ := st.LoadRun(context.Background(), parentID)
	parent.Checkpoint = &store.Checkpoint{NodeID: "nodeA", Outputs: map[string]map[string]any{}}
	parent.Status = store.RunStatusCancelled
	parent.Source = &store.RunSource{
		Kind:         store.RunSourceKindSchedule,
		ScheduleID:   "nightly",
		ScheduleName: "Nightly",
	}
	if err := st.SaveRun(context.Background(), parent); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.WriteTurn(context.Background(), &store.TurnCheckpoint{
		RunID:     parentID,
		NodeID:    "nodeA",
		Backend:   "claw",
		WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write turn: %v", err)
	}
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := svc.Fork(context.Background(), ForkSpec{
		RunID:     parentID,
		NodeID:    "nodeA",
		TurnIndex: -1,
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child, err := st.LoadRun(context.Background(), result.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.Source != nil {
		t.Errorf("child.Source = %+v, want nil for an issue-less parent (no phantom schedule provenance)", child.Source)
	}
}
