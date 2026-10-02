package runview

import (
	"context"
	"reflect"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestAForkedRunIsToldWhatDiedWithItsTurnsProcess(t *testing.T) {
	parent := &store.Checkpoint{SessionLedger: []store.SessionLedgerEntry{
		{SessionID: "other", Tasks: []string{"x (local_agent, t0)"}},
		{SessionID: "sess-1", Tasks: []string{"later (local_agent, t9)"}},
	}}
	turn := &store.TurnCheckpoint{SessionID: "sess-1", TerminatedBackground: []string{"auditor (local_agent, t1)"}}
	got := forkSessionLedger(parent, turn)
	want := []store.SessionLedgerEntry{
		{SessionID: "other", Tasks: []string{"x (local_agent, t0)"}},
		{SessionID: "sess-1", Tasks: []string{"later (local_agent, t9)", "auditor (local_agent, t1)"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child ledger = %+v, want %+v — the parent's entries and the turn's own dead work", got, want)
	}
	if got := forkSessionLedger(nil, &store.TurnCheckpoint{SessionID: "s"}); got != nil {
		t.Fatalf("child ledger = %+v, want none", got)
	}
}

// Through the Fork API, not the helper alone: the child checkpoint is what
// the child run's engine restores its ledger from.
func TestFork_ChildRunIsToldWhatDiedWithTheForkedTurn(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const parentID = "run-fork-ledger"
	if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	parent, err := st.LoadRun(ctx, parentID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	parent.Checkpoint = &store.Checkpoint{
		NodeID:        "step2",
		Outputs:       map[string]map[string]any{"step1": {"value": "alpha"}},
		SessionLedger: []store.SessionLedgerEntry{{SessionID: "other", Tasks: []string{"x (local_agent, t0)"}}},
	}
	parent.Status = store.RunStatusCancelled
	if err := st.SaveRun(ctx, parent); err != nil {
		t.Fatalf("save parent: %v", err)
	}
	if err := st.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: parentID, NodeID: "step2", TurnIndex: 0, Backend: "claude_code", SessionID: "sess-1",
		TerminatedBackground: []string{"auditor (local_agent, t1)"}, WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write turn: %v", err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Fork(ctx, ForkSpec{RunID: parentID, NodeID: "step2", TurnIndex: 0})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child, err := st.LoadRun(ctx, res.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	want := []store.SessionLedgerEntry{
		{SessionID: "other", Tasks: []string{"x (local_agent, t0)"}},
		{SessionID: "sess-1", Tasks: []string{"auditor (local_agent, t1)"}},
	}
	if child.Checkpoint == nil || !reflect.DeepEqual(child.Checkpoint.SessionLedger, want) {
		t.Fatalf("child checkpoint ledger = %+v, want %+v", child.Checkpoint, want)
	}
}

// A label the parent's ledger already holds for the forked session is told
// once: the turn that lost it and the ledger that recorded it name the same
// task.
func TestForkSessionLedgerTellsASharedLabelOnce(t *testing.T) {
	cp := &store.Checkpoint{SessionLedger: []store.SessionLedgerEntry{
		{SessionID: "s2", Tasks: []string{"other (local_agent, t7)"}},
		{SessionID: "s1", Tasks: []string{"auditor (local_agent, t1)"}},
	}}
	turn := &store.TurnCheckpoint{SessionID: "s1", TerminatedBackground: []string{"auditor (local_agent, t1)", "checker (local_agent, t2)"}}
	got := forkSessionLedger(cp, turn)
	want := []store.SessionLedgerEntry{
		{SessionID: "s1", Tasks: []string{"auditor (local_agent, t1)", "checker (local_agent, t2)"}},
		{SessionID: "s2", Tasks: []string{"other (local_agent, t7)"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fork ledger = %+v, want %+v", got, want)
	}
}
