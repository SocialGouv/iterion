package cloudpublisher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
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

// claimAttempt claims the run for one delivered message, as a runner does:
// through the store's attempt-guarded claim, which takes a queued run only
// for a delivery published at or after its queued_at.
func (f *submissionFixture) claimAttempt(t *testing.T, msg queue.RunMessage) {
	t.Helper()
	if !f.tryClaimAttempt(t, msg) {
		t.Fatalf("the runner's claim of %s for the delivery published %s was refused", msg.RunID, msg.PublishedAtRFC)
	}
}

func (f *submissionFixture) tryClaimAttempt(t *testing.T, msg queue.RunMessage) bool {
	t.Helper()
	published, err := time.Parse(time.RFC3339Nano, msg.PublishedAtRFC)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := f.st.ClaimQueuedRunIfAttempt(f.ctx, msg.RunID, published)
	if err != nil {
		t.Fatal(err)
	}
	return ok
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

// A queued run whose publication never happened is COMPLETED by its key as a
// new attempt: the same row at the next version, created when it was, queued
// now, published under an identity of its own.
func TestSubmitLaunch_CompletesAPersistedUnpublishedRun(t *testing.T) {
	f := newSubmissionFixture(t)
	id := runview.SubmissionRunID("team-a", "k")
	f.crashAfterPersist(t, func() { _, _ = f.svc.Launch(f.ctx, f.spec("k")) })
	before, err := f.st.LoadRun(f.ctx, id)
	if err != nil || before.Status != store.RunStatusQueued || len(f.q.messages()) != 0 {
		t.Fatalf("after the crash: run %+v (%v), %d messages — want a queued run, nothing published", before, err, len(f.q.messages()))
	}
	if !runview.QueuedBySubmission(before) {
		t.Fatalf("the launch's own row = %+v, want it recorded as queued by its submission's attempt", before)
	}

	res, err := f.svc.Launch(f.ctx, f.spec("k"))
	if err != nil || res.RunID != id || res.AlreadySubmitted || res.Status != store.RunStatusQueued {
		t.Fatalf("completion = (%+v, %v), want the run published by this call", res, err)
	}
	after, _ := f.st.LoadRun(f.ctx, id)
	if after.CASVersion != before.CASVersion+1 || !after.CreatedAt.Equal(before.CreatedAt) || !after.QueuedAt.After(*before.QueuedAt) || !runview.QueuedBySubmission(after) {
		t.Fatalf("completed row: version %d→%d, created_at %v→%v, queued_at %v→%v — want the same row at the next version, created when it was, queued anew by this attempt",
			before.CASVersion, after.CASVersion, before.CreatedAt, after.CreatedAt, before.QueuedAt, after.QueuedAt)
	}
	msgs := f.q.messages()
	if len(msgs) != 1 || msgs[0].LaunchAttempt == "" || natsq.MessageID(&msgs[0]) == id {
		t.Fatalf("published %+v, want one launch message under an identity of its own attempt", msgs)
	}
	oneExecutableAttempt(t, f, id)
	if n := len(f.runs(t)); n != 1 {
		t.Fatalf("%d runs, want one", n)
	}
}

// A keyed submission meeting a run a resume re-queued answers from it: that
// run was handed on, and completing it would rebuild it from scratch under a
// launch message of its own. Built with and without a checkpoint — a run that
// failed before its first node re-queues with none, and only the attempt
// marker then tells it from the launch's own queued row.
func TestSubmitLaunch_NeverCompletesARunAResumeRequeued(t *testing.T) {
	for _, withCheckpoint := range []bool{true, false} {
		t.Run(fmt.Sprintf("checkpoint=%v", withCheckpoint), func(t *testing.T) {
			f := newSubmissionFixture(t)
			id := runview.SubmissionRunID("team-a", "k")
			if _, err := f.svc.Launch(f.ctx, f.spec("k")); err != nil {
				t.Fatal(err)
			}
			f.claimAttempt(t, f.q.messages()[0])
			var cp *store.Checkpoint
			if withCheckpoint {
				cp = &store.Checkpoint{NodeID: "step2", Outputs: map[string]map[string]any{"step1": {"done": true}}}
			}
			if err := f.st.FailRunResumable(f.ctx, id, cp, "provider 529", store.FailureCode("PROVIDER_OVERLOADED")); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := f.st.FlipToQueued(f.ctx, id, store.RunStatusFailedResumable, time.Now()); err != nil || !ok {
				t.Fatalf("resume flip = (%v, %v)", ok, err)
			}
			resumed, _ := f.st.LoadRun(f.ctx, id)
			if (resumed.Checkpoint != nil) != withCheckpoint {
				t.Fatalf("premise: the resumed run's checkpoint = %v", resumed.Checkpoint)
			}
			res, err := f.svc.Launch(f.ctx, f.spec("k"))
			if err != nil || !res.AlreadySubmitted {
				t.Fatalf("keyed submission over a resumed run = (%+v, %v), want it acknowledged", res, err)
			}
			after, _ := f.st.LoadRun(f.ctx, id)
			if after.CASVersion != resumed.CASVersion || (after.Checkpoint != nil) != withCheckpoint || len(f.q.messages()) != 1 {
				t.Fatalf("the resumed run was rewritten (version %d→%d, checkpoint %v) or published again (%d messages)",
					resumed.CASVersion, after.CASVersion, after.Checkpoint, len(f.q.messages()))
			}
		})
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

// oneExecutableAttempt: of the messages that landed for the run, exactly one
// can still be executed — the runner's admission proceeds on it and the
// store's attempt-guarded claim takes the run for it — and every other one is
// retired, its claim refused. No two attempts share a broker identity, so the
// dedup window can never drop the live one for a retired one. It ends with
// the run claimed by the live attempt.
func oneExecutableAttempt(t *testing.T, f *submissionFixture, runID string) {
	t.Helper()
	r, err := f.st.LoadRun(f.ctx, runID)
	if err != nil {
		t.Fatalf("run %s: %v", runID, err)
	}
	if r.Status != store.RunStatusQueued {
		t.Fatalf("run %s is %s (%s: %s), want it queued for its attempt", runID, r.Status, r.FailureCode, r.Error)
	}
	identity := map[string]string{}
	var live, retired []queue.RunMessage
	for _, msg := range f.q.messages() {
		if msg.RunID != runID {
			continue
		}
		if at, ok := identity[natsq.MessageID(&msg)]; ok && at != msg.PublishedAtRFC {
			t.Fatalf("two attempts of %s share the broker identity %s: the dedup window would drop one for the other", runID, natsq.MessageID(&msg))
		}
		identity[natsq.MessageID(&msg)] = msg.PublishedAtRFC
		if !queue.Superseded(&msg, r) && queue.Admit(&msg, r).Proceeds() {
			live = append(live, msg)
		} else {
			retired = append(retired, msg)
		}
	}
	if len(live) != 1 {
		t.Fatalf("%d executable attempts of %s (and %d retired), want exactly one", len(live), runID, len(retired))
	}
	for _, msg := range retired {
		if f.tryClaimAttempt(t, msg) {
			t.Fatalf("a retired attempt (published %s) claimed run %s", msg.PublishedAtRFC, runID)
		}
	}
	f.claimAttempt(t, live[0])
}

func launchedOnce(t *testing.T, f *submissionFixture, p intake.Pair) {
	t.Helper()
	if p.State != intake.PairLaunched || p.RunID == "" {
		t.Fatalf("pair = %+v, want launched", p)
	}
	oneExecutableAttempt(t, f, p.RunID)
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
	launchedOnce(t, m.f, m.pair(t))
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
			if lateLanding {
				// a was only slow: its publication lands after the
				// completion, and its return finds its claim gone.
				release <- nil
				<-aDone
				if p2 := m.pair(t); p2.RunID != p.RunID || p2.Generation != p.Generation {
					t.Fatalf("a's late return rewrote the pair: %+v → %+v", p, p2)
				}
				if n := len(m.f.q.messages()); n != 2 {
					t.Fatalf("%d messages, want a's late one and the completion's", n)
				}
			}
			launchedOnce(t, m.f, p)
			if n := len(m.f.runs(t)); n != 1 {
				t.Fatalf("%d runs, want one", n)
			}
		})
	}
}

// A worker that dies after its submission was acknowledged and before its
// mark leaves a published run; the reclaim submits the same key. Built in both
// orders of the runner's claim: before the reclaim, the run was handed on and
// the key is answered by it, nothing republished; after it, the run is still
// queued by its attempt and is completed as a new one, which retires the
// first message.
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
				m.f.claimAttempt(t, msgs[0])
			}
			m.clock.Add(m.policy.ClaimTTL)
			m.tick(t, m.worker("b", m.st))
			p := m.pair(t)
			if p.State != intake.PairLaunched || p.RunID != msgs[0].RunID {
				t.Fatalf("the reclaim = %+v, want a's run %s", p, msgs[0].RunID)
			}
			if runnerFirst {
				if r, _ := m.f.st.LoadRun(m.f.ctx, p.RunID); r.Status != store.RunStatusRunning || len(m.f.q.messages()) != 1 {
					t.Fatalf("run %s, %d messages — want the claimed run answered, nothing republished", r.Status, len(m.f.q.messages()))
				}
			} else {
				if n := len(m.f.q.messages()); n != 2 {
					t.Fatalf("%d messages, want a's and the completion's", n)
				}
				launchedOnce(t, m.f, p)
			}
			if n := len(m.f.runs(t)); n != 1 {
				t.Fatalf("%d runs, want one", n)
			}
		})
	}
}

// A first message that landed, retired by a completion, stays retired: once
// the completion's attempt ran and an operator paused the run, the late
// delivery of the first message is not admitted — neither as a launch nor
// as a resume of the pause.
func TestPairMachine_ARetiredAttemptNeverResumesTheRun(t *testing.T) {
	m := newMachine(t)
	m.tick(t, m.worker("a", lostMarks{m.st}))
	first := m.f.q.messages()[0]
	m.clock.Add(m.policy.ClaimTTL)
	m.tick(t, m.worker("b", m.st))
	msgs := m.f.q.messages()
	if len(msgs) != 2 {
		t.Fatalf("%d messages, want the first and the completion's", len(msgs))
	}
	m.f.claimAttempt(t, msgs[1])
	cp := &store.Checkpoint{NodeID: "step2", Outputs: map[string]map[string]any{"step1": {"x": 1}}}
	if err := m.f.st.PauseRun(m.f.ctx, first.RunID, cp); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.f.st.UpdateRunStatusIfCoded(m.f.ctx, first.RunID, store.RunStatusPausedOperator, "operator pause", "", []store.RunStatus{store.RunStatusPausedWaitingHuman}); err != nil || !ok {
		t.Fatalf("operator pause = (%v, %v)", ok, err)
	}
	r, _ := m.f.st.LoadRun(m.f.ctx, first.RunID)
	if adm := queue.Admit(&first, r); adm.Proceeds() {
		t.Fatalf("the retired first message is admitted on the paused run: %+v", adm)
	}
}

// A runner claimed the first publication although its acknowledgement was
// lost; the run executed, parked and was resumed. The worker's retry of the
// same key meets a run a resume re-queued and is answered by it — the
// checkpoint kept, nothing republished.
func TestPairMachine_ARetryNeverCompletesARunAResumeRequeued(t *testing.T) {
	m := newMachine(t)
	ackLost := errors.New("nats: timeout waiting for the publish ack")
	m.f.q.hold = func(n int, msg *queue.RunMessage) error {
		if n == 1 {
			// Landed, and taken by a runner, before the acknowledgement
			// was lost.
			m.f.q.landed = append(m.f.q.landed, *msg)
			m.f.claimAttempt(t, *msg)
			return ackLost
		}
		return nil
	}
	w := m.worker("w", m.st)
	m.tick(t, w)
	p := m.pair(t)
	if p.State != intake.PairPending || p.Attempts != 1 || p.Epoch != 0 {
		t.Fatalf("after the lost acknowledgement: %+v, want a retry of the same key", p)
	}
	id := runview.SubmissionRunID("team-a", p.SubmissionKey())
	cp := &store.Checkpoint{NodeID: "step2", Outputs: map[string]map[string]any{"step1": {"posted_comment": true}}}
	if err := m.f.st.FailRunResumable(m.f.ctx, id, cp, "provider 529", store.FailureCode("PROVIDER_OVERLOADED")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := m.f.st.FlipToQueued(m.f.ctx, id, store.RunStatusFailedResumable, time.Now()); err != nil || !ok {
		t.Fatalf("resume flip = (%v, %v)", ok, err)
	}
	m.clock.Add(m.policy.Delay(1))
	m.tick(t, w)
	p = m.pair(t)
	after, _ := m.f.st.LoadRun(m.f.ctx, id)
	if p.State != intake.PairLaunched || p.RunID != id || after.Checkpoint == nil || len(m.f.q.messages()) != 1 {
		t.Fatalf("pair %+v, checkpoint %v, %d messages — want the pair answered by its resumed run, untouched", p, after.Checkpoint, len(m.f.q.messages()))
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
		launchedOnce(t, m.f, p)
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
		// The replay submits the dead letter's own key, whose run the last
		// attempt left LAUNCH_FAILED: the id is spent, the next attempt
		// takes the next key, and launches once.
		m.tick(t, w)
		if p = m.pair(t); p.State != intake.PairPending || p.Epoch != 2 {
			t.Fatalf("after the replay met its spent key: %+v, want a retry in the next epoch", p)
		}
		m.clock.Add(m.policy.Delay(1))
		m.tick(t, w)
		launchedOnce(t, m.f, m.pair(t))
		if n := len(m.f.q.messages()); n != 1 {
			t.Fatalf("the replay published %d messages, want 1", n)
		}
	})
}

// liveRuns counts the runs that did not end at their launch path.
func liveRuns(t *testing.T, f *submissionFixture) int {
	t.Helper()
	n := 0
	for _, r := range f.runs(t) {
		if r.Status != store.RunStatusFailed || r.FailureCode != store.FailureLaunchFailed {
			n++
		}
	}
	return n
}

// ackLostSubmitter launches for real, then answers as if the acknowledgement
// never reached the worker.
type ackLostSubmitter struct{ inner launchSubmitter }

func (s ackLostSubmitter) Submit(ctx context.Context, in intake.Intake, p intake.Pair) (string, error) {
	if _, err := s.inner.Submit(ctx, in, p); err != nil {
		return "", err
	}
	return "", context.DeadlineExceeded
}

// The last attempt's submission is handed on but its answer is lost — the
// acknowledgement timed out, or the worker died before its mark: the pair is
// buried over a live run. Its replay submits the same key and is answered by
// that run, never by a second one.
func TestPairMachine_AReplayOfADeadPairOverALiveRunStartsNoSecond(t *testing.T) {
	brokerDown := errors.New("broker refused the message")
	lastAttempt := map[string]func(m *machine) intake.Store{
		"answer lost":          func(m *machine) intake.Store { return m.st },
		"died before the mark": func(m *machine) intake.Store { return lostMarks{m.st} },
	}
	for name, storeOf := range lastAttempt {
		t.Run(name, func(t *testing.T) {
			m := newMachine(t)
			m.f.q.hold = func(n int, _ *queue.RunMessage) error {
				if n == 1 {
					return brokerDown
				}
				return nil
			}
			w := m.worker("w", m.st)
			m.tick(t, w) // the broker refuses: LAUNCH_FAILED
			m.clock.Add(m.policy.Delay(1))
			m.tick(t, w) // the spent id: next epoch
			m.clock.Add(m.policy.Delay(2))
			last := m.worker("last", storeOf(m))
			if name == "answer lost" {
				last.Submitter = ackLostSubmitter{launchSubmitter{m.f}}
			}
			m.tick(t, last) // handed on, its answer lost
			if name == "died before the mark" {
				m.clock.Add(m.policy.ClaimTTL)
				m.tick(t, w) // the reclaim spends the last attempt
			}
			p := m.pair(t)
			if p.State != intake.PairDead || liveRuns(t, m.f) != 1 {
				t.Fatalf("after the last attempt: %+v, %d live runs — want the dead letter over one live run", p, liveRuns(t, m.f))
			}
			if _, err := m.st.ReplayPair(context.Background(), "team-a", p.ID, m.clock.Now()); err != nil {
				t.Fatal(err)
			}
			m.tick(t, w)
			p = m.pair(t)
			if n := liveRuns(t, m.f); n != 1 {
				t.Fatalf("the replay left %d live runs, want the one its key names", n)
			}
			launchedOnce(t, m.f, p)
		})
	}
}

// A completion retires the first attempt's pool lease, as a resume does —
// that attempt can no longer claim the run — whether the completion is
// granted a lease of its own (whose acquisition supersedes the first) or
// none (the donor's daily runs spent: it supersedes the first one itself).
// The run's one executed attempt leaves no lease open once it reported.
func TestSubmitLaunch_ACompletionRetiresTheFirstAttemptsPoolLease(t *testing.T) {
	for name, runsPerDay := range map[string]int{"granted again": 5, "granted nothing": 1} {
		t.Run(name, func(t *testing.T) {
			f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5, MaxRunsPerDay: runsPerDay})
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			q := &queueRecorder{}
			f.pub.store, f.pub.publishRun, f.pub.publishRetryDelays = st, q.publish, []time.Duration{}
			ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
			if err := f.pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true,
				Audience: credpool.Audience{Teams: []string{poolTeam}}}); err != nil {
				t.Fatal(err)
			}
			svc, err := runview.NewService(t.TempDir(), runview.WithLogger(iterlog.Nop()), runview.WithStore(st), runview.WithLaunchPublisher(f.pub))
			if err != nil {
				t.Fatal(err)
			}
			spec := runview.LaunchSpec{FilePath: "demo.bot", Source: submissionBot, SubmissionKey: "k"}
			id := runview.SubmissionRunID(poolTeam, "k")
			if _, err := svc.Launch(ctx, spec); err != nil {
				t.Fatal(err)
			}
			first, err := f.leases.GetOpenByRun(ctx, id)
			if err != nil {
				t.Fatalf("the first attempt holds no pool lease: %v", err)
			}
			// The worker died before its mark; the reclaim re-submits the
			// key while the first message waits unclaimed.
			if _, err := svc.Launch(ctx, spec); err != nil {
				t.Fatal(err)
			}
			msgs := q.messages()
			if len(msgs) != 2 {
				t.Fatalf("%d messages, want the first and the completion's", len(msgs))
			}
			if granted := msgs[1].PoolAttemptID != ""; granted != (runsPerDay > 1) {
				t.Fatalf("the completion's pool attempt = %q, want a grant only while the donor has runs left", msgs[1].PoolAttemptID)
			}
			all, _ := f.leases.ListByRun(ctx, id)
			if l := findPoolLease(all, first.ID); l == nil || !l.Closed {
				t.Fatalf("the first attempt's lease = %+v, want it closed by the completion", l)
			}
			published := func(m queue.RunMessage) time.Time {
				v, err := time.Parse(time.RFC3339Nano, m.PublishedAtRFC)
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			if ok, err := st.ClaimQueuedRunIfAttempt(ctx, id, published(msgs[0])); err != nil || ok {
				t.Fatalf("the retired first attempt claimed run %s: (%v, %v)", id, ok, err)
			}
			if ok, err := st.ClaimQueuedRunIfAttempt(ctx, id, published(msgs[1])); err != nil || !ok {
				t.Fatalf("the completion's attempt could not claim the run: (%v, %v)", ok, err)
			}
			if msgs[1].PoolAttemptID != "" {
				if err := f.pub.credPool.ReportAttemptID(ctx, id, msgs[1].PoolAttemptID, published(msgs[1]), credpool.Outcome{CostUSD: 1}); err != nil {
					t.Fatalf("report the executed attempt: %v", err)
				}
			}
			if open, _ := f.leases.ListOpenByRun(ctx, id); len(open) != 0 {
				t.Fatalf("%d pool leases still open after the run's one executed attempt", len(open))
			}
		})
	}
}

func findPoolLease(ls []credpool.Lease, id string) *credpool.Lease {
	for i := range ls {
		if ls[i].ID == id {
			return &ls[i]
		}
	}
	return nil
}
