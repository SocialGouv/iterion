package runner

import (
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The locked entry the OnUsage closure calls must stay locked: the sink
// and the engine's event stream write the same emitter from different
// goroutines (the coordinator vs observe), and an unlocked write is a
// concurrent map write that kills the process — every run on the pod with
// it. This witness races the wrapper itself; the WIRING is guarded by the
// source pin in loop_pool_test.go, which refuses an unlocked call at the
// closure's site (rva r3 R2: a wrapper-only witness cannot see the site).
func TestSupervisorUsageSink_IsRaceFreeAgainstTheEventStream(t *testing.T) {
	m := newMetricsEmitter(nil, nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			m.addRoute(delegate.BackendClaw, "anthropic/claude-opus-5-5", "supervisor:pacer", 0, 1, 1, 2)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			m.observe(store.Event{Type: store.EventLLMStepFinished, NodeID: "n", RunID: "r",
				Data: map[string]any{"input_tokens": 10, "output_tokens": 5}})
		}
	}()
	wg.Wait()
}
