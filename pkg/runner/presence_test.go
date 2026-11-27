package runner

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// fakePresence is the runner's view of its fleet registry record.
type fakePresence struct {
	fresh      atomic.Bool
	freshCalls atomic.Int64

	mu sync.Mutex
	// loopCancelledAtMark records whether the loop context was already
	// cancelled when MarkDraining ran — the ordering the floor relies on.
	marked              int
	loopCancelledAtMark bool
	loopCtx             context.Context
	markErr             error
	// holdMark, when set, keeps MarkDraining blocked until it is closed —
	// a registry that does not answer.
	holdMark chan struct{}
}

func (f *fakePresence) Fresh() bool {
	f.freshCalls.Add(1)
	return f.fresh.Load()
}

func (f *fakePresence) MarkDraining(context.Context) error {
	f.mu.Lock()
	hold := f.holdMark
	f.marked++
	if f.loopCtx != nil {
		f.loopCancelledAtMark = f.loopCtx.Err() != nil
	}
	err := f.markErr
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	return err
}

func (f *fakePresence) markedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marked
}

// waitMarked waits for the asynchronous draining mark.
func waitMarked(t *testing.T, p *fakePresence) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.markedCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("MarkDraining never ran")
		}
		time.Sleep(time.Millisecond)
	}
}

// A runner whose registry record may have expired must not take work: the
// engine floor would not have counted its build. The loop never reaches the
// consumer (nil here — touching it would panic) while the record is stale.
func TestRun_DoesNotFetchWhileThePresenceIsStale(t *testing.T) {
	prev := presencePauseDelay
	presencePauseDelay = time.Millisecond
	t.Cleanup(func() { presencePauseDelay = prev })

	p := &fakePresence{}
	// SandboxOverride none keeps the reaper the loop starts off the cluster.
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), Presence: p, FetchWait: time.Millisecond, SandboxOverride: "none"}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for p.freshCalls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p.freshCalls.Load() < 3 {
		t.Fatal("the loop never asked whether its record is fresh")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel while paused")
	}
}

func TestPresenceAllowsFetch(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	if !r.presenceAllowsFetch() {
		t.Fatal("no registry (local run) must not pause fetching")
	}
	p := &fakePresence{}
	r.cfg.Presence = p
	if r.presenceAllowsFetch() {
		t.Fatal("a stale record must pause fetching")
	}
	if !r.Health().PresencePaused {
		t.Fatal("the probes do not report the pause")
	}
	p.fresh.Store(true)
	if !r.presenceAllowsFetch() {
		t.Fatal("a fresh record must let the loop fetch")
	}
	if r.Health().PresencePaused {
		t.Fatal("the probes still report a pause after the record is fresh again")
	}
}

// The stall detector skips the samples of a loop paused on its record: a
// loop that pulls nothing is no witness of the consumer's deliveries. Both
// directions — a pause never raises the wedge, and never clears a real one.
func TestStallTracker_APausedSampleIsSkipped(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	tr := &stallTracker{stalledAfter: time.Minute}

	// Held work, no progress, for longer than the window — but paused: no
	// onset, and the episode clock did not start.
	for i := 0; i < 5; i++ {
		if stalled, edge, observed := tr.sample(true, true, false, t0.Add(time.Duration(i)*time.Minute)); stalled || edge != stallNone || observed {
			t.Fatalf("paused sample %d: stalled=%v edge=%v observed=%v, want skipped", i, stalled, edge, observed)
		}
	}
	if !tr.since.IsZero() {
		t.Fatal("a paused sample started the episode clock")
	}

	// A real wedge, raised while pulling...
	tr.sample(false, true, false, t0)
	if stalled, edge, _ := tr.sample(false, true, false, t0.Add(2*time.Minute)); !stalled || edge != stallOnset {
		t.Fatalf("wedge: stalled=%v edge=%v, want the onset", stalled, edge)
	}
	// ...is not cleared by the pause that follows.
	if stalled, edge, observed := tr.sample(true, true, true, t0.Add(3*time.Minute)); !stalled || edge != stallNone || observed {
		t.Fatalf("paused over a raised wedge: stalled=%v edge=%v observed=%v, want the stall kept and the sample skipped", stalled, edge, observed)
	}
	if !tr.stalled {
		t.Fatal("the pause cleared a real wedge")
	}
}

// The drain never waits on the registry: with the draining mark stuck on a
// registry that does not answer, Shutdown still engages its drain at once —
// an interrupt drain cancels its run, a lame-duck one waits for it, neither
// behind the mark. Here the run has already finished, so the drain itself
// costs nothing and Shutdown must return immediately.
func TestShutdown_DoesNotWaitOnTheRegistry(t *testing.T) {
	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &fakePresence{loopCtx: loopCtx, holdMark: make(chan struct{})}
	defer close(p.holdMark)
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), Presence: p, DrainMode: DrainModeComplete, DrainTimeout: time.Hour}}
	r.cancel = cancel
	done := make(chan struct{})
	close(done)
	r.current = &inFlight{runID: "run-1", done: done, cancelFn: func(error) {}}
	returned := make(chan struct{})
	go func() { _ = r.Shutdown(context.Background()); close(returned) }()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown waited on a registry that does not answer the draining mark")
	}
	waitMarked(t, p)
}

// The draining mark lands only once fetching has stopped: a runner marked
// draining while still fetching could take a run the floor no longer counts.
func TestShutdown_MarksDrainingAfterFetchingStopped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inFlight bool
	}{{"idle", false}, {"with a run in flight", true}} {
		t.Run(tc.name, func(t *testing.T) {
			loopCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &fakePresence{loopCtx: loopCtx, markErr: errors.New("kv down")}
			r := &Runner{cfg: Config{Logger: iterlog.Nop(), Presence: p, DrainMode: DrainModeComplete, DrainTimeout: time.Hour}}
			r.cancel = cancel
			if tc.inFlight {
				done := make(chan struct{})
				close(done) // the run has already finished: Shutdown returns at once
				r.current = &inFlight{runID: "run-1", cancelFn: func(error) {}, done: done}
			}
			if err := r.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitMarked(t, p)
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.marked != 1 {
				t.Fatalf("MarkDraining called %d times, want once", p.marked)
			}
			if !p.loopCancelledAtMark {
				t.Fatal("MarkDraining ran before the loop stopped fetching")
			}
		})
	}
}
