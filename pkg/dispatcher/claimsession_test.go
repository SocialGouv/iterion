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

// TestClaimSession_StopIsNotHostageToASlowRenewal: Stop() runs ON THE
// ACTOR (finishRun's park arms, and shutdown). If it waits for a renewal
// against a slow store to finish, the whole dispatcher stalls for that
// renewal's full timeout — measured at three seconds, and ADR-028's
// "the actor never blocks on tracker I/O" says it must not.
func TestClaimSession_StopIsNotHostageToASlowRenewal(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	leaser := &slowLeaser{entered: entered, release: release}
	// Built (not StartClaimSession'd) so the cadence is set BEFORE the
	// goroutine reads it — the sibling tests' pattern.
	s := &claimSession{
		issueID: "issue-1", tok: tracker.ClaimToken{Marker: "m", Epoch: 1},
		leaser: leaser, warn: func(string, ...any) {}, interval: 5 * time.Millisecond,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go s.loop()
	defer close(release)

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the heartbeat never issued a renewal")
	}
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		s.Stop()
		done <- time.Since(start)
	}()
	select {
	case took := <-done:
		if took > time.Second {
			t.Fatalf("Stop() waited %s on an in-flight renewal — the actor is blocked for that long", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() is hostage to an in-flight renewal: the actor cannot make progress until the store answers")
	}
}

// slowLeaser blocks inside RenewClaim until released, and reports whether
// the context it was handed got cancelled — the channel the session must
// use to cut a renewal short.
type slowLeaser struct {
	entered chan struct{}
	release chan struct{}
}

func (l *slowLeaser) ClaimLease(context.Context, string, string) (tracker.ClaimToken, error) {
	return tracker.ClaimToken{}, nil
}
func (l *slowLeaser) RenewClaim(ctx context.Context, _ string, _ tracker.ClaimToken) error {
	select {
	case l.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.release:
		return nil
	}
}
func (l *slowLeaser) ReleaseOwned(context.Context, string, tracker.ClaimToken) error { return nil }
func (l *slowLeaser) UpdateStateOwned(context.Context, string, string, tracker.ClaimToken) error {
	return nil
}

// TestClaimLost_DoesNotCancelTheNextRun: cmdClaimLost is queued and
// applied later, and the card becomes re-claimable the instant the
// previous claim goes — the finish worker releases BEFORE stopping the
// heartbeat, so a beat blocked behind that release returns
// ErrClaimConflict and fires the loss for a run that has already ended.
// Matching on the issue id alone then cancelled whatever run held the
// card by the time the message landed: an innocent successor killed with
// ErrRunInterrupted.
func TestClaimLost_DoesNotCancelTheNextRun(t *testing.T) {
	c := &Dispatcher{state: newState(), logger: quietLogger()}
	var cancelled []error
	c.state.running["i1"] = &runningEntry{
		IssueID: "i1", Identifier: "i1", RunID: "run-NEW",
		Cancel: func(err error) { cancelled = append(cancelled, err) },
	}

	// The loss belongs to the PREVIOUS run for this card.
	cmdClaimLost{issueID: "i1", runID: "run-OLD"}.apply(c, context.Background())

	if len(cancelled) != 0 {
		t.Fatalf("the successor run was cancelled by its predecessor's lost claim: %v", cancelled)
	}
	// The same message for the run that actually lost its claim still acts.
	cmdClaimLost{issueID: "i1", runID: "run-NEW"}.apply(c, context.Background())
	if len(cancelled) != 1 {
		t.Fatalf("a genuine claim loss must still cancel its own run: %v", cancelled)
	}
}

// TestClaimSession_OwnReleaseIsNotASupersession: the finish worker
// releases the claim and only then stops the heartbeat (stopping earlier
// would let the lease lapse while it is still writing). A beat in flight
// across that release comes back ErrClaimConflict — our OWN release, not
// another owner. Reporting it warned "claim lost — stopping the worker"
// on ordinary finishes and fired the cancel path for a run already over.
func TestClaimSession_OwnReleaseIsNotASupersession(t *testing.T) {
	fl := &fakeLeaser{renewErrs: []error{tracker.ErrClaimConflict}}
	var lost, warns atomic.Int32
	s := &claimSession{
		issueID: "i1", tok: tracker.ClaimToken{Marker: "m", Epoch: 1},
		leaser: fl, warn: func(string, ...any) { warns.Add(1) },
		interval: 5 * time.Millisecond,
		onLost:   func(error) { lost.Add(1) },
		stop:     make(chan struct{}), done: make(chan struct{}),
	}
	s.Releasing() // the owner is dropping this claim itself
	go s.loop()
	<-s.done // the loop still exits on the conflict — quietly

	if got := lost.Load(); got != 0 {
		t.Fatalf("onLost fired %d time(s) for our own release — the cancel path runs for a run already over", got)
	}
	if got := warns.Load(); got != 0 {
		t.Fatalf("warned %d time(s) about a claim we released ourselves — an alarming line on every ordinary finish that straddles a beat", got)
	}
	if s.Lost() {
		t.Fatal("our own release must not latch the loss flag")
	}
}
