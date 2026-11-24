package runview

// The fork's stranding propagation stays scoped to the pause's own
// anchor (#2461): the stranded conversation is the pause node's LAST
// turns, so the child inherits BackendSessionStranded only when it
// re-executes exactly that turn. An anchor elsewhere — another node, or
// an earlier turn of the pause node — pins a conversation that joined
// the session normally; stamping it stranded would make
// OnSessionCapture silently refuse the child's legitimate captures, and
// the child's session slot never accumulates.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestFork_StrandingStaysScopedToThePauseNode(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	parentID := "run-fork-strand-scope"
	if _, err := st.CreateRun(context.Background(), parentID, "wf", map[string]any{"x": 1}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	parent, err := st.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	parent.Status = store.RunStatusPausedWaitingHuman
	parent.Checkpoint = &store.Checkpoint{
		// The pause — and its stranding — belong to otherpause.
		NodeID:      "otherpause",
		BackendName: "claw",
		BackendConversation: json.RawMessage(
			`[{"role":"user","content":[{"type":"text","text":"blind-turn"}]}]`),
		BackendPendingToolUseID: "toolu_1",
		BackendSessionStranded:  true,
	}
	if err := st.SaveRun(context.Background(), parent); err != nil {
		t.Fatalf("save parent: %v", err)
	}
	// A normal completed claw turn at step2 — NOT the stranded crossing.
	if err := st.WriteTurn(context.Background(), &store.TurnCheckpoint{
		RunID:        parentID,
		NodeID:       "step2",
		LoopIter:     0,
		TurnIndex:    3,
		Backend:      "claw",
		FinishReason: "tool_use",
		MessagesRef:  "step2/0/3.messages.json",
		Messages:     json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`),
		WrittenAt:    time.Now().UTC(),
	}); err != nil {
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
	child, err := st.LoadRun(context.Background(), result.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	// The guard's premise: the cross-node fork still pins the anchor
	// turn's conversation for rehydration.
	if len(child.Checkpoint.BackendConversation) == 0 {
		t.Fatal("expected the anchor turn's conversation pinned on the child")
	}
	if child.Checkpoint.BackendSessionStranded {
		t.Errorf("cross-node fork stamped BackendSessionStranded: the child's step2 conversation joined the session normally (the stranded pause lives at otherpause) — OnSessionCapture would silently refuse the child's captures and the session slot would never accumulate")
	}
}

func TestFork_StrandingTravelsOnlyWithThePauseNodesLatestTurn(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	parentID := "run-fork-strand-latest"
	if _, err := st.CreateRun(context.Background(), parentID, "wf", map[string]any{"x": 1}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	parent, err := st.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	parent.Status = store.RunStatusPausedWaitingHuman
	parent.Checkpoint = &store.Checkpoint{
		// The pause — and its stranding — live at pause, whose last turn
		// is the stranded crossing's own.
		NodeID:      "pause",
		BackendName: "claw",
		BackendConversation: json.RawMessage(
			`[{"role":"user","content":[{"type":"text","text":"crossing"}]}]`),
		BackendPendingToolUseID: "toolu_9",
		BackendSessionStranded:  true,
	}
	if err := st.SaveRun(context.Background(), parent); err != nil {
		t.Fatalf("save parent: %v", err)
	}
	turns := []*store.TurnCheckpoint{
		{
			RunID: parentID, NodeID: "pause", LoopIter: 0, TurnIndex: 2,
			Backend: "claw", FinishReason: "tool_use",
			MessagesRef: "pause/0/2.messages.json",
			Messages:    json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"normal turn"}]}]`),
			WrittenAt:   time.Now().UTC(),
		},
		{
			RunID: parentID, NodeID: "pause", LoopIter: 0, TurnIndex: 3,
			Backend: "claw", FinishReason: "tool_use",
			MessagesRef: "pause/0/3.messages.json",
			Messages:    json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"crossing turn"}]}]`),
			WrittenAt:   time.Now().UTC(),
		},
	}
	for _, tc := range turns {
		if err := st.WriteTurn(context.Background(), tc); err != nil {
			t.Fatalf("write turn %d: %v", tc.TurnIndex, err)
		}
	}

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// The latest turn IS the crossing's own: the stranding fact travels,
	// whatever way the caller named it.
	for _, spec := range []ForkSpec{
		{RunID: parentID, NodeID: "pause", TurnIndex: -1},
		{RunID: parentID, NodeID: "pause", TurnIndex: 3},
	} {
		result, err := svc.Fork(context.Background(), spec)
		if err != nil {
			t.Fatalf("Fork %+v: %v", spec, err)
		}
		child, err := st.LoadRun(context.Background(), result.NewRunID)
		if err != nil {
			t.Fatalf("load child: %v", err)
		}
		if !child.Checkpoint.BackendSessionStranded {
			t.Errorf("fork at the pause node's latest turn (spec turn %d) lost the stranding fact — the child's resumed crossing would mirror the stranded conversation into the session store again", spec.TurnIndex)
		}
	}
	// An earlier turn of the pause node is a normal turn that joined the
	// session — stamping it stranded would refuse the child's captures.
	result, err := svc.Fork(context.Background(), ForkSpec{
		RunID:     parentID,
		NodeID:    "pause",
		TurnIndex: 2,
	})
	if err != nil {
		t.Fatalf("Fork at the earlier turn: %v", err)
	}
	child, err := st.LoadRun(context.Background(), result.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.Checkpoint.BackendSessionStranded {
		t.Errorf("fork at the pause node's EARLIER turn stamped BackendSessionStranded: that conversation joined the session normally — OnSessionCapture would silently refuse the child's captures")
	}
}
