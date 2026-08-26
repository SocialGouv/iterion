package store

import (
	"context"
	"testing"
)

// A finished run.json must name the model that served each node without
// replaying events.jsonl — #474. Last write wins per node so a loop's
// last pass is what inspect shows; parallel nodes must not clobber.
func TestRecordNodeServedRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateRun(ctx, "run-served", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	want := NodeServed{
		Backend:         "claude_code",
		Model:           "glm-4.6",
		DeclaredModel:   "anthropic/claude-opus-5",
		ContextWindow:   200_000,
		MaxOutputTokens: 8192,
	}
	if err := s.RecordNodeServed(ctx, "run-served", "campaign", want); err != nil {
		t.Fatalf("RecordNodeServed: %v", err)
	}
	got, err := s.LoadRun(ctx, "run-served")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.NodesServed["campaign"] != want {
		t.Errorf("NodesServed[campaign] = %+v, want %+v", got.NodesServed["campaign"], want)
	}

	if err := s.RecordNodeServed(ctx, "run-served", "", want); err != nil {
		t.Errorf("empty nodeID: %v", err)
	}
	if err := s.RecordNodeServed(ctx, "no-such-run", "n", want); err == nil {
		t.Error("unknown run returned no error")
	}
}
