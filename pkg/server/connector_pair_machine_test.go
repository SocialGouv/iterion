package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// recordingPublisher records every run id it is asked to publish and fails
// the attempts its fail hook names.
type recordingPublisher struct {
	mu        sync.Mutex
	published []string
	fail      func(n int) error
}

func (p *recordingPublisher) SubmitLaunch(_ context.Context, runID string, _ runview.LaunchSpec, _ *ir.Workflow, _ *runview.CompiledSource) (runview.LaunchSubmission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.published)
	if p.fail != nil {
		if err := p.fail(n); err != nil {
			p.published = append(p.published, "")
			return runview.LaunchSubmission{}, err
		}
	}
	p.published = append(p.published, runID)
	return runview.LaunchSubmission{QueuePosition: 1}, nil
}
func (p *recordingPublisher) CancelRun(context.Context, string) error { return nil }
func (p *recordingPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (p *recordingPublisher) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, *runview.CompiledSource) error {
	return nil
}

// landed are the run ids a publication accepted, distinct.
func (p *recordingPublisher) landed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, id := range p.published {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// lossyMarks loses the first n launched marks: the run is published, the
// pair never hears of it.
type lossyMarks struct {
	intake.Store
	lose int
}

func (l *lossyMarks) MarkPairLaunched(ctx context.Context, id string, gen int64, runID string, at time.Time) error {
	if l.lose > 0 {
		l.lose--
		return errors.New("PROBE: the mark did not land")
	}
	return l.Store.MarkPairLaunched(ctx, id, gen, runID, at)
}

type pairClock struct{ now time.Time }

func (c *pairClock) Now() time.Time { return c.now }

type pairMachine struct {
	w     *intake.Worker
	store intake.Store
	mem   *intake.MemoryStore
	pub   *recordingPublisher
	clock *pairClock
}

func newPairMachine(t *testing.T, pub *recordingPublisher, wrap func(intake.Store) intake.Store) *pairMachine {
	t.Helper()
	s, _, ctx, _ := newQuotaReleaseServer(t)
	s.runs = freshRuns(t, pub)
	subs := trigger.NewMemorySubscriptionStore()
	if err := subs.Create(ctx, trigger.Subscription{ID: "s1", TenantID: "t1", BotID: "review-pr", Enabled: true,
		Invocation: bundle.InvocationKindConnector, ConnectionID: "conn-chat", TriggerID: "chat.message.posted",
		Filters: map[string][]string{"room": {"ops"}}}); err != nil {
		t.Fatal(err)
	}
	cat := connection.NewMemoryCatalog(submitChatPackage())
	matcher := &binding.Matcher{Subscriptions: subs, Catalog: func(string) connection.Catalog { return cat }}
	launcher := &serviceLauncher{
		launch: s.launchTriggered,
		logger: s.logger,
		gate:   s.gateLaunch,
		answer: s.answerTriggeredSubmission,
		resolveBot: func(context.Context, string, string) (*launchBot, error) {
			return &launchBot{BotID: "review-pr", Origin: "catalog", Path: "catalog/review-pr/main.bot", Source: aWorkflow}, nil
		},
	}
	mem := intake.NewMemoryStore()
	var st intake.Store = mem
	if wrap != nil {
		st = wrap(mem)
	}
	clock := &pairClock{now: time.Now().UTC()}
	policy := intake.Policy{MaxAttempts: 6, Backoff: time.Second, BackoffMax: time.Second, ClaimTTL: time.Minute}
	w := &intake.Worker{
		Store:   st,
		Matcher: matcher,
		Submitter: &connectorSubmitter{
			subs: subs, matcher: matcher, launcher: launcher,
			botVars:     func(context.Context, string, string) ([]string, bool, error) { return []string{"prompt"}, true, nil },
			repoTrusted: func(context.Context, intake.Intake, string) error { return nil },
		},
		Owner: "w-test", Intakes: policy, Pairs: policy, Now: clock.Now,
	}
	if _, _, err := mem.Accept(ctx, intake.Intake{TenantID: "t1", ConnectionID: "conn-chat", IngressID: "ing-1",
		TriggerID: "chat.message.posted", DedupKey: "d1", ReceivedAt: clock.now,
		Event: []byte(`{"outputs":{"prompt":"ship it"},"payload":{"room":"ops"},"headers":{}}`)}); err != nil {
		t.Fatal(err)
	}
	return &pairMachine{w: w, store: st, mem: mem, pub: pub, clock: clock}
}

// tick runs one matching pass and one launching pass, then moves the clock
// past every claim and backoff.
func (m *pairMachine) tick(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := m.w.MatchTick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.w.LaunchTick(ctx); err != nil {
		t.Fatal(err)
	}
	m.clock.now = m.clock.now.Add(2 * time.Minute)
}

func (m *pairMachine) pair(t *testing.T) intake.Pair {
	t.Helper()
	ps, err := m.mem.ListPairs(context.Background(), "t1", intake.PairQuery{}, 10)
	if err != nil || len(ps) != 1 {
		t.Fatalf("pairs = %+v, %v; want one", ps, err)
	}
	return ps[0]
}

// A run published whose pair never heard of it: the pair is reclaimed and
// submitted again under its key, and the run service answers with the run
// it already holds — one run, whatever the publications.
func TestThePairMachineLosesNoRunWhenItsMarkIsLost(t *testing.T) {
	m := newPairMachine(t, &recordingPublisher{}, func(s intake.Store) intake.Store { return &lossyMarks{Store: s, lose: 1} })
	for i := 0; i < 3; i++ {
		m.tick(t)
	}
	p := m.pair(t)
	landed := m.pub.landed()
	if p.State != intake.PairLaunched || len(landed) != 1 || p.RunID != landed[0] {
		t.Fatalf("pair %+v, landed runs %v; want the pair launched on the one run its key derived", p, landed)
	}
}

// A run persisted whose publication was refused: the run stays queued by
// its submission, and the retry under the same key completes that run —
// one run, published once, the refused attempt behind it.
func TestThePairMachineCompletesARunItsPublicationRefused(t *testing.T) {
	pub := &recordingPublisher{fail: func(n int) error {
		if n == 0 {
			return errors.New("PROBE: the broker refused the publish")
		}
		return nil
	}}
	m := newPairMachine(t, pub, nil)
	for i := 0; i < 4; i++ {
		m.tick(t)
	}
	p := m.pair(t)
	landed := pub.landed()
	if len(pub.published) < 2 {
		t.Fatalf("publications %v: the refused attempt never happened, the probe proves nothing", pub.published)
	}
	if p.State != intake.PairLaunched || len(landed) != 1 || p.RunID != landed[0] {
		t.Fatalf("pair %+v, landed runs %v; want launched on its one published run", p, landed)
	}
}

// A worker that claimed the pair and died before launching: the claim
// expires, another worker takes it, and the run launches once.
func TestThePairMachineSurvivesAWorkerDeadBeforeTheLaunch(t *testing.T) {
	m := newPairMachine(t, &recordingPublisher{}, nil)
	ctx := context.Background()
	if _, err := m.w.MatchTick(ctx); err != nil {
		t.Fatal(err)
	}
	if claimed, err := m.mem.ClaimPairs(ctx, "w-dead", m.clock.now, time.Minute, 5); err != nil || len(claimed) != 1 {
		t.Fatalf("the dead worker's claim = %d, %v", len(claimed), err)
	}
	m.tick(t) // within the dead worker's claim: nothing to take
	if landed := m.pub.landed(); len(landed) != 0 {
		t.Fatalf("a pair under another worker's live claim launched: %v", landed)
	}
	m.tick(t)
	p := m.pair(t)
	landed := m.pub.landed()
	if p.State != intake.PairLaunched || len(landed) != 1 || p.RunID != landed[0] {
		t.Fatalf("pair %+v, landed runs %v; want one launch after the claim expired", p, landed)
	}
}
