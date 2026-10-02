package model

import (
	"context"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A run forked from a claude_code turn resumes that turn's session: the work
// that died with the turn's process must reach the turn checkpoint the Fork
// API reads, through the delegate hook, the capture info and the store hook.
func TestATurnCheckpointCarriesTheWorkThatDiedWithItsProcess(t *testing.T) {
	dead := []string{"auditor (local_agent, t1)"}
	const runID = "run-turn-dead-work"
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	e := &ClawExecutor{hooks: NewStoreEventHooks(ctx, st, runID, iterlog.Nop(), nil)}
	e.delegateHooksFor("worker", delegate.BackendClaudeCode, 0).OnTurnFinished(delegate.TurnFinishedInfo{
		SessionID: "sess-1", Text: "waiting", TerminatedBackgroundTasks: dead,
	})
	turn, err := store.AsTurnStore(st).LatestTurn(ctx, runID, "worker")
	if err != nil {
		t.Fatalf("LatestTurn: %v", err)
	}
	if turn.SessionID != "sess-1" || !reflect.DeepEqual(turn.TerminatedBackground, dead) {
		t.Fatalf("turn = session %q, terminated %v; want sess-1 and %v", turn.SessionID, turn.TerminatedBackground, dead)
	}
}
