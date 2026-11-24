package server

import (
	"context"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/runwatch"
)

// announcedWatchReads announces every pass's FIRST read, so a witness can
// tell a loop that returned on its verdict from one that swept again. With an
// err it is a replica whose node cannot reach the watch store — the
// sick-but-alive leader of #2685; without one it delegates, which is the
// idle pass of a fleet with nothing to watch.
type announcedWatchReads struct {
	runwatch.Store
	reads chan struct{}
	err   error
}

func (s *announcedWatchReads) ActiveWatchUpperBound(ctx context.Context) (*runwatch.WatchCursor, error) {
	// Never blocks the sweep: a witness that stopped reading must not hold
	// the loop it is judging.
	select {
	case s.reads <- struct{}{}:
	default:
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.ActiveWatchUpperBound(ctx)
}

func unreachableWatchStore() error {
	return &url.Error{
		Op:  "Get",
		URL: "https://store.example/runwatch",
		Err: &net.DNSError{Err: "no such host", Name: "store.example", IsNotFound: true},
	}
}

// The watch sweep's loop is where the handover actually happens: the verdict
// is recorded by the pass, classified by lease.Health, and then it has to
// make the loop RETURN — returning is what releases the lease (lease.Run) so
// a replica that can read the store arms, observes and delivers instead
// (#2685). Nothing witnessed that return: every existing test of this loop
// ends it by cancelling the term, which is the one way out that would still
// work with the verdict ignored.
//
// So the loop is driven here with a live context and no shutdown channel: the
// only way out is the verdict. The loop is woken once after its first pass,
// so a loop that ignored the verdict sweeps again AT ONCE instead of at the
// next tick — the witness's budget is then about the mechanism, never about
// the sweep interval or the machine.
func TestAssistantWatchSweepLoop_ReturnsOnTheHandoverVerdict(t *testing.T) {
	f := newArmFixture(t, true)
	sick := &announcedWatchReads{Store: f.ws, reads: make(chan struct{}, 8), err: unreachableWatchStore()}
	f.coord.wake = make(chan struct{}, 1)
	f.coord.setRuntime(f.srv.runs, sick, f.srv.cfg.Bots.Paths)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// K = 1: one unreachable pass ends this term.
	health := &lease.Health{Passes: 1}
	done := make(chan struct{})
	go func() { defer close(done); f.coord.sweepLoop(ctx, nil, health) }()
	awaitWatchSignal(t, sick.reads)
	select {
	case f.coord.wake <- struct{}{}:
	default:
	}
	select {
	case <-done:
	case <-sick.reads:
		cancel()
		awaitWatchSignal(t, done)
		t.Fatal("the loop swept again instead of returning on the handover verdict: the term never ends, so the lease is never released and no peer covers the net")
	case <-time.After(10 * time.Second):
		cancel()
		awaitWatchSignal(t, done)
		t.Fatal("the loop neither returned nor swept again")
	}
	// The way out was the VERDICT, not a cancellation: the term is still
	// live, and the policy recorded exactly one handover.
	if err := ctx.Err(); err != nil {
		t.Fatalf("the term's context ended (%v) — this witness proved a cancellation, not a verdict", err)
	}
	if snap := health.Snapshot(); snap.Handovers != 1 || snap.FailingPasses != 1 {
		t.Fatalf("handover state = %+v, want one handover after one unreachable pass", snap)
	}
}

// The mirror, so the witness above cannot pass by the loop merely ending: a
// pass that found nothing to do is the healthiest state this net has, and it
// must leave the loop running. A sweep loop that returned on any pass would
// release the lease at the first quiet tick.
func TestAssistantWatchSweepLoop_AnIdlePassKeepsTheTerm(t *testing.T) {
	f := newArmFixture(t, true)
	idle := &announcedWatchReads{Store: f.ws, reads: make(chan struct{}, 8)}
	f.coord.wake = make(chan struct{}, 1)
	f.coord.setRuntime(f.srv.runs, idle, f.srv.cfg.Bots.Paths)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	health := &lease.Health{Passes: 1}
	done := make(chan struct{})
	go func() { defer close(done); f.coord.sweepLoop(ctx, nil, health) }()
	awaitWatchSignal(t, idle.reads)
	// Woken the same way the sick loop is: the second pass proves the loop
	// is still serving, rather than a budget proving nothing happened.
	select {
	case f.coord.wake <- struct{}{}:
	default:
	}
	awaitWatchSignal(t, idle.reads)
	select {
	case <-done:
		t.Fatal("the loop returned on an idle pass: a net whose passes find nothing to do would release its lease at the first quiet tick")
	default:
	}
	if snap := health.Snapshot(); snap.Handovers != 0 || snap.FailingPasses != 0 {
		t.Fatalf("handover state = %+v, want nothing counted against an idle pass", snap)
	}
	// Only the term ending stops it.
	cancel()
	awaitWatchSignal(t, done)
}
