package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestRunEvents_Sticky verifies the run-scoped registry's two delivery orders:
// a wait that arrives AFTER the emit still observes it (sticky), and a wait that
// arrives BEFORE the emit blocks until the signal.
func TestRunEvents_Sticky(t *testing.T) {
	re := newRunEvents()

	// emit-then-wait: the channel is already closed, payload is available.
	re.signal("ready", map[string]any{"value": 42})
	select {
	case <-re.waitChan("ready"):
	default:
		t.Fatal("waitChan for an already-fired event must be closed (sticky)")
	}
	if got := re.payloadFor("ready"); got["value"] != 42 {
		t.Errorf("payload value = %v, want 42", got["value"])
	}

	// wait-then-emit: the channel is open until signal closes it.
	ch := re.waitChan("later")
	select {
	case <-ch:
		t.Fatal("waitChan for an un-fired event must block")
	default:
	}
	re.signal("later", map[string]any{"value": 7})
	select {
	case <-ch:
	default:
		t.Fatal("waitChan must be closed after signal")
	}
	if got := re.payloadFor("later"); got["value"] != 7 {
		t.Errorf("payload value = %v, want 7", got["value"])
	}
}

// TestRunEvents_PayloadDeepIsolation verifies the ADR-051 immutability boundary
// holds for NESTED payload values: a waiter that mutates a nested map/slice in
// the payload it received must not corrupt the registry's stored event (nor any
// other waiter's copy). This is the regression for the shallow-clone bug — a
// per-key copy would leave the nested map aliased and let the mutation leak back.
func TestRunEvents_PayloadDeepIsolation(t *testing.T) {
	re := newRunEvents()
	re.signal("nested", map[string]any{
		"meta": map[string]any{"count": 1},
		"tags": []any{"a", "b"},
	})

	// First waiter reads the payload and mutates the nested structures.
	first := re.payloadFor("nested")
	first["meta"].(map[string]any)["count"] = 999
	first["tags"].([]any)[0] = "MUTATED"

	// A second, independent read must still see the original nested values.
	second := re.payloadFor("nested")
	if got := second["meta"].(map[string]any)["count"]; got != 1 {
		t.Errorf("nested map leaked mutation: count = %v, want 1", got)
	}
	if got := second["tags"].([]any)[0]; got != "a" {
		t.Errorf("nested slice leaked mutation: tags[0] = %v, want \"a\"", got)
	}
}

// TestRunEvents_DoubleSignal verifies a second emit of the same event does not
// panic on a re-close and refreshes the payload.
func TestRunEvents_DoubleSignal(t *testing.T) {
	re := newRunEvents()
	re.signal("e", map[string]any{"n": 1})
	re.signal("e", map[string]any{"n": 2}) // must not panic on re-close
	if got := re.payloadFor("e"); got["n"] != 2 {
		t.Errorf("payload n = %v, want 2 (latest)", got["n"])
	}
}

func TestRunEvents_CheckpointRoundTripRestoresStickySignal(t *testing.T) {
	original := newRunEvents()
	original.signal("ready", map[string]any{
		"nested": map[string]any{"value": "ok"},
	})

	restored := newRunEvents()
	restored.restore(original.snapshot())
	select {
	case <-restored.waitChan("ready"):
	default:
		t.Fatal("restored fired event did not close its sticky waiter")
	}
	got := restored.payloadFor("ready")
	if got["nested"].(map[string]any)["value"] != "ok" {
		t.Fatalf("restored payload = %#v", got)
	}
	got["nested"].(map[string]any)["value"] = "mutated"
	if again := restored.payloadFor("ready"); again["nested"].(map[string]any)["value"] != "ok" {
		t.Fatalf("restored payload aliases caller mutation: %#v", again)
	}
}

func TestFanOutEachResumeRestoresEventFromCompletedSibling(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "fanout_resume_sticky_event",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"dispatch": &ir.RouterNode{
				BaseNode:    ir.BaseNode{ID: "dispatch"},
				RouterMode:  ir.RouterFanOutEach,
				Over:        "{{outputs.entry.items}}",
				OverRefs:    []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "items"}}},
				ItemBinding: "item",
				KeyField:    "id",
				DepsField:   "deps",
			},
			"route":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "route"}},
			"emit_ready": &ir.EmitNode{BaseNode: ir.BaseNode{ID: "emit_ready"}, Event: "ready"},
			"gate":       &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}},
			"wait_ready": &ir.WaitNode{BaseNode: ir.BaseNode{ID: "wait_ready"}, Event: "ready", Timeout: 250 * time.Millisecond},
			"collect":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitWaitAll},
			"done":       &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "dispatch"},
			{
				From: "dispatch",
				To:   "route",
				With: []*ir.DataMapping{{
					Key: "id",
					Refs: []*ir.Ref{{
						Kind: ir.RefOutputs,
						Path: []string{"dispatch", "item", "id"},
					}},
				}},
			},
			{From: "route", To: "emit_ready", Condition: "emitter"},
			{From: "route", To: "gate", IsElse: true},
			{From: "emit_ready", To: "collect"},
			{From: "gate", To: "wait_ready"},
			{From: "wait_ready", To: "collect"},
			{From: "collect", To: "done"},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{}, Foreaches: map[string]*ir.Foreach{},
	}
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{
			map[string]any{"id": "emitter", "deps": []any{}},
			map[string]any{"id": "waiter", "deps": []any{"emitter"}},
		}}, nil
	})
	exec.on("route", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"emitter": input["id"] == "emitter"}, nil
	})

	runStore := tmpStore(t)
	const runID = "fanout-resume-sticky-event"
	if err := New(wf, runStore, exec).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want branch pause", err)
	}
	run, err := runStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Checkpoint == nil || run.Checkpoint.FiredEvents["ready"] == nil {
		t.Fatalf("checkpoint fired events = %#v, want ready from completed sibling", run.Checkpoint)
	}
	if err := New(wf, runStore, exec).Resume(context.Background(), runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("resume lost completed sibling event: %v", err)
	}
}
