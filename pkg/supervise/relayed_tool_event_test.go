package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #811 — a supervisor's tool monitor must arm on a SANDBOXED claw node
// exactly as on an in-process one. Tool events fire inside the container,
// reach the host only through the `__claw-runner` relay, and the hub is
// fed by the same store hooks (ExecutorSpec.EventObservers) that carry
// the in-process ones. Without the relay this hub stays silent for the
// whole node and a `tool_error` monitor never fires.
func TestRelayedToolEventArmsAToolMonitor(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-supervised-sandbox"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hub := NewEventHub()
	events, release, err := hub.ObserveRun(ctx, runID)
	if err != nil {
		t.Fatalf("ObserveRun: %v", err)
	}
	defer release()
	hostHooks := model.NewStoreEventHooks(ctx, st, runID, iterlog.Nop(), nil, hub.Publish)

	// The runner half, inside the container: the failing tool call fires
	// on the relay hooks, which encode it as an `event` envelope.
	var wire []delegate.Envelope
	relay := model.SandboxRelayHooks(func(env delegate.Envelope) error {
		wire = append(wire, env)
		return nil
	}, func(err error) { t.Errorf("relay: %v", err) })
	if relay.OnToolCall == nil {
		t.Fatal("the runner's relay carries no tool hook: a supervisor watching a sandboxed claw node sees no tool at all")
	}
	relay.OnToolCall("implement", model.LLMToolCallInfo{
		ToolName:  "bash",
		ToolUseID: "tu_7",
		Output:    "FAIL\texit status 1",
		Duration:  900 * time.Millisecond,
		Error:     errors.New("exit status 1"),
	})

	// The launcher half: decode each envelope and re-fire the host's own
	// hooks — what the multiplexer handler's OnEvent does in production.
	for _, env := range wire {
		if env.Type != delegate.EnvelopeEvent {
			continue
		}
		var ed delegate.EventData
		if err := json.Unmarshal(env.Data, &ed); err != nil {
			t.Fatalf("decode event envelope: %v", err)
		}
		handled, err := model.ApplyRelayedEvent(hostHooks, "implement", ed.Type, ed.Payload)
		if err != nil || !handled {
			t.Fatalf("ApplyRelayedEvent(%s) = handled %v, err %v", ed.Type, handled, err)
		}
	}

	monitor := Monitor{EventType: string(store.EventToolError), ToolName: "bash"}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt == nil {
				t.Fatal("hub closed before the relayed tool event arrived")
			}
			if !monitor.matches(evt) {
				continue
			}
			if evt.NodeID != "implement" {
				t.Errorf("matched event node = %q, want implement", evt.NodeID)
			}
			return
		case <-deadline:
			t.Fatal("no event the tool monitor matches: a sandboxed claw node's failing bash never reaches its supervisor")
		}
	}
}
