package cloudpublisher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The witnesses of ADR-168's pair machine against the REAL launch path: the
// run service's Launch and this publisher's SubmitLaunch over a run store,
// with only the queue's broker replaced. A crash is the state it leaves
// behind — a worker that never returns.

const submissionBot = "workflow demo:\n  entry: done\n"

var irWorkflowDemo = ir.Workflow{Name: "demo"}

// queueRecorder stands for the broker: it records what landed, and hold — when
// set — decides each publication first.
type queueRecorder struct {
	mu     sync.Mutex
	landed []queue.RunMessage
	hold   func(n int, msg *queue.RunMessage) error
	calls  int
}

func (q *queueRecorder) publish(_ context.Context, msg *queue.RunMessage) error {
	q.mu.Lock()
	q.calls++
	n, hold := q.calls, q.hold
	q.mu.Unlock()
	if hold != nil {
		if err := hold(n, msg); err != nil {
			return err
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.landed = append(q.landed, *msg)
	return nil
}

func (q *queueRecorder) messages() []queue.RunMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]queue.RunMessage(nil), q.landed...)
}

type submissionFixture struct {
	st  *store.FilesystemRunStore
	pub *Publisher
	svc *runview.Service
	q   *queueRecorder
	ctx context.Context
}

func newSubmissionFixture(t *testing.T) *submissionFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	q := &queueRecorder{}
	pub := &Publisher{store: st, logger: iterlog.Nop(), publishRetryDelays: []time.Duration{}, publishRun: q.publish}
	svc, err := runview.NewService(dir, runview.WithLogger(iterlog.Nop()), runview.WithStore(st), runview.WithLaunchPublisher(pub))
	if err != nil {
		t.Fatal(err)
	}
	return &submissionFixture{st: st, pub: pub, svc: svc, q: q, ctx: store.WithIdentity(context.Background(), "team-a", "u1")}
}

func (f *submissionFixture) spec(key string) runview.LaunchSpec {
	return runview.LaunchSpec{FilePath: "demo.bot", Source: submissionBot, SubmissionKey: key}
}

func (f *submissionFixture) runs(t *testing.T) []*store.Run {
	t.Helper()
	ids, err := f.st.ListRuns(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Run
	for _, id := range ids {
		r, err := f.st.LoadRun(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// claimByRunner moves a queued run to running, as a runner's claim does.
func (f *submissionFixture) claimByRunner(t *testing.T, runID string) {
	t.Helper()
	ok, err := f.st.UpdateRunStatusIfCoded(f.ctx, runID, store.RunStatusRunning, "", "", []store.RunStatus{store.RunStatusQueued})
	if err != nil || !ok {
		t.Fatalf("runner claim of %s = (%v, %v)", runID, ok, err)
	}
}

// crashAfterPersist runs a submission whose broker never answers — the
// process died between the run's persistence and its publication — and
// returns once the run is persisted. Its goroutine is never resumed.
func (f *submissionFixture) crashAfterPersist(t *testing.T, submit func()) {
	t.Helper()
	persisted := make(chan struct{})
	never := make(chan struct{})
	f.q.mu.Lock()
	prev := f.q.hold
	f.q.hold = func(n int, msg *queue.RunMessage) error {
		if n == 1 {
			close(persisted)
			<-never
		}
		if prev != nil {
			return prev(n, msg)
		}
		return nil
	}
	f.q.mu.Unlock()
	go submit()
	select {
	case <-persisted:
	case <-time.After(30 * time.Second):
		t.Fatal("the crashing submission never reached its publication")
	}
}

// A queued run whose publication never happened is COMPLETED by its key: the
// same row, at the next version, with its first instants, published once.
func TestSubmitLaunch_CompletesAPersistedUnpublishedRun(t *testing.T) {
	f := newSubmissionFixture(t)
	id := runview.SubmissionRunID("team-a", "k")
	f.crashAfterPersist(t, func() { _, _ = f.svc.Launch(f.ctx, f.spec("k")) })
	before, err := f.st.LoadRun(f.ctx, id)
	if err != nil || before.Status != store.RunStatusQueued || len(f.q.messages()) != 0 {
		t.Fatalf("after the crash: run %+v (%v), %d messages — want a queued run, nothing published", before, err, len(f.q.messages()))
	}

	res, err := f.svc.Launch(f.ctx, f.spec("k"))
	if err != nil || res.RunID != id || res.AlreadySubmitted || res.Status != store.RunStatusQueued {
		t.Fatalf("completion = (%+v, %v), want the run published by this call", res, err)
	}
	msgs := f.q.messages()
	if len(msgs) != 1 || msgs[0].RunID != id || msgs[0].Resume != nil {
		t.Fatalf("published %+v, want one launch message for %s", msgs, id)
	}
	after, _ := f.st.LoadRun(f.ctx, id)
	if after.CASVersion != before.CASVersion+1 || !after.QueuedAt.Equal(*before.QueuedAt) || !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("completed row: version %d→%d, queued_at %v→%v, created_at %v→%v — want the same row at the next version, its instants kept",
			before.CASVersion, after.CASVersion, before.QueuedAt, after.QueuedAt, before.CreatedAt, after.CreatedAt)
	}
	if published, _ := time.Parse(time.RFC3339Nano, msgs[0].PublishedAtRFC); queue.Superseded(&msgs[0], after) || published.Before(*after.QueuedAt) {
		t.Fatalf("the completion's message reads as superseded by its own run (published %s, queued %s)", msgs[0].PublishedAtRFC, after.QueuedAt)
	}
	if n := len(f.runs(t)); n != 1 {
		t.Fatalf("%d runs, want one", n)
	}
}

// The publisher judges the run again at its own read: a runner that claimed
// it after the service's read makes the submission acknowledged, and nothing
// is written or published.
func TestSubmitLaunch_AnswersARunClaimedSinceTheServiceRead(t *testing.T) {
	f := newSubmissionFixture(t)
	id := runview.SubmissionRunID("team-a", "k")
	f.crashAfterPersist(t, func() { _, _ = f.svc.Launch(f.ctx, f.spec("k")) })
	f.claimByRunner(t, id)
	before, _ := f.st.LoadRun(f.ctx, id)
	sub, err := f.pub.SubmitLaunch(f.ctx, id, f.spec("k"), nil, nil)
	if err != nil || !sub.AlreadySubmitted || sub.Status != store.RunStatusRunning {
		t.Fatalf("SubmitLaunch = (%+v, %v), want the running run acknowledged", sub, err)
	}
	if after, _ := f.st.LoadRun(f.ctx, id); after.CASVersion != before.CASVersion || len(f.q.messages()) != 0 {
		t.Fatalf("the acknowledged run was rewritten (version %d→%d) or published (%d)", before.CASVersion, after.CASVersion, len(f.q.messages()))
	}
	if _, err := f.pub.SubmitLaunch(f.ctx, "another-id", f.spec("k"), nil, nil); err == nil {
		t.Fatal("the publisher completed a run id its key does not derive")
	}
}

// The publisher's own read answers a deleted run with a refusal — the
// service's read precedes it, but a deletion can land in between, and
// reading the tombstone as "no run" would launch the key again.
func TestSubmitLaunch_RefusesADeletedSubmissionAtItsOwnRead(t *testing.T) {
	f := newSubmissionFixture(t)
	id := runview.SubmissionRunID("team-a", "k")
	f.crashAfterPersist(t, func() { _, _ = f.svc.Launch(f.ctx, f.spec("k")) })
	if _, err := f.st.UpdateRunStatusIfCoded(f.ctx, id, store.RunStatusFailed, "operator gave up", "", []store.RunStatus{store.RunStatusQueued}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DeleteRun(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	_, err := f.pub.SubmitLaunch(f.ctx, id, f.spec("k"), &irWorkflowDemo, nil)
	var refused *runview.SubmissionRefusedError
	if !errors.As(err, &refused) || !errors.Is(err, store.ErrRunDeleted) {
		t.Fatalf("SubmitLaunch on a deleted submission = %v, want a refusal naming the deletion", err)
	}
	if len(f.q.messages()) != 0 {
		t.Fatalf("a deleted submission was published again")
	}
}

// casRaceStore lets a runner claim the run between the publisher's read of
// it and its save.
type casRaceStore struct {
	store.RunStore
	after func(id string)
	once  sync.Once
}

func (s *casRaceStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	r, err := s.RunStore.LoadRun(ctx, id)
	if err == nil && s.after != nil {
		s.once.Do(func() { s.after(id) })
	}
	return r, err
}

// A completion's save is a compare-and-swap on the version it read: a claim
// landing in between refuses the save, and the runner's row stands — neither
// overwritten nor ended by the launch's rollback.
func TestSubmitLaunch_ACompletionNeverOverwritesAClaimThatRacedIt(t *testing.T) {
	f := newSubmissionFixture(t)
	id := runview.SubmissionRunID("team-a", "k")
	f.crashAfterPersist(t, func() { _, _ = f.svc.Launch(f.ctx, f.spec("k")) })
	f.pub.store = &casRaceStore{RunStore: f.st, after: func(id string) { f.claimByRunner(t, id) }}
	if _, err := f.pub.SubmitLaunch(f.ctx, id, f.spec("k"), &irWorkflowDemo, nil); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("SubmitLaunch across a racing claim = %v, want the save's conflict", err)
	}
	r, _ := f.st.LoadRun(f.ctx, id)
	if r.Status != store.RunStatusRunning || len(f.q.messages()) != 0 {
		t.Fatalf("after the refused completion: status %s, %d messages — want the runner's row, nothing published", r.Status, len(f.q.messages()))
	}
	f.pub.store = f.st
	res, err := f.svc.Launch(f.ctx, f.spec("k"))
	if err != nil || !res.AlreadySubmitted {
		t.Fatalf("the retry = (%+v, %v), want the claimed run acknowledged", res, err)
	}
}

// ---- the pair machine, end to end ----

type launchSubmitter struct{ f *submissionFixture }

func (s launchSubmitter) Submit(ctx context.Context, in intake.Intake, p intake.Pair) (string, error) {
	res, err := s.f.svc.Launch(store.WithIdentity(ctx, in.TenantID, "connector:"+in.ConnectionID), s.f.spec(p.SubmissionKey()))
	if err != nil {
		return "", err
	}
	return res.RunID, nil
}

type oneMatch struct{}

func (oneMatch) Match(context.Context, intake.Intake) ([]intake.Verdict, error) {
	return []intake.Verdict{{SubscriptionID: "s1", Matched: true}}, nil
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// lostMarks is a worker's store whose launch marks never land: the process
// died between its submission's acknowledgement and the mark.
type lostMarks struct{ intake.Store }

func (lostMarks) MarkPairLaunched(context.Context, string, int64, string, time.Time) error {
	return errors.New("process died before the mark")
}

type machine struct {
	f      *submissionFixture
	st     *intake.MemoryStore
	clock  *clock
	policy intake.Policy
}

func newMachine(t *testing.T) *machine {
	f := newSubmissionFixture(t)
	m := &machine{f: f, st: intake.NewMemoryStore(), clock: &clock{now: time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC)},
		policy: intake.Policy{MaxAttempts: 3, Backoff: 10 * time.Second, BackoffMax: time.Minute, ClaimTTL: time.Minute}}
	if _, _, err := m.st.Accept(f.ctx, intake.Intake{
		TenantID: "team-a", ConnectionID: "c1", IngressID: "i1", TriggerID: "issue.updated",
		DedupKey: "digest", Event: []byte(`{}`), ReceivedAt: m.clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *machine) worker(owner string, st intake.Store) *intake.Worker {
	return &intake.Worker{Store: st, Matcher: oneMatch{}, Submitter: launchSubmitter{m.f}, Owner: owner,
		Intakes: m.policy, Pairs: m.policy, Now: m.clock.Now, Logger: iterlog.Nop()}
}

func (m *machine) tick(t *testing.T, w *intake.Worker) {
	t.Helper()
	if _, err := w.MatchTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.LaunchTick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (m *machine) pair(t *testing.T) intake.Pair {
	t.Helper()
	pairs, err := m.st.ListPairs(context.Background(), "team-a", intake.PairQuery{}, 0)
	if err != nil || len(pairs) != 1 {
		t.Fatalf("pairs = (%d, %v), want one", len(pairs), err)
	}
	return pairs[0]
}

// exactlyOnePublishedRun: one run reached the queue, and every message that
// landed is that run's one launch identity — a launch message (its
// Nats-Msg-Id is the run id, so the broker absorbs a repeat inside its window)
// that its run's queued_at does not supersede (the runner's admission absorbs
// a repeat past it).
func exactlyOnePublishedRun(t *testing.T, f *submissionFixture, p intake.Pair) {
	t.Helper()
	if p.State != intake.PairLaunched || p.RunID == "" {
		t.Fatalf("pair = %+v, want launched", p)
	}
	msgs := f.q.messages()
	if len(msgs) == 0 {
		t.Fatal("nothing was published")
	}
	r, err := f.st.LoadRun(f.ctx, p.RunID)
	if err != nil {
		t.Fatalf("the pair's run %s: %v", p.RunID, err)
	}
	for _, msg := range msgs {
		if msg.RunID != p.RunID || msg.Resume != nil || queue.Superseded(&msg, r) {
			t.Fatalf("message %+v is not run %s's launch (queued_at %v)", msg, p.RunID, r.QueuedAt)
		}
	}
	if r.Status == store.RunStatusFailed {
		t.Fatalf("the pair's run reads failed (%s): %s", r.FailureCode, r.Error)
	}
}

func TestPairMachine_ACrashBeforeTheLaunchEndsWithOnePublishedRun(t *testing.T) {
	m := newMachine(t)
	a := m.worker("a", m.st)
	if _, err := a.MatchTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// a claims the pair and dies before submitting anything.
	if got, err := m.st.ClaimPairs(context.Background(), "a", m.clock.Now(), m.policy.ClaimTTL, 1); err != nil || len(got) != 1 {
		t.Fatalf("a's claim = (%v, %v)", got, err)
	}
	m.clock.Add(m.policy.ClaimTTL)
	m.tick(t, m.worker("b", m.st))
	exactlyOnePublishedRun(t, m.f, m.pair(t))
	if n := len(m.f.q.messages()); n != 1 {
		t.Fatalf("%d messages, want 1", n)
	}
}

// A worker that dies after its run was persisted and before it was published
// leaves a queued run nothing will claim; the worker that reclaims the pair
// completes it. Built in both orders of the dead worker's publication: never
// landing (a crash), and landing after the completion (a hang).
func TestPairMachine_ACrashBetweenPersistenceAndPublicationEndsWithOnePublishedRun(t *testing.T) {
	for _, lateLanding := range []bool{false, true} {
		t.Run(fmt.Sprintf("late landing=%v", lateLanding), func(t *testing.T) {
			m := newMachine(t)
			a := m.worker("a", m.st)
			if _, err := a.MatchTick(context.Background()); err != nil {
				t.Fatal(err)
			}
			persisted := make(chan struct{})
			release := make(chan error)
			aDone := make(chan struct{})
			m.f.q.hold = func(n int, _ *queue.RunMessage) error {
				if n == 1 {
					close(persisted)
					return <-release
				}
				return nil
			}
			go func() { defer close(aDone); _, _ = a.LaunchTick(context.Background()) }()
			<-persisted
			if runs := m.f.runs(t); len(runs) != 1 || runs[0].Status != store.RunStatusQueued {
				t.Fatalf("after a's crash: %d runs, want one queued", len(runs))
			}
			m.clock.Add(m.policy.ClaimTTL)
			m.tick(t, m.worker("b", m.st))
			p := m.pair(t)
			exactlyOnePublishedRun(t, m.f, p)
			if lateLanding {
				release <- nil
				<-aDone
				if p2 := m.pair(t); p2.RunID != p.RunID || p2.Generation != p.Generation {
					t.Fatalf("a's late return rewrote the pair: %+v → %+v", p, p2)
				}
				exactlyOnePublishedRun(t, m.f, m.pair(t))
			}
			if n := len(m.f.runs(t)); n != 1 {
				t.Fatalf("%d runs, want one", n)
			}
		})
	}
}

// A worker that dies after its submission was acknowledged and before its
// mark leaves a published run; the reclaim submits the same key and is
// answered by it. Built in both orders of the runner's claim: before the
// reclaim (acknowledged, nothing republished) and after it (completed under
// the same identity, which the broker and the runner absorb).
func TestPairMachine_ACrashBetweenPublicationAndTheMarkEndsWithOnePublishedRun(t *testing.T) {
	for _, runnerFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("runner claims first=%v", runnerFirst), func(t *testing.T) {
			m := newMachine(t)
			m.tick(t, m.worker("a", lostMarks{m.st}))
			msgs := m.f.q.messages()
			if len(msgs) != 1 || m.pair(t).State != intake.PairClaimed {
				t.Fatalf("after a's crash: %d messages, pair %+v — want one publication, the pair still claimed", len(msgs), m.pair(t))
			}
			if runnerFirst {
				m.f.claimByRunner(t, msgs[0].RunID)
			}
			m.clock.Add(m.policy.ClaimTTL)
			m.tick(t, m.worker("b", m.st))
			p := m.pair(t)
			exactlyOnePublishedRun(t, m.f, p)
			if p.RunID != msgs[0].RunID {
				t.Fatalf("the reclaim launched %s, want a's run %s", p.RunID, msgs[0].RunID)
			}
			want := 2
			if runnerFirst {
				want = 1
			}
			if n := len(m.f.q.messages()); n != want {
				t.Fatalf("%d messages, want %d", n, want)
			}
			if n := len(m.f.runs(t)); n != 1 {
				t.Fatalf("%d runs, want one", n)
			}
		})
	}
}

// A launch error the publisher returned ends its run LAUNCH_FAILED; the pair
// retries with backoff, meets the spent id, moves to its next epoch and
// launches there once. Past its attempts it lands in the dead letter, and a
// replay launches it once.
func TestPairMachine_AReturnedLaunchErrorRetriesThenBuriesThenReplaysOnce(t *testing.T) {
	brokerDown := errors.New("broker refused the message")

	t.Run("retry", func(t *testing.T) {
		m := newMachine(t)
		m.f.q.hold = func(n int, _ *queue.RunMessage) error {
			if n == 1 {
				return brokerDown
			}
			return nil
		}
		w := m.worker("w", m.st)
		m.tick(t, w)
		p := m.pair(t)
		if p.State != intake.PairPending || p.Attempts != 1 || p.Epoch != 0 {
			t.Fatalf("after the returned error: %+v, want a retry in epoch 0", p)
		}
		m.clock.Add(m.policy.Delay(1))
		m.tick(t, w)
		if p = m.pair(t); p.Epoch != 1 || p.Attempts != 2 || p.State != intake.PairPending {
			t.Fatalf("after meeting the spent id: %+v, want epoch 1", p)
		}
		m.clock.Add(m.policy.Delay(2))
		m.tick(t, w)
		p = m.pair(t)
		exactlyOnePublishedRun(t, m.f, p)
		if n := len(m.f.q.messages()); n != 1 {
			t.Fatalf("%d messages, want 1", n)
		}
		failed := 0
		for _, r := range m.f.runs(t) {
			if r.Status == store.RunStatusFailed && r.FailureCode == store.FailureLaunchFailed {
				failed++
			}
		}
		if failed != 1 {
			t.Fatalf("%d LAUNCH_FAILED runs, want the first attempt's, kept visible", failed)
		}
	})

	t.Run("dead letter and replay", func(t *testing.T) {
		m := newMachine(t)
		down := true
		m.f.q.hold = func(int, *queue.RunMessage) error {
			if down {
				return brokerDown
			}
			return nil
		}
		w := m.worker("w", m.st)
		for i := 1; i <= m.policy.MaxAttempts; i++ {
			m.tick(t, w)
			m.clock.Add(m.policy.Delay(i))
		}
		p := m.pair(t)
		if p.State != intake.PairDead || len(m.f.q.messages()) != 0 {
			t.Fatalf("after %d attempts: %+v, %d messages — want the dead letter, nothing published", m.policy.MaxAttempts, p, len(m.f.q.messages()))
		}
		down = false
		if _, err := m.st.ReplayPair(context.Background(), "team-a", p.ID, m.clock.Now()); err != nil {
			t.Fatal(err)
		}
		m.tick(t, w)
		m.tick(t, w)
		exactlyOnePublishedRun(t, m.f, m.pair(t))
		if n := len(m.f.q.messages()); n != 1 {
			t.Fatalf("the replay published %d messages, want 1", n)
		}
	})
}
