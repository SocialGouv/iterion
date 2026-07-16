package runview

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/clock"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// pausingGateBot pauses immediately on a human entry node so a Launch
// test can inspect the persisted run doc + the events it emitted, with
// no backend credentials.
const pausingGateBot = `
schema gate_out:
  approve: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow dispatch_fields_demo:
  entry: gate
  gate -> done when approve
  gate -> fail when not approve
`

func writePausingGateBot(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "gate.bot")
	if err := os.WriteFile(p, []byte(pausingGateBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	return p
}

// TestLaunch_HonoursDispatcherConvergenceFields exercises the four
// ADR-046 LaunchSpec additions on the in-process Launch path:
//   - SourceRef is stamped onto the persisted run record.
//   - ExtraObservers fire on every store AppendEvent (the stall-heartbeat
//     contract the dispatcher relies on).
//   - WorkDir + DailyCap are accepted and don't break the engine.
func TestLaunch_HonoursDispatcherConvergenceFields(t *testing.T) {
	dir := t.TempDir()
	botPath := writePausingGateBot(t, dir)

	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	var (
		mu   sync.Mutex
		seen []string
	)
	observer := func(evt store.Event) {
		mu.Lock()
		seen = append(seen, string(evt.Type))
		mu.Unlock()
	}

	src := &store.RunSource{
		Kind:            store.RunSourceKindDispatcher,
		IssueID:         "native:abc123",
		IssueIdentifier: "abc123",
		IssueTitle:      "Ship ADR-046",
	}

	// A distinct per-launch daily cap (a fresh guard, not the service
	// default) — the pause bot spends $0 so it never trips; we only assert
	// it is accepted and the launch still pauses cleanly.
	cap := runtime.NewDailyCapGuard(store.AsSpendStore(svc.store), clock.Default, runtime.DailyCapConfig{MaxCostPerDayUSD: 999})

	res, err := svc.Launch(context.Background(), LaunchSpec{
		FilePath:       botPath,
		WorkDir:        dir,
		ExtraObservers: []func(store.Event){observer},
		DailyCap:       cap,
		SourceRef:      src,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case <-res.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("run goroutine did not exit (expected immediate human pause)")
	}

	// SourceRef → persisted run record.
	r, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Source == nil {
		t.Fatal("run.Source = nil, want the dispatcher SourceRef stamped")
	}
	if r.Source.Kind != store.RunSourceKindDispatcher || r.Source.IssueID != "native:abc123" || r.Source.IssueIdentifier != "abc123" || r.Source.IssueTitle != "Ship ADR-046" {
		t.Errorf("run.Source = %+v, want the passed SourceRef", r.Source)
	}

	// ExtraObservers → fired on store-level events (at least the run
	// lifecycle + pause). The store-level wrap is what keeps the
	// dispatcher's stall watermark alive on long agent nodes.
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("ExtraObserver never fired; store-level wiring is broken")
	}
	if !containsEvent(got, string(store.EventRunStarted)) {
		t.Errorf("observer events = %v, want at least a run_started", got)
	}
}

func containsEvent(events []string, want string) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}

// TestWrapWithObservers_NoObserversIsIdentity guards the hot-path
// promise: a launch with no ExtraObservers pays nothing (the store is
// returned unwrapped, not boxed).
func TestWrapWithObservers_NoObserversIsIdentity(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if got := wrapWithObservers(svc.store, nil); got != store.RunStore(svc.store) {
		t.Error("wrapWithObservers(s, nil) should return the store unchanged")
	}
	wrapped := wrapWithObservers(svc.store, []func(store.Event){func(store.Event) {}})
	if _, ok := wrapped.(*observerStore); !ok {
		t.Errorf("wrapWithObservers with observers = %T, want *observerStore", wrapped)
	}
}
