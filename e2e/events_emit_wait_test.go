package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestEventsEmitWait is the executable proof for ADR-051: in-bot event-driven
// coordination. examples/events/pingpong.bot fans out two parallel branches —
// a producer that `emit`s event "ready" carrying value=42, and a consumer that
// `wait`s for it — and converges. The consumer can only obtain the value by
// receiving the event, so a value of 42 at convergence proves the emit→wait
// handoff worked across branches. No LLM, no shell.
func TestEventsEmitWait(t *testing.T) {
	wf := compileFixture(t, "events/pingpong.bot")

	s := tmpStore(t)
	// No LLM/tool nodes — emit/wait/compute are engine-handled; the stub
	// executor is never invoked.
	eng := runtime.New(wf, s, newScenarioExecutor())

	if err := eng.Run(context.Background(), "e2e-events-pingpong", nil); err != nil {
		t.Fatalf("run pingpong: %v", err)
	}

	r, err := s.LoadRun(context.Background(), "e2e-events-pingpong")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}

	// gather (the convergence node) reads outputs.pong.value — the payload the
	// consumer received from the producer's event. If the emit→wait handoff
	// failed, the wait would have timed out and the run would have failed.
	art, err := s.LoadLatestArtifact(context.Background(), "e2e-events-pingpong", "gather")
	if err != nil {
		t.Fatalf("load gather artifact: %v", err)
	}
	if got := toInt(art.Data["received"]); got != 42 {
		t.Errorf("gather.received = %d, want 42 (the value carried by event \"ready\")", got)
	}
}
