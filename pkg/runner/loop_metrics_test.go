package runner

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
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

// The wiring a source pin cannot see: the sink must land on the slice
// ELEMENT StartDeclared receives, not on a copy of it — a sink set on a
// copied struct books nowhere, and every supervisor evaluation spends
// while the run's ledger stays empty (Revi #2721 R1: the OnUsage closure
// was assigned to a copy of specs[i] and never reached StartDeclared).
func TestMeterSupervisorSpecs_TheSinkRidesTheSliceElement(t *testing.T) {
	specs := []supervise.Spec{{Name: "pacer", Model: "openai_compatible/does-not-resolve"}}
	m := newMetricsEmitter(nil, nil)
	meterSupervisorSpecs(context.Background(), specs, m)
	if specs[0].OnUsage == nil {
		t.Fatal("meterSupervisorSpecs left OnUsage nil on the slice element — StartDeclared would receive a sink-less spec and every evaluation would spend off the ledger")
	}
	specs[0].OnUsage(10, 5)
	// A gateway id resolves to itself — the route the evaluation books on
	// names the wire id the walk judged.
	got, ok := m.byRoute[routeKey{backend: delegate.BackendClaw, model: "openai_compatible/does-not-resolve", source: "supervisor:pacer"}]
	if !ok {
		t.Fatalf("the sink booked no supervisor:pacer route; ledger: %v", m.byRoute)
	}
	if got.inputTokens != 10 || got.outputTokens != 5 || got.aggregateTokens != 15 {
		t.Errorf("the sink booked (%d, %d, %d); want (10, 5, 15)", got.inputTokens, got.outputTokens, got.aggregateTokens)
	}
}
