package sseutil

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// expect asserts that got matches the wanted shape on the fields that matter
// for the accumulator (Type, Index, ContentBlock, Delta). Fields outside
// that subset are ignored.
func expect(t *testing.T, got []api.StreamEvent, want []api.StreamEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count mismatch: got %d, want %d\ngot:  %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i].Type != want[i].Type {
			t.Errorf("event[%d].Type = %q, want %q", i, got[i].Type, want[i].Type)
		}
		if got[i].Index != want[i].Index {
			t.Errorf("event[%d].Index = %d, want %d", i, got[i].Index, want[i].Index)
		}
		if got[i].ContentBlock != want[i].ContentBlock {
			t.Errorf("event[%d].ContentBlock = %+v, want %+v", i, got[i].ContentBlock, want[i].ContentBlock)
		}
		if got[i].Delta != want[i].Delta {
			t.Errorf("event[%d].Delta = %+v, want %+v", i, got[i].Delta, want[i].Delta)
		}
	}
}

func TestAccumulator_NoEventsBeforeIDAndName(t *testing.T) {
	acc := NewToolCallAccumulator(1)
	if got := acc.HandleDelta("call_1", "", ""); len(got) != 0 {
		t.Errorf("expected no events with id only, got %v", got)
	}
	if acc.Started() {
		t.Errorf("Started() should be false before name arrives")
	}
}

func TestAccumulator_StartEmittedOnceIDAndNameKnown(t *testing.T) {
	acc := NewToolCallAccumulator(2)
	// First delta brings id only.
	expect(t, acc.HandleDelta("call_1", "", ""), nil)
	// Second delta brings name; start should fire.
	expect(t, acc.HandleDelta("", "do_x", ""), []api.StreamEvent{{
		Type:         api.EventContentBlockStart,
		Index:        2,
		ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 2, ID: "call_1", Name: "do_x"},
	}})
	if !acc.Started() {
		t.Errorf("Started() should be true after id+name")
	}
	// Idempotent: another delta with no new info emits nothing.
	expect(t, acc.HandleDelta("", "", ""), nil)
}

func TestAccumulator_BufferedArgsFlushOnStart(t *testing.T) {
	acc := NewToolCallAccumulator(0)
	// Arguments arrive before id/name (rare, but observed in some streams).
	expect(t, acc.HandleDelta("", "", `{"k":`), nil)
	expect(t, acc.HandleDelta("", "", `"v"}`), nil)
	// id and name arrive together — start event + flushed arg deltas.
	got := acc.HandleDelta("call_42", "do_x", "")
	expect(t, got, []api.StreamEvent{
		{
			Type:         api.EventContentBlockStart,
			Index:        0,
			ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "call_42", Name: "do_x"},
		},
		{
			Type:  api.EventContentBlockDelta,
			Index: 0,
			Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"k":`},
		},
		{
			Type:  api.EventContentBlockDelta,
			Index: 0,
			Delta: api.Delta{Type: "input_json_delta", PartialJSON: `"v"}`},
		},
	})
}

func TestAccumulator_ArgsAfterStart(t *testing.T) {
	acc := NewToolCallAccumulator(3)
	// id+name+chunk all in one delta.
	got := acc.HandleDelta("call_1", "do_x", `{"a":1}`)
	expect(t, got, []api.StreamEvent{
		{
			Type:         api.EventContentBlockStart,
			Index:        3,
			ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 3, ID: "call_1", Name: "do_x"},
		},
		{
			Type:  api.EventContentBlockDelta,
			Index: 3,
			Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"a":1}`},
		},
	})
	// Subsequent arg-only delta emits a single delta event.
	expect(t, acc.HandleDelta("", "", `,"b":2}`), []api.StreamEvent{{
		Type:  api.EventContentBlockDelta,
		Index: 3,
		Delta: api.Delta{Type: "input_json_delta", PartialJSON: `,"b":2}`},
	}})
}

func TestAccumulator_FinishOnlyAfterStart(t *testing.T) {
	never := NewToolCallAccumulator(1)
	if events := never.Finish(); len(events) != 0 {
		t.Errorf("Finish() before start should emit nothing, got %v", events)
	}

	acc := NewToolCallAccumulator(1)
	_ = acc.HandleDelta("c", "n", "")
	expect(t, acc.Finish(), []api.StreamEvent{{Type: api.EventContentBlockStop, Index: 1}})
	// Idempotent.
	if events := acc.Finish(); len(events) != 0 {
		t.Errorf("Finish() called twice should be no-op, got %v", events)
	}
}

func TestAccumulator_MarkStarted(t *testing.T) {
	acc := NewToolCallAccumulator(7)
	got := acc.MarkStarted("call_X", "tool_y")
	if got.Type != api.EventContentBlockStart || got.Index != 7 {
		t.Errorf("MarkStarted should emit ContentBlockStart at index 7, got %+v", got)
	}
	if got.ContentBlock != (api.ContentBlockInfo{Type: "tool_use", Index: 7, ID: "call_X", Name: "tool_y"}) {
		t.Errorf("MarkStarted ContentBlock = %+v", got.ContentBlock)
	}
	// Now an argument-only delta should pass through immediately.
	expect(t, acc.HandleDelta("", "", "ARG"), []api.StreamEvent{{
		Type:  api.EventContentBlockDelta,
		Index: 7,
		Delta: api.Delta{Type: "input_json_delta", PartialJSON: "ARG"},
	}})
	expect(t, acc.Finish(), []api.StreamEvent{{Type: api.EventContentBlockStop, Index: 7}})
}

func TestAccumulator_BlockIndex(t *testing.T) {
	acc := NewToolCallAccumulator(42)
	if acc.BlockIndex() != 42 {
		t.Errorf("BlockIndex = %d, want 42", acc.BlockIndex())
	}
}

func TestAccumulator_IDAndNameStableAcrossDeltas(t *testing.T) {
	// Some providers echo id + name on every delta. Ensure the accumulator
	// keeps the first values rather than being confused by later equal ones.
	acc := NewToolCallAccumulator(0)
	_ = acc.HandleDelta("call_1", "do_x", "")
	// Same id, same name, plus an arg chunk: should emit only one delta event.
	expect(t, acc.HandleDelta("call_1", "do_x", `chunk`), []api.StreamEvent{{
		Type:  api.EventContentBlockDelta,
		Index: 0,
		Delta: api.Delta{Type: "input_json_delta", PartialJSON: "chunk"},
	}})
}
