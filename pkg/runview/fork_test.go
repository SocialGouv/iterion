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
	if _, err := st.CreateRun(context.Background(), parentID, "wf", map[string]interface{}{"x": 1}); err != nil {
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
		Outputs: map[string]map[string]interface{}{
			"step1": {"value": "alpha"},
		},
		Vars: map[string]interface{}{"workflow_var": "v"},
	}
	parent.WorkflowHash = "hash-abc"
	parent.Status = store.RunStatusCancelled
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
	parent.Checkpoint = &store.Checkpoint{NodeID: "nodeA", Outputs: map[string]map[string]interface{}{}}
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
