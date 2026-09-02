package dispatcher

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// fakeLeaser is a scriptable tracker.ClaimLeaser: renewErrs are consumed
// one per RenewClaim call (nil = success), then the last entry repeats.
type fakeLeaser struct {
	mu        sync.Mutex
	renews    int
	renewErrs []error
}

func (f *fakeLeaser) ClaimLease(context.Context, string, string) (tracker.ClaimToken, error) {
	return tracker.ClaimToken{Marker: "m", Epoch: 1}, nil
}
func (f *fakeLeaser) RenewClaim(_ context.Context, _ string, _ tracker.ClaimToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews++
	if len(f.renewErrs) == 0 {
		return nil
	}
	err := f.renewErrs[0]
	if len(f.renewErrs) > 1 {
		f.renewErrs = f.renewErrs[1:]
	}
	return err
}
func (f *fakeLeaser) ReleaseOwned(context.Context, string, tracker.ClaimToken) error { return nil }
func (f *fakeLeaser) UpdateStateOwned(context.Context, string, string, tracker.ClaimToken) error {
	return nil
}
func (f *fakeLeaser) renewCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renews
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestClaimSession_HeartbeatsUnconditionally: the ticker renews with no
// run events at all — the exact gap the event-driven heartbeat left (a
// silent long model call read as death).
func TestClaimSession_HeartbeatsUnconditionally(t *testing.T) {
	fl := &fakeLeaser{}
	s := &claimSession{
		issueID: "i1", tok: tracker.ClaimToken{Marker: "m", Epoch: 1},
		leaser: fl, warn: t.Logf, interval: 10 * time.Millisecond,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go s.loop()
	waitFor(t, "3 renewals", func() bool { return fl.renewCount() >= 3 })
	if s.Lost() {
		t.Fatal("healthy renewals must not read as loss")
	}
	s.Stop()
	after := fl.renewCount()
	time.Sleep(30 * time.Millisecond)
	if fl.renewCount() != after {
		t.Fatal("Stop must end the heartbeat")
	}
}

// TestClaimSession_ConflictLatchesLossOnce: the first ErrClaimConflict
// calls onLost exactly once, latches Lost, and stops renewing; transient
// errors before it neither latch nor stop.
func TestClaimSession_ConflictLatchesLossOnce(t *testing.T) {
	fl := &fakeLeaser{renewErrs: []error{nil, context.DeadlineExceeded, tracker.ErrClaimConflict}}
	var lost atomic.Int32
	s := &claimSession{
		issueID: "i1", tok: tracker.ClaimToken{Marker: "m", Epoch: 1},
		leaser: fl, warn: t.Logf, interval: 5 * time.Millisecond,
		onLost: func(error) { lost.Add(1) },
		stop:   make(chan struct{}), done: make(chan struct{}),
	}
	go s.loop()
	waitFor(t, "loss latched", s.Lost)
	<-s.done // the loop must have exited on its own
	if got := lost.Load(); got != 1 {
		t.Fatalf("onLost calls = %d, want exactly 1", got)
	}
	if fl.renewCount() != 3 {
		t.Fatalf("renewals after loss = %d, want none past the conflict (3 total)", fl.renewCount())
	}
	s.Stop() // idempotent after self-exit
}
