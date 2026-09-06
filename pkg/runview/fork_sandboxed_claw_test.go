package runview

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #811 — `iterion fork --turn` on a run whose claw node executed inside a
// sandbox. The tool loop, and with it every turn capture, runs in the
// container; the host learns of a turn only through the `__claw-runner`
// relay. This drives the host half of that relay (the decode the
// multiplexer handler performs on each `event` envelope) into the run's
// own store hooks, then forks through the very API `iterion fork` calls —
// so a missing relay shows up as "no turn checkpoint", and a relay that
// drops the snapshot shows up as a child with no conversation.
func TestFork_SandboxedClawTurnRelayedFromTheContainer(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const parentID = "run-sandboxed-claw"
	if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	parent, err := st.LoadRun(ctx, parentID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	parent.Checkpoint = &store.Checkpoint{NodeID: "campaign"}
	parent.Status = store.RunStatusCancelled
	if err := st.SaveRun(ctx, parent); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	// One turn capture as it arrives from the container: the payload of
	// the `event` envelope the in-container relay writes (see
	// model.SandboxRelayHooks and its round-trip test
	// TestSandboxRelay_TurnCaptureCrossesIntoTheRunsTurnStore).
	const relayed = `{
		"step": 1,
		"text": "patching the handler",
		"finish_reason": "tool_use",
		"input_tokens": 12000,
		"output_tokens": 300,
		"iteration": 0,
		"conversation": [
			{"role":"user","content":[{"type":"text","text":"ship the feature"}]},
			{"role":"assistant","content":[{"type":"text","text":"patching the handler"}]}
		]
	}`
	var payload map[string]any
	if err := json.Unmarshal([]byte(relayed), &payload); err != nil {
		t.Fatalf("decode relayed payload: %v", err)
	}
	hooks := model.NewStoreEventHooks(ctx, st, parentID, logger, nil)
	handled, err := model.ApplyRelayedEvent(hooks, "campaign", "llm_turn_capture", payload)
	if err != nil {
		t.Fatalf("ApplyRelayedEvent: %v", err)
	}
	if !handled {
		t.Fatal("the host drops a relayed llm_turn_capture: a sandboxed claw run has no fork anchor at all")
	}

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := svc.Fork(ctx, ForkSpec{RunID: parentID, NodeID: "campaign", TurnIndex: -1})
	if err != nil {
		t.Fatalf("Fork on the relayed turn: %v", err)
	}
	child, err := st.LoadRun(ctx, result.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.Checkpoint == nil || child.Checkpoint.BackendName != "claw" {
		t.Fatalf("child checkpoint = %+v, want the claw backend the relayed turn named", child.Checkpoint)
	}
	if len(child.Checkpoint.BackendConversation) == 0 {
		t.Fatal("child carries no BackendConversation: the fork replays nothing the sandboxed node had said")
	}
	var msgs []map[string]any
	if err := json.Unmarshal(child.Checkpoint.BackendConversation, &msgs); err != nil {
		t.Fatalf("child conversation is not a message list: %v", err)
	}
	if len(msgs) != 2 || msgs[0]["role"] != "user" || msgs[1]["role"] != "assistant" {
		t.Fatalf("child conversation = %+v, want the two messages the container captured", msgs)
	}
}
