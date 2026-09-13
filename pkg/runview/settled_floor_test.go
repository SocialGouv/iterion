package runview

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Two ways for a run to re-execute a node — rewind and fork — both rebuild
// its input from the incoming state the checkpoint carries: the edges routing
// selected, and the floor a stabilized fan-out left behind. Both used to lose
// it, in opposite directions.

// applyRewind must keep the PIVOT's incoming state (the re-execution rebuilds
// from it) and clear every INVALIDATED node below, including the ones that
// never produced an output — those are absent from `dropped`, which is
// filtered on output presence and always contains the pivot.
func TestApplyRewind_KeepsThePivotAndClearsEveryInvalidatedNode(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID: "collect",
		SelectedIncoming: map[string][]store.IncomingEdge{
			"collect":     {{From: "work", To: "collect"}},
			"downstream":  {{From: "collect", To: "downstream"}},
			"never_ran":   {{From: "downstream", To: "never_ran"}},
			"upstream":    {{From: "entry", To: "upstream"}},
			"unaffected2": {{From: "entry", To: "unaffected2"}},
		},
		SettledIncoming: map[string][]store.IncomingEdge{
			"collect":   {{From: "dead", To: "collect"}},
			"never_ran": {{From: "dead2", To: "never_ran"}},
		},
		ArtifactsKnown: true, ArtifactRevisionsKnown: true,
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{}, Edges: []*ir.Edge{}}

	// `never_ran` produced no output, so it is invalidated but NOT dropped.
	applyRewind(cp, wf, "collect",
		[]string{"collect", "downstream"},
		[]string{"collect", "downstream", "never_ran"})

	if len(cp.SelectedIncoming["collect"]) != 1 || len(cp.SettledIncoming["collect"]) != 1 {
		t.Fatalf("the pivot lost its incoming state: selected=%+v settled=%+v — the re-execution rebuilds its input from exactly this",
			cp.SelectedIncoming["collect"], cp.SettledIncoming["collect"])
	}
	for _, id := range []string{"downstream", "never_ran"} {
		if _, present := cp.SelectedIncoming[id]; present {
			t.Errorf("invalidated node %q kept its selection: a node the run never reached is absent from `dropped`, which is why cleanup reads `invalidated`", id)
		}
		if _, present := cp.SettledIncoming[id]; present {
			t.Errorf("invalidated node %q kept its settled floor", id)
		}
	}
	if len(cp.SelectedIncoming["upstream"]) != 1 || len(cp.SelectedIncoming["unaffected2"]) != 1 {
		t.Fatalf("a node outside the rewind lost its selection: %+v", cp.SelectedIncoming)
	}
}

// A fork re-executes its anchor first, so the anchor needs the same incoming
// state an ordinary resume would give it. The synthetic checkpoint used to
// carry outputs, vars and artifact state and neither incoming map, so a
// forked collector lost mappings a plain resume keeps.
func TestFork_CarriesTheAnchorsIncomingState(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	ctx := context.Background()
	const parentID = "run-fork-incoming"
	if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	parent, err := st.LoadRun(ctx, parentID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	parent.Checkpoint = &store.Checkpoint{
		NodeID:                 "collect",
		Outputs:                map[string]map[string]any{"entry": {"count": 2}},
		Vars:                   map[string]any{},
		ArtifactsKnown:         true,
		ArtifactRevisionsKnown: true,
		SelectedIncoming: map[string][]store.IncomingEdge{
			"collect":   {{From: "work", To: "collect"}},
			"elsewhere": {{From: "x", To: "elsewhere"}},
		},
		SettledIncoming: map[string][]store.IncomingEdge{
			"collect":   {{From: "dead", To: "collect"}},
			"elsewhere": {{From: "y", To: "elsewhere"}},
		},
	}
	parent.Status = store.RunStatusCancelled
	if err := st.SaveRun(ctx, parent); err != nil {
		t.Fatalf("save parent: %v", err)
	}
	if err := st.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: parentID, NodeID: "collect", LoopIter: 0, TurnIndex: 0,
		Backend: "claw", FinishReason: "tool_use", WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write turn: %v", err)
	}

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := svc.Fork(ctx, ForkSpec{RunID: parentID, NodeID: "collect", TurnIndex: 0})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child, err := st.LoadRun(ctx, result.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.Checkpoint == nil {
		t.Fatal("the child carries no checkpoint")
	}
	if len(child.Checkpoint.SelectedIncoming["collect"]) != 1 || child.Checkpoint.SelectedIncoming["collect"][0].From != "work" {
		t.Fatalf("child selected incoming = %+v — the anchor re-executes first and rebuilds its input from this", child.Checkpoint.SelectedIncoming)
	}
	if len(child.Checkpoint.SettledIncoming["collect"]) != 1 || child.Checkpoint.SettledIncoming["collect"][0].From != "dead" {
		t.Fatalf("child settled floor = %+v — a forked collector must keep what a plain resume keeps", child.Checkpoint.SettledIncoming)
	}
	if _, present := child.Checkpoint.SelectedIncoming["elsewhere"]; present {
		t.Fatalf("child carried a node other than the anchor: %+v — nothing below the anchor re-executes, so nothing below it should travel", child.Checkpoint.SelectedIncoming)
	}
	if _, present := child.Checkpoint.SettledIncoming["elsewhere"]; present {
		t.Fatalf("child carried a floor other than the anchor's: %+v", child.Checkpoint.SettledIncoming)
	}
}
