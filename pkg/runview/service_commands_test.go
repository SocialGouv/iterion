package runview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// steerTestService builds a Service over a temp store with one seeded
// run in the given status. Returns the service and the run id.
func steerTestService(t *testing.T, status store.RunStatus) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { svc.Stop(context.Background()) })

	seed, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if _, err := seed.CreateRun(context.Background(), "r-steer", "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := seed.LoadRun(context.Background(), "r-steer")
	if err != nil {
		t.Fatal(err)
	}
	r.Status = status
	if err := seed.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return svc, "r-steer"
}

func TestBumpLoopCtx_TerminalRunIs409(t *testing.T) {
	svc, runID := steerTestService(t, store.RunStatusFinished)
	_, err := svc.BumpLoopCtx(context.Background(), runID, BumpLoopRequest{LoopName: "l", Delta: 1})
	var te *RunTerminalError
	if !errors.As(err, &te) || te.Status != store.RunStatusFinished {
		t.Fatalf("err = %v, want RunTerminalError{finished}", err)
	}
}

func TestBumpLoopCtx_NotHeldIs409(t *testing.T) {
	// Status running but no live engine in THIS process (orphan / other
	// process) and no publisher: the truthful answer is "not held".
	svc, runID := steerTestService(t, store.RunStatusRunning)
	_, err := svc.BumpLoopCtx(context.Background(), runID, BumpLoopRequest{LoopName: "l", Delta: 1})
	if !errors.Is(err, ErrRunNotHeld) {
		t.Fatalf("err = %v, want ErrRunNotHeld", err)
	}
}

func TestBumpLoopCtx_UnknownRunIs404Shaped(t *testing.T) {
	svc, _ := steerTestService(t, store.RunStatusRunning)
	_, err := svc.BumpLoopCtx(context.Background(), "absent", BumpLoopRequest{LoopName: "l", Delta: 1})
	if !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

func TestRaiseBudgetCtx_ValidationBeforeLookup(t *testing.T) {
	svc, runID := steerTestService(t, store.RunStatusRunning)
	_, err := svc.RaiseBudgetCtx(context.Background(), runID, RaiseBudgetRequest{})
	if !errors.Is(err, runtime.ErrInvalidOverride) {
		t.Fatalf("empty raise err = %v, want ErrInvalidOverride", err)
	}
	_, err = svc.RaiseBudgetCtx(context.Background(), runID, RaiseBudgetRequest{
		Budget: ir.BudgetOverrides{MaxDuration: "bogus"},
	})
	if !errors.Is(err, runtime.ErrInvalidOverride) {
		t.Fatalf("bad duration err = %v, want ErrInvalidOverride", err)
	}
}

func TestBumpLoopCtx_LiveEngineApplies(t *testing.T) {
	svc, runID := steerTestService(t, store.RunStatusRunning)

	// Register a live steering channel with a consumer goroutine
	// standing in for the engine's drain: it acks the contract shapes
	// the real applyOverride produces (the apply logic itself is
	// covered by the runtime override tests).
	wf := &ir.Workflow{Name: "w", Nodes: map[string]ir.Node{}, Loops: map[string]*ir.Loop{}}
	eng := runtime.New(wf, svc.store, nil)
	ch := make(chan *runtime.OverrideMsg, 8)
	svc.registerRunEngine(runID, eng, ch)
	t.Cleanup(func() { svc.unregisterRunEngine(runID) })

	go func() {
		msg := <-ch
		msg.Ack(runtime.OverrideResult{
			Applied:   map[string]any{"loop": "retry", "delta": 3, "extra": 3},
			Effective: map[string]any{"effective_max": 5, "current": 2},
		})
	}()
	res, err := svc.BumpLoopCtx(context.Background(), runID, BumpLoopRequest{LoopName: "retry", Delta: 3})
	if err != nil {
		t.Fatalf("BumpLoopCtx: %v", err)
	}
	if res.EffectiveMax != 5 || res.Extra != 3 || res.Current != 2 {
		t.Fatalf("res = %+v, want effective_max 5 extra 3 current 2", res)
	}

	// Unknown loop through the same live path → typed error propagated.
	go func() {
		msg := <-ch
		msg.Ack(runtime.OverrideResult{Err: &runtime.UnknownLoopError{Loop: "nope", Available: []string{"retry"}}})
	}()
	_, err = svc.BumpLoopCtx(context.Background(), runID, BumpLoopRequest{LoopName: "nope", Delta: 1})
	var ule *runtime.UnknownLoopError
	if !errors.As(err, &ule) {
		t.Fatalf("err = %v, want UnknownLoopError", err)
	}

	// Noop raise: contract carries reason, not an error.
	go func() {
		msg := <-ch
		msg.Ack(runtime.OverrideResult{Noop: true, NoopReason: "new caps do not exceed the current ones",
			Effective: map[string]any{"max_tokens": 1000}})
	}()
	rres, err := svc.RaiseBudgetCtx(context.Background(), runID, RaiseBudgetRequest{Budget: ir.BudgetOverrides{MaxTokens: 500}})
	if err != nil {
		t.Fatalf("RaiseBudgetCtx noop: %v", err)
	}
	if !rres.Noop || rres.NoopReason == "" {
		t.Fatalf("rres = %+v, want noop with reason", rres)
	}
}

func TestBumpLoopCtx_BusyEngineIsPending(t *testing.T) {
	svc, runID := steerTestService(t, store.RunStatusRunning)
	wf := &ir.Workflow{Name: "w", Nodes: map[string]ir.Node{}, Loops: map[string]*ir.Loop{}}
	eng := runtime.New(wf, svc.store, nil)
	ch := make(chan *runtime.OverrideMsg, 8)
	svc.registerRunEngine(runID, eng, ch)
	t.Cleanup(func() { svc.unregisterRunEngine(runID) })

	// Nothing drains the channel: the command is delivered but unacked
	// (run busy inside a long node). The caller gets the honest
	// "queued, will apply" — not a fake success, not a hang.
	start := time.Now()
	_, err := svc.BumpLoopCtx(context.Background(), runID, BumpLoopRequest{LoopName: "l", Delta: 1})
	if !errors.Is(err, ErrSteerPending) {
		t.Fatalf("err = %v, want ErrSteerPending", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("pending took %s, want bounded by steerAckTimeout", elapsed)
	}
}

func TestAnswerHumanCtx_WrongStateIs409(t *testing.T) {
	svc, runID := steerTestService(t, store.RunStatusRunning)
	_, err := svc.AnswerHumanCtx(context.Background(), runID, map[string]any{"ok": true})
	if !errors.Is(err, ErrNotAwaitingHuman) {
		t.Fatalf("err = %v, want ErrNotAwaitingHuman", err)
	}
}
