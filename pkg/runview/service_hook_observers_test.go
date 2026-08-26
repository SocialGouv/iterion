package runview

import (
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// hookEventObservers is the single way launch AND resume build
// ExecutorSpec.EventObservers. It must always include the broker: hook
// events (assistant_text, tool_*, llm_*) never fire the engine's
// observer, so a path missing the broker leaves resume-spawned
// supervisors and live subscribers blind to the agent's own words —
// exactly what the resume path shipped with.
func TestHookEventObserversReachTheBroker(t *testing.T) {
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const runID = "run-hook-obs"
	sub := svc.broker.Subscribe(runID)
	defer sub.Cancel()

	var extraSeen bool
	obs := svc.hookEventObservers([]func(store.Event){func(store.Event) { extraSeen = true }})
	evt := store.Event{Seq: 7, Type: store.EventAssistantText, RunID: runID, NodeID: "campaign"}
	for _, fn := range obs {
		fn(evt)
	}

	if !extraSeen {
		t.Error("extra observer not invoked")
	}
	select {
	case got := <-sub.C:
		if got.Type != store.EventAssistantText || got.Seq != 7 {
			t.Fatalf("broker got %+v; want the published assistant_text", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("broker subscriber never received the hook event — the broker is missing from hookEventObservers")
	}
}
