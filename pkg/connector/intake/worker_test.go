package intake

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/schedgate"
)

// fakeClock is the worker's clock, moved by the test.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// stubMatcher answers a fixed verdict list, or an error.
type stubMatcher struct {
	mu       sync.Mutex
	verdicts []Verdict
	err      error
	calls    int
}

func (m *stubMatcher) Match(context.Context, Intake) ([]Verdict, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return append([]Verdict(nil), m.verdicts...), m.err
}

// idempotentSubmitter models the launch path's contract: one run per key,
// and a repeated key answered with that run. fail, when set, decides the
// error of a call before any run exists for its key.
type idempotentSubmitter struct {
	mu   sync.Mutex
	runs map[string]string
	keys []string
	fail func(key string, call int) error
	// lose, when it says so, loses the answer of a call that started or met
	// a run: the run exists, the caller hears an error.
	lose  func(call int) bool
	calls int
	// during runs inside Submit, before the answer — a seam for
	// interleavings.
	during func(key string)
}

func newSubmitter() *idempotentSubmitter { return &idempotentSubmitter{runs: map[string]string{}} }

func (s *idempotentSubmitter) Submit(_ context.Context, _ Intake, p Pair) (string, error) {
	key := p.SubmissionKey()
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.keys = append(s.keys, key)
	during := s.during
	s.mu.Unlock()
	if during != nil {
		during(key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if run, ok := s.runs[key]; ok {
		if s.lose != nil && s.lose(call) {
			return "", context.DeadlineExceeded
		}
		return run, nil
	}
	if s.fail != nil {
		if err := s.fail(key, call); err != nil {
			return "", err
		}
	}
	run := fmt.Sprintf("run-%d", len(s.runs)+1)
	s.runs[key] = run
	if s.lose != nil && s.lose(call) {
		return "", context.DeadlineExceeded
	}
	return run, nil
}

func (s *idempotentSubmitter) runCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.runs) }

func newWorker(s Store, m Matcher, sub Submitter, clock *fakeClock, owner string) *Worker {
	p := Policy{MaxAttempts: 3, Backoff: 10 * time.Second, BackoffMax: time.Minute, ClaimTTL: time.Minute}
	return &Worker{Store: s, Matcher: m, Submitter: sub, Owner: owner, Intakes: p, Pairs: p, Now: clock.Now}
}

func tick(t *testing.T, w *Worker) {
	t.Helper()
	if _, err := w.MatchTick(context.Background()); err != nil {
		t.Fatalf("MatchTick: %v", err)
	}
	if _, err := w.LaunchTick(context.Background()); err != nil {
		t.Fatalf("LaunchTick: %v", err)
	}
}

func onlyPair(t *testing.T, s Store, tenant string) Pair {
	t.Helper()
	pairs, err := s.ListPairs(context.Background(), tenant, PairQuery{}, 0)
	if err != nil || len(pairs) != 1 {
		t.Fatalf("pairs = (%v, %v), want exactly one", pairSubs(pairs), err)
	}
	return pairs[0]
}

// The pairs an intake gets never depend on the order the matcher judged its
// candidates in — each is keyed by (intake, subscription): built in both
// orders, the record is the same.
func TestWorkerWritesOnePairPerCandidateInAStableOrder(t *testing.T) {
	verdicts := []Verdict{
		{SubscriptionID: "s2", Matched: true},
		{SubscriptionID: "s1", Reason: "label filter"},
		{SubscriptionID: "s3", Matched: true, Refusal: "expression past its budget"},
		{SubscriptionID: "s0"},
	}
	reversed := make([]Verdict, len(verdicts))
	for i, v := range verdicts {
		reversed[len(verdicts)-1-i] = v
	}
	var records [][]Pair
	for _, order := range [][]Verdict{verdicts, reversed} {
		s := NewMemoryStore()
		clock := &fakeClock{now: t0}
		in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
		w := newWorker(s, &stubMatcher{verdicts: order}, newSubmitter(), clock, "w")
		if _, err := w.MatchTick(context.Background()); err != nil {
			t.Fatal(err)
		}
		cur, _ := s.GetIntake(context.Background(), "t1", in.ID)
		if cur.State != IntakeMatched || cur.MatchedAt == nil {
			t.Fatalf("intake after matching = %+v, want matched", cur)
		}
		pairs, _ := s.ListPairs(context.Background(), "t1", PairQuery{IntakeID: in.ID}, 0)
		for i := range pairs {
			pairs[i].UpdatedAt, pairs[i].CreatedAt = time.Time{}, time.Time{}
		}
		records = append(records, pairs)
	}
	if !reflect.DeepEqual(records[0], records[1]) {
		t.Fatalf("pairs depend on the verdict order:\n%+v\n%+v", records[0], records[1])
	}
	want := map[string][2]string{
		"s0": {PairNotMatched, "the subscription's filters did not match"},
		"s1": {PairNotMatched, "label filter"},
		"s2": {PairPending, ""},
		"s3": {PairRefused, "expression past its budget"},
	}
	for _, p := range records[0] {
		if got := [2]string{p.State, p.Reason}; got != want[p.SubscriptionID] {
			t.Errorf("pair %s = %v, want %v", p.SubscriptionID, got, want[p.SubscriptionID])
		}
	}
}

// Matching that keeps failing backs off, then lands in the dead letter; a
// replay matches it.
func TestWorkerMatchingRetriesThenBuriesThenReplays(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	clock := &fakeClock{now: t0}
	m := &stubMatcher{err: errors.New("subscription store unreachable")}
	w := newWorker(s, m, newSubmitter(), clock, "w")
	in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))

	tick(t, w)
	cur, _ := s.GetIntake(ctx, "t1", in.ID)
	if cur.State != IntakePending || cur.Attempts != 1 || !cur.NotBefore.Equal(stamp(t0.Add(10*time.Second))) || cur.LastError == "" {
		t.Fatalf("after one failure = %+v, want pending, one attempt, the first backoff", cur)
	}
	tick(t, w) // still backing off
	if m.calls != 1 {
		t.Fatalf("matched %d times inside the backoff", m.calls)
	}
	clock.Add(10 * time.Second)
	tick(t, w)
	cur, _ = s.GetIntake(ctx, "t1", in.ID)
	if cur.Attempts != 2 || !cur.NotBefore.Equal(stamp(clock.Now().Add(20*time.Second))) {
		t.Fatalf("after two failures = %+v, want the doubled backoff", cur)
	}
	clock.Add(20 * time.Second)
	tick(t, w)
	cur, _ = s.GetIntake(ctx, "t1", in.ID)
	if cur.State != IntakeDead || cur.Attempts != 3 {
		t.Fatalf("after the third failure = %+v, want the dead letter with its three attempts", cur)
	}

	m.mu.Lock()
	m.err, m.verdicts = nil, []Verdict{{SubscriptionID: "s1", Matched: true}}
	m.mu.Unlock()
	if _, err := s.ReplayIntake(ctx, "t1", in.ID, clock.Now()); err != nil {
		t.Fatal(err)
	}
	tick(t, w)
	if cur, _ = s.GetIntake(ctx, "t1", in.ID); cur.State != IntakeMatched {
		t.Fatalf("after the replay = %+v, want matched", cur)
	}
	if p := onlyPair(t, s, "t1"); p.State != PairLaunched {
		t.Fatalf("the replayed intake's pair = %+v, want launched", p)
	}
}

// A matcher answering one subscription twice, or none, fails the matching:
// no pair set can record it.
func TestWorkerRefusesAMalformedVerdictList(t *testing.T) {
	for name, vs := range map[string][]Verdict{
		"twice":           {{SubscriptionID: "s1", Matched: true}, {SubscriptionID: "s1"}},
		"no subscription": {{Matched: true}},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewMemoryStore()
			in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
			w := newWorker(s, &stubMatcher{verdicts: vs}, newSubmitter(), &fakeClock{now: t0}, "w")
			tick(t, w)
			cur, _ := s.GetIntake(context.Background(), "t1", in.ID)
			if cur.State != IntakePending || cur.Attempts != 1 {
				t.Fatalf("intake = %+v, want a failed attempt", cur)
			}
			if pairs, _ := s.ListPairs(context.Background(), "t1", PairQuery{}, 0); len(pairs) != 0 {
				t.Fatalf("pairs written from a malformed answer: %v", pairSubs(pairs))
			}
		})
	}
}

// A launch that keeps failing backs off with the doubling schedule, lands in
// the dead letter when its attempts are spent, and a replay launches it once —
// under the same key, which the launch path answers with the run of an
// attempt that was handed on, and starts otherwise.
func TestWorkerLaunchRetriesWithBackoffThenBuriesThenReplaysOnce(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	clock := &fakeClock{now: t0}
	sub := newSubmitter()
	broken := true
	sub.fail = func(string, int) error {
		if broken {
			return errors.New("queue unavailable")
		}
		return nil
	}
	w := newWorker(s, &stubMatcher{verdicts: []Verdict{{SubscriptionID: "s1", Matched: true}}}, sub, clock, "w")
	mustAccept(t, s, delivery("t1", "ing-1", "d", t0))

	tick(t, w)
	p := onlyPair(t, s, "t1")
	if p.State != PairPending || p.Attempts != 1 || !p.NotBefore.Equal(stamp(t0.Add(10*time.Second))) {
		t.Fatalf("after one failure = %+v, want the first backoff", p)
	}
	clock.Add(10 * time.Second)
	tick(t, w)
	p = onlyPair(t, s, "t1")
	if p.Attempts != 2 || !p.NotBefore.Equal(stamp(clock.Now().Add(20*time.Second))) {
		t.Fatalf("after two failures = %+v, want the doubled backoff", p)
	}
	clock.Add(20 * time.Second)
	tick(t, w)
	p = onlyPair(t, s, "t1")
	if p.State != PairDead || p.Attempts != 3 || sub.runCount() != 0 {
		t.Fatalf("after the third failure = %+v (%d runs), want the dead letter with three attempts and no run", p, sub.runCount())
	}

	broken = false
	if _, err := s.ReplayPair(ctx, "t1", p.ID, clock.Now()); err != nil {
		t.Fatal(err)
	}
	tick(t, w)
	tick(t, w)
	p = onlyPair(t, s, "t1")
	if p.State != PairLaunched || p.RunID != "run-1" || p.Epoch != 0 || sub.runCount() != 1 || sub.keys[3] != sub.keys[0] {
		t.Fatalf("after the replay = %+v (%d runs, keys %v), want one launch under the dead letter's key", p, sub.runCount(), sub.keys)
	}
}

// A last attempt whose submission was handed on but whose answer was lost —
// the acknowledgement timed out, or the worker died before its mark — buries
// a pair whose run lives. Its replay re-submits the same key and is answered
// by that run: one run for the pair, never a second.
func TestWorkerReplayOfADeadPairNeverStartsASecondRun(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	clock := &fakeClock{now: t0}
	sub := newSubmitter()
	sub.lose = func(int) bool { return true }
	w := newWorker(s, &stubMatcher{verdicts: []Verdict{{SubscriptionID: "s1", Matched: true}}}, sub, clock, "w")
	mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
	for i := 1; i <= w.Pairs.MaxAttempts; i++ {
		tick(t, w)
		clock.Add(w.Pairs.Delay(i))
	}
	p := onlyPair(t, s, "t1")
	if p.State != PairDead || sub.runCount() != 1 {
		t.Fatalf("after the lost answers: %+v, %d runs — want the dead letter over the one run its key started", p, sub.runCount())
	}
	sub.lose = nil
	if _, err := s.ReplayPair(ctx, "t1", p.ID, clock.Now()); err != nil {
		t.Fatal(err)
	}
	tick(t, w)
	if p = onlyPair(t, s, "t1"); p.State != PairLaunched || p.RunID != "run-1" || sub.runCount() != 1 {
		t.Fatalf("after the replay: %+v, %d runs — want the pair answered by its one run", p, sub.runCount())
	}
}

// A pair refused by matching never reaches a launch: no filter ever said yes
// to it, and a replay would launch it without one.
func TestWorkerNeverLaunchesAMatchRefusedPair(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	clock := &fakeClock{now: t0}
	sub := newSubmitter()
	w := newWorker(s, &stubMatcher{verdicts: []Verdict{{SubscriptionID: "s1", Refusal: "expression past its budget"}}}, sub, clock, "w")
	mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
	tick(t, w)
	p := onlyPair(t, s, "t1")
	if _, err := s.ReplayPair(ctx, "t1", p.ID, clock.Now()); !errors.Is(err, ErrNotReplayable) {
		t.Fatalf("replay of a match-refused pair = %v, want ErrNotReplayable", err)
	}
	tick(t, w)
	if sub.calls != 0 {
		t.Fatalf("a match-refused pair was submitted %d times", sub.calls)
	}
}

// The intake read under a pair's claim spends the claim's window like the
// launch: a read that overruns it releases the pair unattempted, and the
// launch is never handed a spent window.
func TestWorkerJudgesTheWindowAgainAfterTheIntakeRead(t *testing.T) {
	s := NewMemoryStore()
	clock := &fakeClock{now: t0}
	sub := newSubmitter()
	w := newWorker(slowIntakeRead{Store: s, clock: clock, by: 31 * time.Second}, &stubMatcher{verdicts: []Verdict{{SubscriptionID: "s1", Matched: true}}}, sub, clock, "w")
	mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
	tick(t, w)
	if p := onlyPair(t, s, "t1"); sub.calls != 0 || p.State != PairPending || p.Attempts != 0 {
		t.Fatalf("pair = %+v after %d submissions, want it released unattempted", p, sub.calls)
	}
}

// slowIntakeRead advances the worker's clock while it reads an intake.
type slowIntakeRead struct {
	Store
	clock *fakeClock
	by    time.Duration
}

func (s slowIntakeRead) GetIntake(ctx context.Context, tenantID, id string) (Intake, error) {
	in, err := s.Store.GetIntake(ctx, tenantID, id)
	s.clock.Add(s.by)
	return in, err
}

// spentError is what the launch path answers for a key whose run never left
// it — by method, as runview.SubmissionSpentError does.
type spentError struct{}

func (spentError) Error() string         { return "run never left its launch path" }
func (spentError) SubmissionSpent() bool { return true }

// A spent submission moves the pair to its next epoch: the next attempt
// submits under a new key and launches.
func TestWorkerMovesASpentSubmissionToTheNextEpoch(t *testing.T) {
	s := NewMemoryStore()
	clock := &fakeClock{now: t0}
	sub := newSubmitter()
	sub.fail = func(key string, call int) error {
		if call == 1 {
			return fmt.Errorf("launch: %w", spentError{})
		}
		return nil
	}
	w := newWorker(s, &stubMatcher{verdicts: []Verdict{{SubscriptionID: "s1", Matched: true}}}, sub, clock, "w")
	mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
	tick(t, w)
	p := onlyPair(t, s, "t1")
	if p.State != PairPending || p.Epoch != 1 || p.Attempts != 1 {
		t.Fatalf("after the spent submission = %+v, want epoch 1 after one attempt", p)
	}
	clock.Add(10 * time.Second)
	tick(t, w)
	p = onlyPair(t, s, "t1")
	if p.State != PairLaunched || sub.keys[0] == sub.keys[1] || sub.keys[1] != p.SubmissionKey() {
		t.Fatalf("pair = %+v, keys %v: want a launch under the next epoch's key", p, sub.keys)
	}
}

// A permanent refusal — the worker's own, or one the launch path marks by
// method — is recorded with its reason and never retried.
func TestWorkerRecordsARefusal(t *testing.T) {
	for name, err := range map[string]error{
		"intake.Refuse": Refuse("bot not found", errors.New("404")),
		"by method":     fmt.Errorf("launch: %w", &refusedByMethod{reason: "run id held by another tenant"}),
	} {
		t.Run(name, func(t *testing.T) {
			s := NewMemoryStore()
			clock := &fakeClock{now: t0}
			sub := newSubmitter()
			sub.fail = func(string, int) error { return err }
			w := newWorker(s, &stubMatcher{verdicts: []Verdict{{SubscriptionID: "s1", Matched: true}}}, sub, clock, "w")
			mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
			tick(t, w)
			p := onlyPair(t, s, "t1")
			var want string
			if r, ok := refusalReason(err); ok {
				want = r
			}
			if p.State != PairRefused || p.Reason != want || p.Attempts != 0 {
				t.Fatalf("pair = %+v, want refused saying %q", p, want)
			}
			clock.Add(time.Hour)
			tick(t, w)
			if sub.calls != 1 {
				t.Fatalf("a refused pair was submitted %d times", sub.calls)
			}
		})
	}
}

type refusedByMethod struct{ reason string }

func (r *refusedByMethod) Error() string         { return r.reason }
func (r *refusedByMethod) RefusalReason() string { return r.reason }

// A maintenance hold postpones without spending the attempt budget.
func TestWorkerMaintenanceHoldSpendsNoAttempt(t *testing.T) {
	s := NewMemoryStore()
	clock := &fakeClock{now: t0}
	sub := newSubmitter()
	sub.fail = func(string, int) error { return fmt.Errorf("platform: %w", schedgate.ErrMaintenanceHold) }
	w := newWorker(s, &stubMatcher{verdicts: []Verdict{{SubscriptionID: "s1", Matched: true}}}, sub, clock, "w")
	mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
	for i := 0; i < 5; i++ {
		tick(t, w)
		clock.Add(10 * time.Second)
	}
	p := onlyPair(t, s, "t1")
	if p.State != PairPending || p.Attempts != 0 || sub.calls != 5 {
		t.Fatalf("pair = %+v after %d holds, want pending with its budget whole", p, sub.calls)
	}
}

// postponedErr is a transient denial, as the launch gate answers a team at
// its concurrency cap.
type postponedErr struct{ after time.Duration }

func (e postponedErr) Error() string                      { return "concurrency cap" }
func (e postponedErr) PostponeFor() (time.Duration, bool) { return e.after, true }

// A transient denial postpones the pair by the delay it names and spends no
// attempt, however often it repeats (ADR-168 Q3).
func TestWorkerPostponesATransientDenialWithoutSpending(t *testing.T) {
	s := NewMemoryStore()
	clock := &fakeClock{now: t0}
	sub := newSubmitter()
	sub.fail = func(string, int) error { return fmt.Errorf("gate: %w", postponedErr{after: 45 * time.Second}) }
	w := newWorker(s, &stubMatcher{verdicts: []Verdict{{SubscriptionID: "s1", Matched: true}}}, sub, clock, "w")
	mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
	tick(t, w)
	p := onlyPair(t, s, "t1")
	if p.State != PairPending || p.Attempts != 0 || !p.NotBefore.Equal(stamp(t0.Add(45*time.Second))) {
		t.Fatalf("pair = %+v, want pending, attempts whole, due in 45s", p)
	}
	for i := 0; i < 10; i++ {
		clock.Add(time.Minute)
		tick(t, w)
	}
	if p := onlyPair(t, s, "t1"); p.State != PairPending || p.Attempts != 0 {
		t.Fatalf("pair = %+v after %d denials, want its budget whole", p, sub.calls)
	}
}

// A pair whose intake is gone cannot be launched: refused, visibly.
func TestWorkerRefusesAPairWhoseIntakeIsGone(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
	if err := s.WritePairs(ctx, []Pair{pending(in, "s1", t0)}); err != nil {
		t.Fatal(err)
	}
	delete(s.intakes, in.ID)
	sub := newSubmitter()
	w := newWorker(s, &stubMatcher{}, sub, &fakeClock{now: t0}, "w")
	if _, err := w.LaunchTick(ctx); err != nil {
		t.Fatal(err)
	}
	if p := onlyPair(t, s, "t1"); p.State != PairRefused || sub.calls != 0 {
		t.Fatalf("pair = %+v (%d submissions), want refused unsubmitted", p, sub.calls)
	}
}

// A worker whose claim expired mid-launch — it hung, or its process was cut —
// leaves the pair to the one that reclaims it: the same key is submitted, so
// one run, and the late worker's mark fails instead of landing. Built in both
// orders: the late mark before, and after, the reclaimer's own.
func TestWorkerALateWorkerNeitherDoublesNorClobbersALaunch(t *testing.T) {
	for _, lateFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("late mark first=%v", lateFirst), func(t *testing.T) {
			ctx := context.Background()
			s := NewMemoryStore()
			clock := &fakeClock{now: t0}
			sub := newSubmitter()
			matcher := &stubMatcher{verdicts: []Verdict{{SubscriptionID: "s1", Matched: true}}}
			a := newWorker(s, matcher, sub, clock, "a")
			b := newWorker(s, matcher, sub, clock, "b")
			mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
			if _, err := a.MatchTick(ctx); err != nil {
				t.Fatal(err)
			}
			// a claims the pair; while its Submit is in flight its claim
			// expires and b takes the pair.
			claimed, err := s.ClaimPairs(ctx, "a", clock.Now(), a.Pairs.ClaimTTL, 1)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("a's claim = (%v, %v)", claimed, err)
			}
			stale := claimed[0]
			clock.Add(a.Pairs.ClaimTTL + time.Second)
			runA, err := sub.Submit(ctx, Intake{}, stale)
			if err != nil {
				t.Fatal(err)
			}
			lateMark := func() error { return s.MarkPairLaunched(ctx, stale.ID, stale.Generation, runA, clock.Now()) }
			if lateFirst {
				sub.during = func(string) {
					if err := lateMark(); !errors.Is(err, ErrClaimLost) {
						t.Errorf("a's late mark during b's launch = %v, want ErrClaimLost", err)
					}
				}
			}
			if _, err := b.LaunchTick(ctx); err != nil {
				t.Fatal(err)
			}
			if !lateFirst {
				if err := lateMark(); !errors.Is(err, ErrClaimLost) {
					t.Fatalf("a's late mark after b's = %v, want ErrClaimLost", err)
				}
			}
			p := onlyPair(t, s, "t1")
			if p.State != PairLaunched || p.RunID != runA || p.Owner != "" || sub.runCount() != 1 {
				t.Fatalf("pair = %+v, %d runs: want one run, launched by b's claim", p, sub.runCount())
			}
		})
	}
}

// A batch that outlived a row's share of its claim gives the row back
// unattempted instead of racing whoever takes it after the expiry.
func TestWorkerReleasesARowPastItsWindow(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	clock := &fakeClock{now: t0}
	sub := newSubmitter()
	w := newWorker(s, &stubMatcher{}, sub, clock, "w")
	in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
	if err := s.WritePairs(ctx, []Pair{pending(in, "s1", t0)}); err != nil {
		t.Fatal(err)
	}
	claimed, _ := s.ClaimPairs(ctx, "w", t0, w.Pairs.ClaimTTL, 1)
	clock.Add(w.Pairs.ClaimTTL / 2)
	w.launchOne(ctx, claimed[0])
	p := onlyPair(t, s, "t1")
	if p.State != PairPending || p.Attempts != 0 || sub.calls != 0 {
		t.Fatalf("pair = %+v (%d submissions), want it released unattempted", p, sub.calls)
	}
}

// Work that never returns still reaches its dead letter: each reclaim spends
// an attempt, and a reclaim past the budget buries the row.
func TestWorkerBuriesARowReclaimedPastItsBudget(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	clock := &fakeClock{now: t0}
	sub := newSubmitter()
	w := newWorker(s, &stubMatcher{}, sub, clock, "w")
	in := mustAccept(t, s, delivery("t1", "ing-1", "d", t0))
	if err := s.WritePairs(ctx, []Pair{pending(in, "s1", t0)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < w.Pairs.MaxAttempts; i++ {
		if _, err := s.ClaimPairs(ctx, "hung", clock.Now(), w.Pairs.ClaimTTL, 1); err != nil {
			t.Fatal(err)
		}
		clock.Add(w.Pairs.ClaimTTL)
	}
	if _, err := w.LaunchTick(ctx); err != nil {
		t.Fatal(err)
	}
	if p := onlyPair(t, s, "t1"); p.State != PairDead || sub.calls != 0 {
		t.Fatalf("pair = %+v (%d submissions), want the dead letter", p, sub.calls)
	}
}

func TestWorkerRefusesAnIncompleteConfiguration(t *testing.T) {
	ok := newWorker(NewMemoryStore(), &stubMatcher{}, newSubmitter(), &fakeClock{now: t0}, "w")
	for name, mutate := range map[string]func(*Worker){
		"no store":     func(w *Worker) { w.Store = nil },
		"no matcher":   func(w *Worker) { w.Matcher = nil },
		"no submitter": func(w *Worker) { w.Submitter = nil },
		"no owner":     func(w *Worker) { w.Owner = "" },
		"no claim ttl": func(w *Worker) { w.Pairs.ClaimTTL = 0 },
	} {
		w := *ok
		mutate(&w)
		if _, err := w.LaunchTick(context.Background()); err == nil {
			t.Errorf("%s: LaunchTick ran", name)
		}
		if err := w.Run(context.Background(), time.Second); err == nil {
			t.Errorf("%s: Run started", name)
		}
	}
}
