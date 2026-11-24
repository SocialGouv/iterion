package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// unreachableResume is what a resume from a node whose egress is broken
// answers: the HTTP client's *url.Error around a resolver failure.
func unreachableResume() error {
	return &url.Error{Op: "Post", URL: "http://queue.invalid", Err: &net.DNSError{Err: "no such host", Name: "queue.invalid", IsNotFound: true}}
}

// deliveryVerdictStore lets one test fail the delivery path's own store
// writes and reads (the due-episode list, the completion write) while every
// read the scan makes still answers.
type deliveryVerdictStore struct {
	runwatch.Store
	listDueErr  error
	completeErr error
}

func (s *deliveryVerdictStore) ListDueEpisodes(ctx context.Context, now time.Time, limit int) ([]runwatch.Episode, error) {
	if s.listDueErr != nil {
		return nil, s.listDueErr
	}
	return s.Store.ListDueEpisodes(ctx, now, limit)
}

func (s *deliveryVerdictStore) CompleteEpisode(ctx context.Context, id, owner string, now time.Time) error {
	if s.completeErr != nil {
		return s.completeErr
	}
	return s.Store.CompleteEpisode(ctx, id, owner, now)
}

// armDeliveryWatch arms one watch over a failed target, so the next sweep
// observes the failure and owes one delivery to the assistant.
func armDeliveryWatch(t *testing.T, f *armFixture, suffix string) {
	t.Helper()
	assistant := f.assistant(t, "verdict-assistant-"+suffix, "verdict-card-"+suffix)
	target := f.target(t, "verdict-target-"+suffix, "verdict-target-card-"+suffix, assistant.CreatedAt.Add(time.Minute))
	if err := f.ws.CreateWatch(context.Background(), runwatch.Watch{
		ID: "verdict-watch-" + suffix, TargetRunID: target.ID, AssistantRunID: assistant.ID,
		State: runwatch.WatchActive, Kinds: []string{trigger.KindRunFailed}, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// armIdleWatch arms one watch over a target still running: the scan reads
// it, and nothing is owed.
func armIdleWatch(t *testing.T, f *armFixture) {
	t.Helper()
	assistant := f.assistant(t, "idle-assistant", "idle-card")
	target := f.target(t, "idle-target", "idle-target-card", assistant.CreatedAt.Add(time.Minute))
	target.Status = store.RunStatusRunning
	if err := f.rs.SaveRun(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if err := f.ws.CreateWatch(context.Background(), runwatch.Watch{
		ID: "idle-watch", TargetRunID: target.ID, AssistantRunID: assistant.ID,
		State: runwatch.WatchActive, Kinds: []string{trigger.KindRunFailed}, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// The assistant run watch's pass is worth its deliveries, never its scan
// (#2986): a holder whose store reads succeed while every resume reaches
// nothing reads unreachable, so the handover can move Copi's veilles to a
// replica that delivers them.
func TestAssistantWatchSweepPass_classifiesWhatItsDeliveriesReached(t *testing.T) {
	queueDown := fmt.Errorf("resume: %w", &runview.QueueUnavailableError{Cause: errors.New("nats: timeout")})
	for _, c := range []struct {
		name        string
		arm         func(t *testing.T, f *armFixture)
		resume      []error // the answer of the n-th resume; the last one repeats
		listDueErr  error
		completeErr error
		want        lease.PassVerdict
		wantCalls   int
	}{
		{name: "no armed watch", want: lease.VerdictIdle},
		{name: "an armed watch with nothing due", arm: armIdleWatch, want: lease.VerdictIdle},
		{name: "a delivered episode", arm: armOne, resume: []error{nil}, want: lease.VerdictProgressed, wantCalls: 1},
		{name: "a resume that reached nothing", arm: armOne, resume: []error{unreachableResume()}, want: lease.VerdictUnreachable, wantCalls: 1},
		{name: "a resume refused without reaching the far side", arm: armOne, resume: []error{errors.New("run is not resumable")}, want: lease.VerdictIdle, wantCalls: 1},
		{name: "a queue that accepted nothing", arm: armOne, resume: []error{queueDown}, want: lease.VerdictUnreachable, wantCalls: 1},
		{name: "a resume cut off under a live term", arm: armOne, resume: []error{context.Canceled}, want: lease.VerdictIdle, wantCalls: 1},
		{name: "a resume that timed out under a live term", arm: armOne, resume: []error{fmt.Errorf("publish: %w", context.DeadlineExceeded)}, want: lease.VerdictUnreachable, wantCalls: 1},
		{name: "the due episodes unreadable", arm: armIdleWatch, listDueErr: unreachableResume(), want: lease.VerdictUnreachable},
		{name: "the completion write unreachable", arm: armOne, resume: []error{nil}, completeErr: unreachableResume(), want: lease.VerdictUnreachable, wantCalls: 1},
		{name: "refused then unreachable", arm: armTwo, resume: []error{errors.New("refused"), unreachableResume()}, want: lease.VerdictUnreachable, wantCalls: 2},
		{name: "unreachable then refused", arm: armTwo, resume: []error{unreachableResume(), errors.New("refused")}, want: lease.VerdictUnreachable, wantCalls: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newArmFixture(t, true)
			if c.arm != nil {
				c.arm(t, f)
			}
			var mu sync.Mutex
			calls := 0
			f.coord.resumeRun = func(context.Context, runview.ResumeSpec) (*runview.LaunchResult, error) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if len(c.resume) == 0 {
					t.Errorf("unexpected resume")
					return nil, errors.New("unexpected resume")
				}
				err := c.resume[min(calls, len(c.resume))-1]
				if err != nil {
					return nil, err
				}
				return &runview.LaunchResult{}, nil
			}
			f.coord.setRuntime(f.srv.runs, &deliveryVerdictStore{Store: f.ws, listDueErr: c.listDueErr, completeErr: c.completeErr}, f.srv.cfg.Bots.Paths)
			got := f.coord.sweep(t.Context())
			mu.Lock()
			defer mu.Unlock()
			if calls != c.wantCalls {
				t.Fatalf("the sweep made %d resumes, want %d — the fixture did not reach the delivery under test", calls, c.wantCalls)
			}
			if got != c.want {
				t.Fatalf("pass verdict = %s, want %s", got, c.want)
			}
		})
	}
}

func armOne(t *testing.T, f *armFixture) { armDeliveryWatch(t, f, "a") }

func armTwo(t *testing.T, f *armFixture) {
	armDeliveryWatch(t, f, "a")
	armDeliveryWatch(t, f, "b")
}

// A delivery made outside an elected sweep — an HTTP request, a direct call —
// accounts into nothing and must not panic on the absent pass.
func TestAssistantWatchAttempt_outsideAPassAccountsIntoNothing(t *testing.T) {
	f := newArmFixture(t, true)
	armOne(t, f)
	f.coord.resumeRun = func(context.Context, runview.ResumeSpec) (*runview.LaunchResult, error) {
		return nil, unreachableResume()
	}
	// The sweep creates the episode and its delivery fails; the episode is
	// released, then claimed again below on a ctx that carries no pass.
	f.coord.sweep(t.Context())
	episodes, err := f.ws.ListEpisodesByWatch(t.Context(), "verdict-watch-a", "", 10)
	if err != nil || len(episodes) != 1 {
		t.Fatalf("episodes = %+v, err = %v; want one", episodes, err)
	}
	f.coord.attempt(store.WithTenant(t.Context(), episodes[0].TenantID), episodes[0].ID)
}

// The term ending inside a delivery records nothing, through the production
// sweep loop: whatever the cut-off resume answers — a transport failure or a
// success — the Health keeps the run of unreachable passes the term had
// accumulated.
func TestAssistantWatchSweep_aTermEndingInsideADeliveryAccountsIntoNothing(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer error
	}{
		{name: "the resume cut off answers unreachable", answer: unreachableResume()},
		{name: "the resume answered as the term ended", answer: nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newArmFixture(t, true)
			armOne(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			f.coord.resumeRun = func(context.Context, runview.ResumeSpec) (*runview.LaunchResult, error) {
				calls++
				cancel()
				if c.answer != nil {
					return nil, c.answer
				}
				return &runview.LaunchResult{}, nil
			}
			health := &lease.Health{Passes: 3}
			health.Observe(lease.VerdictUnreachable)
			health.Observe(lease.VerdictUnreachable)
			f.coord.sweepLoop(ctx, nil, health)
			if calls != 1 {
				t.Fatalf("the sweep made %d resumes, want exactly 1", calls)
			}
			if st := health.Snapshot(); st.LastPassVerdict != lease.VerdictIdle.String() || st.FailingPasses != 2 {
				t.Fatalf("handover state = %+v, want an idle pass over an intact run of 2 unreachable passes — this holder's own step-down says nothing about its delivery path", st)
			}
		})
	}
}

// The handover itself, through the production loop: a holder whose store
// reads all answer while its deliveries reach nothing ends its term, so the
// lease goes to a replica that can deliver.
func TestAssistantWatchSweepLoop_HandsOverWhenItsDeliveriesReachNothing(t *testing.T) {
	f := newArmFixture(t, true)
	armOne(t, f)
	f.coord.resumeRun = func(context.Context, runview.ResumeSpec) (*runview.LaunchResult, error) {
		return nil, unreachableResume()
	}
	health := &lease.Health{Passes: 1}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.coord.sweepLoop(t.Context(), nil, health)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep loop kept its term although every delivery reached nothing")
	}
	if st := health.Snapshot(); st.Handovers != 1 {
		t.Fatalf("handover state = %+v, want one handover", st)
	}
}

// The recorder itself, on a term that has ended: an answer that lands after
// the term's end — a completion write that succeeded as the holder stepped
// down — records nothing in either arm, and a bare cancellation under a live
// term is no item either. The sweep cannot stage a write that succeeds under
// a done ctx (assistantWatchContextStore refuses it first), so the race is
// exercised here.
func TestRecordWatchDelivery_recordsNeitherTheTermsEndNorACancellation(t *testing.T) {
	for _, c := range []struct {
		name string
		done bool
		err  error
	}{
		{name: "a success after the term ended", done: true},
		{name: "a transport failure after the term ended", done: true, err: unreachableResume()},
		{name: "a cancellation under a live term", err: fmt.Errorf("resume: %w", context.Canceled)},
		{name: "a local refusal before any publish", err: capacityRefusal("r")},
	} {
		t.Run(c.name, func(t *testing.T) {
			var pass lease.Pass
			ctx, cancel := context.WithCancel(withWatchPass(context.Background(), &pass))
			defer cancel()
			if c.done {
				cancel()
			}
			recordWatchDelivery(ctx, c.err)
			if got := pass.Verdict(); got != lease.VerdictIdle {
				t.Fatalf("pass verdict = %s, want idle — nothing was learned about the delivery path", got)
			}
		})
	}
	// Outside a pass, recording is a no-op rather than a panic.
	recordWatchDelivery(context.Background(), unreachableResume())

	// A cancellation joined to a reach failure is classified cause by cause,
	// as pkg/lease states: the cut-off item is evidence about nothing and the
	// DNS failure still decides.
	var pass lease.Pass
	recordWatchDelivery(withWatchPass(context.Background(), &pass), errors.Join(unreachableResume(), context.Canceled))
	if got := pass.Verdict(); got != lease.VerdictUnreachable {
		t.Fatalf("joined cancellation and DNS failure: pass verdict = %s, want unreachable", got)
	}
}

// simClockStore advances the episode store's notion of now by a simulated
// offset, so a test drives consecutive sweep passes one sweep interval apart
// without sleeping: attempt and deliverDue read the wall clock, and every
// time they hand the store is shifted by the offset. createErr and claimErr
// fail the delivery path's own episode writes while every read answers.
type simClockStore struct {
	runwatch.Store
	offset    atomic.Int64 // nanoseconds
	createErr error
	claimErr  error
}

func (s *simClockStore) off() time.Duration { return time.Duration(s.offset.Load()) }

func (s *simClockStore) ListDueEpisodes(ctx context.Context, now time.Time, limit int) ([]runwatch.Episode, error) {
	return s.Store.ListDueEpisodes(ctx, now.Add(s.off()), limit)
}

func (s *simClockStore) ClaimEpisode(ctx context.Context, id, owner string, now time.Time, leaseFor time.Duration) (runwatch.Episode, bool, error) {
	if s.claimErr != nil {
		return runwatch.Episode{}, false, s.claimErr
	}
	return s.Store.ClaimEpisode(ctx, id, owner, now.Add(s.off()), leaseFor)
}

func (s *simClockStore) ReleaseEpisode(ctx context.Context, id, owner string, next time.Time, reason string) error {
	return s.Store.ReleaseEpisode(ctx, id, owner, next.Add(s.off()), reason)
}

func (s *simClockStore) CreateEpisode(ctx context.Context, ep runwatch.Episode) (bool, error) {
	if s.createErr != nil {
		return false, s.createErr
	}
	return s.Store.CreateEpisode(ctx, ep)
}

// queueTimeout is the chain a cloud resume returns when this node's publish
// to the queue outlives its retries: a QueueUnavailableError over the
// publish deadline, wrapped by the resume.
func queueTimeout() error {
	return fmt.Errorf("cloudpublisher: republish: %w", &runview.QueueUnavailableError{Cause: context.DeadlineExceeded})
}

// capacityRefusal is the chain cloudpublisher returns for a resume it
// refuses locally, before any queue traffic.
func capacityRefusal(runID string) error {
	return fmt.Errorf("cloudpublisher: resume of run %s refused before publishing, the run put back as it was: %w", runID,
		fmt.Errorf("%w: api-key(anthropic) %q holds 2/2 alive runs", cloudpublisher.ErrCredentialAtCapacity, "team-key"))
}

// runSimPasses drives the production sweep and feeds each verdict to health
// exactly as sweepLoop does. Passes are paced like time.Ticker: pass i starts
// at simulated start + (i-1) sweep intervals whatever real time the earlier
// passes took, and the real time elapsed inside a pass is kept. It answers
// the pass that handed over (0: none within max) and the verdict sequence,
// for the failure message.
func runSimPasses(t *testing.T, f *armFixture, ws *simClockStore, health *lease.Health, max int) (int, string) {
	t.Helper()
	var seq []string
	start := time.Now()
	for i := 1; i <= max; i++ {
		ws.offset.Store(int64(time.Duration(i-1)*assistantWatchSweepInterval) - int64(time.Since(start)))
		v := f.coord.sweep(t.Context())
		handed := health.Observe(v)
		seq = append(seq, fmt.Sprintf("p%d(+%ds):%s/fail=%d", i, (i-1)*int(assistantWatchSweepInterval/time.Second), v, health.Snapshot().FailingPasses))
		if handed {
			return i, strings.Join(seq, " ")
		}
	}
	return 0, strings.Join(seq, " ")
}

// A holder whose delivery path reaches nothing hands over through K=3
// passes, every one after the first delivered by deliverDue's retries: the
// observer creates an episode once and attempts it in the scan, so passes
// 2..K of a real handover are retries. A resume refused locally before any
// publish (a credential at capacity) is no answer from the far side and
// cannot clear the run of unreachable passes another watch's queue timeouts
// build, and a create or claim of the episode that reaches nothing is a
// delivery that never started.
func TestAssistantWatchSweep_K3HandoverThroughRetryDeliveries(t *testing.T) {
	for _, c := range []struct {
		name      string
		arm       func(t *testing.T, f *armFixture)
		resume    func(runID string) error
		createErr error
		claimErr  error
		wantPass  int // the first pass that may hand over
		maxPass   int // the last pass that may hand over (0: wantPass exactly)
		minCalls  int
	}{
		// A released episode falls due 20 s after its claim, which lands
		// just after the next tick, so one pass in between reads idle.
		{name: "every resume unreachable", arm: armOne, resume: func(string) error { return unreachableResume() }, wantPass: 4, maxPass: 5, minCalls: 3},
		// A publish timeout leaves its episode claimed for its 45 s
		// delivery lease, so it is retried every third pass.
		{name: "a queue timeout alone", arm: armOne, resume: func(string) error { return queueTimeout() }, wantPass: 7, minCalls: 3},
		{name: "a queue timeout beside a local capacity refusal", arm: armTwo, resume: func(runID string) error {
			if strings.HasSuffix(runID, "-b") {
				return capacityRefusal(runID)
			}
			return queueTimeout()
		}, wantPass: 7, minCalls: 3},
		{name: "the episode create unreachable", arm: armOne, createErr: unreachableResume(), wantPass: 3},
		{name: "the episode claim unreachable", arm: armOne, claimErr: unreachableResume(), wantPass: 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newArmFixture(t, true)
			c.arm(t, f)
			var mu sync.Mutex
			calls := 0
			f.coord.resumeRun = func(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if c.resume == nil {
					return &runview.LaunchResult{}, nil
				}
				return nil, c.resume(spec.RunID)
			}
			ws := &simClockStore{Store: f.ws, createErr: c.createErr, claimErr: c.claimErr}
			f.coord.setRuntime(f.srv.runs, ws, f.srv.cfg.Bots.Paths)
			health := &lease.Health{Passes: 3}
			at, seq := runSimPasses(t, f, ws, health, 30)
			mu.Lock()
			defer mu.Unlock()
			if at < c.wantPass || at > max(c.wantPass, c.maxPass) {
				t.Fatalf("handover at pass %d (0: none in 30 passes), want pass %d..%d; %s", at, c.wantPass, max(c.wantPass, c.maxPass), seq)
			}
			if calls < c.minCalls {
				t.Fatalf("the sweeps made %d resumes, want at least %d — the retries under test did not run; %s", calls, c.minCalls, seq)
			}
		})
	}
}
