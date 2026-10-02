package lease

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// until polls pred until it holds or the budget runs out. Waits in this file
// are on conditions; the lease durations pace the campaigns. Where a test
// asserts on elapsed time, the margin it leaves a loaded scheduler is stated
// beside its TTL.
func until(t *testing.T, budget time.Duration, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", budget, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// logSink captures a campaign's Info and Warn lines so a test can assert WHY
// a term ended, not only that it did.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) add(level string) func(string, ...any) {
	return func(format string, args ...any) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
	}
}

func (l *logSink) has(substr string) bool { return l.count(substr) > 0 }

func (l *logSink) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func (l *logSink) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func spec(owner string, ttl time.Duration, log *logSink) Spec {
	s := Spec{Name: "sweeper", Owner: owner, TTL: ttl}
	if log != nil {
		s.Info, s.Warn = log.add("INFO"), log.add("WARN")
	}
	return s
}

// terms records who is serving: how many terms run at once (the invariant is
// at most one) and which owners ever served.
type terms struct {
	active, maxActive atomic.Int32
	mu                sync.Mutex
	owners            []string
}

func (tr *terms) serve(owner string) func(context.Context) {
	return func(ctx context.Context) {
		n := tr.active.Add(1)
		for {
			m := tr.maxActive.Load()
			if n <= m || tr.maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		tr.mu.Lock()
		tr.owners = append(tr.owners, owner)
		tr.mu.Unlock()
		<-ctx.Done()
		tr.active.Add(-1)
	}
}

func (tr *terms) servedBy() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.owners...)
}

// stopClock records when a term's context ended.
type stopClock struct{ at atomic.Int64 }

func (c *stopClock) serve(started chan<- struct{}) func(context.Context) {
	var once sync.Once
	return func(ctx context.Context) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		c.at.CompareAndSwap(0, time.Now().UnixNano())
	}
}

func (c *stopClock) stopped() time.Time { return time.Unix(0, c.at.Load()) }

// The point of the package: N replicas campaign, ONE serves, and it keeps
// serving — renewals hold the lease across several TTLs, so the term never
// changes hands while its holder is healthy.
func TestRun_OneCandidateServesAndKeepsServing(t *testing.T) {
	st := NewMemoryStore()
	var tr terms
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	const candidates = 5
	// Renewal every 500ms against a 1.25s step-down: a scheduler stall under
	// a loaded -race run has to exceed 750ms to end the term.
	const leaseTTL = 1500 * time.Millisecond
	for i := 0; i < candidates; i++ {
		owner := fmt.Sprintf("replica-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Run(ctx, st, spec(owner, leaseTTL, nil), tr.serve(owner)); err != nil {
				t.Errorf("Run(%s): %v", owner, err)
			}
		}()
	}
	until(t, 10*time.Second, "a candidate to be elected", func() bool { return tr.active.Load() == 1 })
	// Hold across several TTLs: a holder that did not renew would lose the
	// lease (or step down) well within this window.
	time.Sleep(3 * leaseTTL)
	cancel()
	wg.Wait()

	if got := tr.maxActive.Load(); got != 1 {
		t.Errorf("%d terms ran at once, want 1", got)
	}
	if served := tr.servedBy(); len(served) != 1 {
		t.Errorf("terms served by %v, want ONE uninterrupted term — the holder must renew its lease", served)
	}
	if n := tr.active.Load(); n != 0 {
		t.Errorf("%d terms still running after every campaign returned", n)
	}
}

// A rollout: the holder stops, releases, and a follower takes over at its next
// attempt — not after the TTL. The TTL here is far longer than the test's
// whole budget, so only the release can explain the hand-over.
func TestRun_AStoppingHolderHandsOverWithoutWaitingOutTheTTL(t *testing.T) {
	st := NewMemoryStore()
	var tr terms
	const leaseTTL = time.Hour
	aCtx, aStop := context.WithCancel(context.Background())
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		_ = Run(aCtx, st, spec("a", leaseTTL, nil), tr.serve("a"))
	}()
	until(t, 5*time.Second, "a to be elected", func() bool { return tr.active.Load() == 1 })

	bCtx, bStop := context.WithCancel(context.Background())
	defer bStop()
	bSpec := spec("b", leaseTTL, nil)
	bSpec.Retry = 10 * time.Millisecond
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		_ = Run(bCtx, st, bSpec, tr.serve("b"))
	}()

	aStop()
	<-aDone
	until(t, 5*time.Second, "b to take over after a released", func() bool {
		served := tr.servedBy()
		return len(served) == 2 && served[1] == "b" && tr.active.Load() == 1
	})
	bStop()
	<-bDone
	if ok, err := st.Acquire(context.Background(), "sweeper", "c", time.Now(), time.Minute); err != nil || !ok {
		t.Errorf("after every candidate stopped, the lease is still held (%v, %v) — a stopping holder must release", ok, err)
	}
}

// A term that ends in a panic still hands the lease on: the release is
// deferred, not a statement after fn.
func TestRun_APanickingTermStillReleases(t *testing.T) {
	st := NewMemoryStore()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		_ = Run(context.Background(), st, spec("a", time.Hour, nil), func(context.Context) { panic("boom") })
	}()
	if p := <-done; p == nil {
		t.Fatal("Run returned without the panic")
	}
	if ok, err := st.Acquire(context.Background(), "sweeper", "b", time.Now(), time.Minute); err != nil || !ok {
		t.Errorf("after a's term panicked, b cannot take the lease (%v, %v) — the panic path did not release", ok, err)
	}
}

// Another owner took the lease (the holder overran and was outlived). The
// holder must stop at its next renewal because the store SAID so — not later,
// when its own step-down timer runs out.
func TestRun_ALeaseTakenByAnotherOwnerStopsTheHolder(t *testing.T) {
	st := NewMemoryStore()
	log := &logSink{}
	var tr terms
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Renewal every 1s, step-down 2.5s after the last one: the refused renewal
	// arrives at least 0.5s before the timer could, whatever the steal's phase.
	const leaseTTL = 3 * time.Second
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, st, spec("a", leaseTTL, log), tr.serve("a"))
	}()
	until(t, 5*time.Second, "a to be elected", func() bool { return tr.active.Load() == 1 })

	// The thief presents an instant past a's expiry, which is exactly what a
	// successor sees after a holder stalled beyond its TTL.
	if ok, err := st.Acquire(context.Background(), "sweeper", "thief", time.Now().Add(2*leaseTTL), time.Hour); err != nil || !ok {
		t.Fatalf("thief Acquire = %v, %v", ok, err)
	}
	until(t, 5*time.Second, "a to stop serving", func() bool { return tr.active.Load() == 0 })
	if !log.has("lost the lease to another owner") {
		t.Errorf("a stopped without the store refusing its renewal; log: %v", log.all())
	}
	if log.has("stepping down — no renewal it can prove") {
		t.Errorf("a stopped on its step-down timer instead of on the refused renewal; log: %v", log.all())
	}
	if log.has("had already lost the lease") {
		t.Errorf("the loss was logged twice; log: %v", log.all())
	}
	cancel()
	<-done
}

// flakyStore grants the first Acquire, then fails every renewal and every
// later call — a holder that loses its store mid-term.
type flakyStore struct {
	Store
	mu        sync.Mutex
	acquires  int
	firstSent time.Time
}

func (f *flakyStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	f.acquires++
	first := f.acquires == 1
	if first {
		f.firstSent = now
	}
	f.mu.Unlock()
	if first {
		return f.Store.Acquire(ctx, name, owner, now, ttl)
	}
	return false, errors.New("store unreachable")
}

func (f *flakyStore) Renew(context.Context, string, string, time.Time, time.Duration) (bool, error) {
	return false, errors.New("store unreachable")
}

func (f *flakyStore) sent() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.firstSent
}

// A holder that cannot renew must stop BEFORE its lease can expire in the
// store — otherwise it keeps sweeping while a successor, seeing the lease
// expired, starts sweeping too — and it must keep the TTL/6 margin that
// absorbs clock skew between the replicas, not merely stop "in time".
func TestRun_AHolderThatCannotRenewStepsDownWithTheSkewMarginLeft(t *testing.T) {
	fs := &flakyStore{Store: NewMemoryStore()}
	log := &logSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Step-down at 5s, expiry at 6s; the assertion at 5.5s leaves 500ms either
	// side — a margin shrunk to TTL/60 would stop at 5.9s.
	const leaseTTL = 6 * time.Second
	var clock stopClock
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, fs, spec("a", leaseTTL, log), clock.serve(started))
	}()
	<-started
	until(t, 10*time.Second, "a to step down", func() bool { return clock.at.Load() != 0 })
	if after := clock.stopped().Sub(fs.sent()); after > leaseTTL-leaseTTL/12 {
		t.Errorf("a stopped %s after the acquire was sent: its lease expires at %s and less than half the TTL/6 skew margin was left",
			after, leaseTTL)
	}
	if !log.has("stepping down — no renewal it can prove") {
		t.Errorf("no step-down line; log: %v", log.all())
	}
	cancel()
	<-done
}

// slowStore answers late while its write lands at once, stamped with the
// instant the caller sent it — what a slow round-trip to Mongo looks like.
// grantDelay slows the acquire. The first okRenewals renewals succeed, each
// answered renewDelay late; every renewal after them fails, so the term's end
// is decided by the step-down timer counted from the last stamp.
type slowStore struct {
	Store
	grantDelay, renewDelay time.Duration
	okRenewals             int
	mu                     sync.Mutex
	grantSent, renewSent   time.Time
	renewals               int
}

func (f *slowStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	ok, err := f.Store.Acquire(ctx, name, owner, now, ttl)
	f.mu.Lock()
	if f.grantSent.IsZero() {
		f.grantSent = now
	}
	f.mu.Unlock()
	time.Sleep(f.grantDelay)
	return ok, err
}

func (f *slowStore) Renew(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	f.renewals++
	n := f.renewals
	if n <= f.okRenewals {
		f.renewSent = now
	}
	f.mu.Unlock()
	if n > f.okRenewals {
		return false, errors.New("store unreachable")
	}
	ok, err := f.Store.Renew(ctx, name, owner, now, ttl)
	time.Sleep(f.renewDelay)
	return ok, err
}

// The step-down counts from when the call that stamped the lease was SENT: a
// slow answer does not buy the holder time the store never gave it.
func TestRun_TheStepDownCountsFromWhenTheCallWasSent(t *testing.T) {
	// hold = 5s; a count started at the send stops at 5s, one started at the
	// answer at 5.9s; the assertion at 5.5s leaves 500ms either side.
	const leaseTTL, slow = 6 * time.Second, 900 * time.Millisecond
	for _, c := range []struct {
		name  string
		store *slowStore
		sent  func(*slowStore) time.Time
	}{
		{"slow grant", &slowStore{Store: NewMemoryStore(), grantDelay: slow}, func(f *slowStore) time.Time { return f.grantSent }},
		{"slow renewal", &slowStore{Store: NewMemoryStore(), renewDelay: slow, okRenewals: 1}, func(f *slowStore) time.Time { return f.renewSent }},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var clock stopClock
			started := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = Run(ctx, c.store, spec("a", leaseTTL, nil), clock.serve(started))
			}()
			<-started
			until(t, 15*time.Second, "a to step down", func() bool { return clock.at.Load() != 0 })
			c.store.mu.Lock()
			expiry := c.sent(c.store).Add(leaseTTL)
			c.store.mu.Unlock()
			if stopped := clock.stopped(); !stopped.Before(expiry.Add(-leaseTTL / 12)) {
				t.Errorf("a stopped %s before its lease's expiry in the store — want at least %s: the deadline was counted from the answer, not the send",
					expiry.Sub(stopped), leaseTTL/12)
			}
			cancel()
			<-done
		})
	}
}

// blipStore fails exactly one renewal, then behaves.
type blipStore struct {
	Store
	renewals atomic.Int32
}

func (f *blipStore) Renew(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if f.renewals.Add(1) == 1 {
		return false, errors.New("store blip")
	}
	return f.Store.Renew(ctx, name, owner, now, ttl)
}

// A renewal the store could not answer is not a lost lease: the holder keeps
// its term through a blip shorter than the step-down, and a new term — for
// the gate sweep, a new deep pass over days of runs — is not paid for nothing.
func TestRun_ASingleFailedRenewalKeepsTheTerm(t *testing.T) {
	fs := &blipStore{Store: NewMemoryStore()}
	log := &logSink{}
	var tr terms
	ctx, cancel := context.WithCancel(context.Background())
	const leaseTTL = 3 * time.Second // renew every 1s, hold 2.5s: 500ms of slack for the renewal after the blip
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, fs, spec("a", leaseTTL, log), tr.serve("a"))
	}()
	until(t, 5*time.Second, "a elected", func() bool { return tr.active.Load() == 1 })
	until(t, 10*time.Second, "the blip", func() bool { return log.has("could not renew") })
	time.Sleep(leaseTTL) // past the step-down the blip would have caused without the next renewal
	cancel()
	<-done
	if served := tr.servedBy(); len(served) != 1 || log.has("stepping down — no renewal") {
		t.Errorf("one failed renewal ended the term: terms %v; log %v", served, log.all())
	}
}

// lostAnswerStore lands every Acquire and loses its answer: the shape of a
// write held up in the store past its caller's timeout, or cut by a stop.
type lostAnswerStore struct {
	Store
	attempts atomic.Int32
}

func (f *lostAnswerStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	_, _ = f.Store.Acquire(ctx, name, owner, now, ttl)
	f.attempts.Add(1)
	return false, errors.New("the answer was lost")
}

// An attempt whose answer was lost may have taken the lease. The campaign
// gives it back, or a candidate that cannot hear the store holds the lease
// without serving and locks every healthy candidate out for the TTL.
func TestRun_AnUnansweredAttemptGivesBackWhatItMayHaveTaken(t *testing.T) {
	inner := NewMemoryStore()
	sick := &lostAnswerStore{Store: inner}
	ctx, cancel := context.WithCancel(context.Background())
	sickDone := make(chan struct{})
	var sickServed atomic.Bool
	go func() {
		defer close(sickDone)
		_ = Run(ctx, sick, spec("sick", time.Hour, nil), func(context.Context) { sickServed.Store(true) })
	}()
	// The sick candidate's attempt lands, its answer does not, and its next
	// attempt is twenty minutes away: the healthy candidate can only serve
	// within the test if the lease the lost attempt took was given back.
	until(t, 5*time.Second, "the sick candidate's attempt", func() bool { return sick.attempts.Load() >= 1 })
	var tr terms
	healthy := spec("healthy", time.Hour, nil)
	healthy.Retry = 10 * time.Millisecond
	hDone := make(chan struct{})
	go func() {
		defer close(hDone)
		_ = Run(ctx, inner, healthy, tr.serve("healthy"))
	}()
	until(t, 5*time.Second, "the healthy candidate to serve", func() bool { return tr.active.Load() == 1 })
	cancel()
	<-sickDone
	<-hDone
	if sickServed.Load() {
		t.Error("the sick candidate served without an answer from the store")
	}
}

// A campaign stopped while its attempt is in flight gives back what that
// attempt may have taken, so a clean stop never leaves the lease to a
// process that is gone.
func TestRun_AStopDuringAnAttemptLeavesNoLease(t *testing.T) {
	inner := NewMemoryStore()
	entered := make(chan struct{})
	blocking := &blockingAcquireStore{Store: inner, entered: entered}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, blocking, spec("c", time.Hour, nil), func(context.Context) {})
	}()
	<-entered // the attempt's write has landed; its answer is pending
	cancel()
	<-done
	if ok, err := inner.Acquire(context.Background(), "sweeper", "d", time.Now(), time.Minute); err != nil || !ok {
		t.Errorf("after c's clean stop, the lease is still held (%v, %v): a successor waits out the TTL", ok, err)
	}
}

// blockingAcquireStore lands the first Acquire, then holds its answer until
// the caller gives up.
type blockingAcquireStore struct {
	Store
	entered chan struct{}
	once    sync.Once
}

func (f *blockingAcquireStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if _, err := f.Store.Acquire(ctx, name, owner, now, ttl); err != nil {
		return false, err
	}
	f.once.Do(func() { close(f.entered) })
	<-ctx.Done()
	return false, ctx.Err()
}

// The step-down comes before its log line: a slow log sink must not keep a
// holder serving past its lease.
func TestRun_TheStepDownDoesNotWaitForTheLog(t *testing.T) {
	fs := &flakyStore{Store: NewMemoryStore()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const leaseTTL = 6 * time.Second // step-down at 5s, expiry at 6s: 1s of margin
	sp := spec("a", leaseTTL, nil)
	sp.Warn = func(format string, args ...any) {
		if strings.Contains(fmt.Sprintf(format, args...), "stepping down") {
			time.Sleep(leaseTTL) // a log pipe that stalls
		}
	}
	var clock stopClock
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, fs, sp, clock.serve(started))
	}()
	<-started
	until(t, 15*time.Second, "a to step down", func() bool { return clock.at.Load() != 0 })
	if expiry := fs.sent().Add(leaseTTL); !clock.stopped().Before(expiry) {
		t.Errorf("a stopped %s after its lease expired: the step-down waited for its log line", clock.stopped().Sub(expiry))
	}
	cancel()
	<-done
}

// slowAnswerStore answers every call after delay.
type slowAnswerStore struct {
	Store
	delay time.Duration
}

func (f *slowAnswerStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return f.Store.Acquire(ctx, name, owner, now, ttl)
}

// Retry paces the attempts; it does not bound them. A retry shorter than the
// store's round-trip must still elect — bounded by the retry, every attempt
// would time out, however healthy the store.
func TestRun_ARetryShorterThanTheStoresAnswerStillElects(t *testing.T) {
	st := &slowAnswerStore{Store: NewMemoryStore(), delay: 50 * time.Millisecond}
	var tr terms
	ctx, cancel := context.WithCancel(context.Background())
	sp := spec("a", 3*time.Second, nil)
	sp.Retry = time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, st, sp, tr.serve("a"))
	}()
	until(t, 5*time.Second, "a to be elected on a slow but healthy store", func() bool { return tr.active.Load() == 1 })
	cancel()
	<-done
}

// stepDownAt must fall TTL/6 before the expiry the store actually records —
// which, kept to the millisecond, can be up to two milliseconds short of
// sent+TTL. Counted from sent+TTL instead, a short lease would step down after
// its successor could start. And it keeps the monotonic reading of the instant
// it was given, so a wall-clock step cannot move the holder's deadline.
func TestStepDownAt_KeepsTheMarginToTheRecordedExpiry(t *testing.T) {
	c := &campaign{spec: Spec{Name: "sweeper", Owner: "a", TTL: time.Minute}}
	if got := c.stepDownAt(time.Now()); !strings.Contains(got.String(), " m=") {
		t.Errorf("stepDownAt dropped the monotonic reading of the send instant: %s", got)
	}
	for _, ttl := range []time.Duration{
		3 * time.Millisecond, 3*time.Millisecond + 700*time.Microsecond,
		time.Second, time.Second + 300*time.Microsecond, 3 * time.Minute,
	} {
		c := &campaign{spec: Spec{Name: "sweeper", Owner: "a", TTL: ttl}}
		for _, offset := range []time.Duration{0, 300 * time.Microsecond, 900 * time.Microsecond, 999 * time.Microsecond} {
			sent := t0.Add(offset)
			st := NewMemoryStore()
			if ok, err := st.Acquire(context.Background(), "sweeper", "a", sent, ttl); err != nil || !ok {
				t.Fatal(ok, err)
			}
			recorded := st.leases["sweeper"].expiresAt
			if got, want := c.stepDownAt(sent), recorded.Add(-ttl/6); got.After(want) {
				t.Errorf("TTL %s, sent at +%s: step-down at +%s, after the recorded expiry minus TTL/6 (+%s)",
					ttl, offset, got.Sub(t0), want.Sub(t0))
			}
		}
	}
}

// replayStore re-issues, on demand, the last call the campaign sent after its
// grant — exactly as sent — the way a write held up in the store lands late.
type replayStore struct {
	Store
	mu      sync.Mutex
	granted bool
	replay  func()
}

func (f *replayStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	ok, err := f.Store.Acquire(ctx, name, owner, now, ttl)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.granted {
		f.replay = func() { _, _ = f.Store.Acquire(context.Background(), name, owner, now, ttl) }
	}
	if ok {
		f.granted = true
	}
	return ok, err
}

func (f *replayStore) Renew(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	f.replay = func() { _, _ = f.Store.Renew(context.Background(), name, owner, now, ttl) }
	f.mu.Unlock()
	return f.Store.Renew(ctx, name, owner, now, ttl)
}

// The holder's last renewal lands after its term released the lease (on Mongo,
// behind a lock, a slow disk, a failover). It must leave the lease free — a
// renewal that could create the lease would hand it to a process that is gone.
func TestRun_ARenewalLandingAfterTheReleaseLeavesTheLeaseFree(t *testing.T) {
	inner := NewMemoryStore()
	f := &replayStore{Store: inner}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, f, spec("a", 300*time.Millisecond, nil), func(c context.Context) { <-c.Done() })
	}()
	until(t, 5*time.Second, "a renewal", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.replay != nil })
	cancel()
	<-done
	f.mu.Lock()
	replay := f.replay
	f.mu.Unlock()
	replay()
	if ok, err := inner.Acquire(context.Background(), "sweeper", "b", time.Now(), time.Minute); err != nil || !ok {
		t.Errorf("a renewal landing after the release re-created the lease (%v, %v)", ok, err)
	}
}

// hangOnceStore never answers its first Acquire (and lands nothing), then
// behaves.
type hangOnceStore struct {
	Store
	calls atomic.Int32
}

func (f *hangOnceStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if f.calls.Add(1) == 1 {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return f.Store.Acquire(ctx, name, owner, now, ttl)
}

// An attempt the store never answers is abandoned at its bound (TTL/3), so the
// campaign tries again; unbounded, one hung call would keep the candidate out
// of the election forever.
func TestRun_AnAttemptTheStoreNeverAnswersIsAbandoned(t *testing.T) {
	st := &hangOnceStore{Store: NewMemoryStore()}
	var tr terms
	ctx, cancel := context.WithCancel(context.Background())
	sp := spec("a", 300*time.Millisecond, nil) // attempts bounded at 100ms
	sp.Retry = 10 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, st, sp, tr.serve("a"))
	}()
	until(t, 5*time.Second, "a to be elected after its first attempt hung", func() bool { return tr.active.Load() == 1 })
	cancel()
	<-done
}

// renewSignalStore reports each successful renewal.
type renewSignalStore struct {
	Store
	renewed chan struct{}
}

func (f *renewSignalStore) Renew(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	ok, err := f.Store.Renew(ctx, name, owner, now, ttl)
	if ok && err == nil {
		select {
		case f.renewed <- struct{}{}:
		default:
		}
	}
	return ok, err
}

// The refused renewal stops the holder BEFORE its log line: a slow log sink
// must not keep a deposed holder serving beside its successor.
func TestRun_TheLossStopsTheHolderBeforeItsLogLine(t *testing.T) {
	inner := NewMemoryStore()
	st := &renewSignalStore{Store: inner, renewed: make(chan struct{}, 1)}
	const leaseTTL = 3 * time.Second // renewals 1s apart; step-down 2.5s after the last
	sp := spec("a", leaseTTL, nil)
	sp.Warn = func(format string, args ...any) {
		if strings.Contains(fmt.Sprintf(format, args...), "lost the lease") {
			time.Sleep(leaseTTL) // a log pipe that stalls
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var clock stopClock
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, st, sp, clock.serve(started))
	}()
	<-started
	<-st.renewed // steal right after a renewal: the next one, ~1s away, is refused
	stolen := time.Now()
	if ok, err := inner.Acquire(context.Background(), "sweeper", "thief", time.Now().Add(2*leaseTTL), time.Hour); err != nil || !ok {
		t.Fatalf("thief Acquire = %v, %v", ok, err)
	}
	until(t, 10*time.Second, "a to stop", func() bool { return clock.at.Load() != 0 })
	// Refusal ~1s after the steal; a log-first holder would stop only at its
	// step-down, ~2.5s after it.
	if after := clock.stopped().Sub(stolen); after > 1750*time.Millisecond {
		t.Errorf("a stopped %s after losing its lease: the stop waited for the log line", after)
	}
	cancel()
	<-done
}

type downStore struct{}

func (downStore) Acquire(context.Context, string, string, time.Time, time.Duration) (bool, error) {
	return false, errors.New("store unreachable")
}
func (downStore) Renew(context.Context, string, string, time.Time, time.Duration) (bool, error) {
	return false, errors.New("store unreachable")
}
func (downStore) Release(context.Context, string, string) error { return nil }

// A store that cannot answer elects nobody. Running anyway would put the work
// back on every replica — the very load the election exists to remove — so
// the campaign waits, says so once, and keeps trying.
func TestRun_AnUnreachableStoreElectsNobodyAndSaysSoOnce(t *testing.T) {
	log := &logSink{}
	var served atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	sp := spec("a", 90*time.Millisecond, log)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, downStore{}, sp, func(context.Context) { served.Store(true) })
	}()
	until(t, 5*time.Second, "a failed attempt", func() bool { return log.count("cannot reach the lease store") >= 1 })
	time.Sleep(10 * sp.TTL / 3) // ~10 more attempts
	cancel()
	<-done
	if served.Load() {
		t.Error("the work ran without a lease the store granted")
	}
	if n := log.count("cannot reach the lease store"); n != 1 {
		t.Errorf("the outage was reported %d times, want once (edge-triggered)", n)
	}
}

// togglingStore fails while down is set.
type togglingStore struct {
	Store
	down atomic.Bool
}

func (f *togglingStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if f.down.Load() {
		return false, errors.New("store unreachable")
	}
	return f.Store.Acquire(ctx, name, owner, now, ttl)
}

// Edge-triggered means once PER EPISODE: a second outage is news again, and
// so is each recovery.
func TestRun_EachOutageAndEachRecoveryIsReported(t *testing.T) {
	inner := NewMemoryStore()
	// Another owner holds the lease: the campaign is answered "no", never elected.
	if ok, err := inner.Acquire(context.Background(), "sweeper", "holder", time.Now(), time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	fs := &togglingStore{Store: inner}
	log := &logSink{}
	ctx, cancel := context.WithCancel(context.Background())
	sp := spec("a", 90*time.Millisecond, log)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, fs, sp, func(context.Context) {})
	}()
	for episode := 1; episode <= 2; episode++ {
		fs.down.Store(true)
		until(t, 5*time.Second, fmt.Sprintf("outage %d reported", episode), func() bool { return log.count("cannot reach the lease store") == episode })
		fs.down.Store(false)
		until(t, 5*time.Second, fmt.Sprintf("recovery %d reported", episode), func() bool { return log.count("reaches the lease store again") == episode })
	}
	cancel()
	<-done
}

func TestRun_RejectsAnUnusableSpec(t *testing.T) {
	ctx := context.Background()
	noop := func(context.Context) {}
	for name, c := range map[string]struct {
		st Store
		sp Spec
	}{
		"nil store":               {nil, spec("a", time.Second, nil)},
		"no lease name":           {NewMemoryStore(), Spec{Owner: "a", TTL: time.Second}},
		"no owner":                {NewMemoryStore(), Spec{Name: "sweeper", TTL: time.Second}},
		"TTL under a millisecond": {NewMemoryStore(), Spec{Name: "sweeper", Owner: "a", TTL: 2}},
	} {
		if err := Run(ctx, c.st, c.sp, noop); err == nil {
			t.Errorf("%s: Run returned nil, want an error", name)
		}
	}
}
