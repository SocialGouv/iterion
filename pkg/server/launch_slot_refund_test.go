package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// #1725: the three sibling launch surfaces (the trigger spine, the board
// dispatcher, the retry sweeper) keep the metered slot when the error
// itself says a run may have started — RunMayHaveStarted reads the fact
// off the error (a queue publish that reports failure after the message
// landed) instead of inferring it from err != nil — and hand the slot
// back only when the launch is proven not to have started. The HTTP
// handler's both-directions witness lives in
// runs_launch_quota_release_test.go; these drive each sibling through its
// own seam on the keep-direction, which is the direction the ticket calls
// worse: refunding a slot a real run is using under-counts the org.

// landingPublishStub is a publisher whose submit reports failure AFTER the
// message landed — the exact shape the ticket quotes (an ack timeout, a
// context cancelled past the publish): the runner may claim and execute
// the revision this call published.
type landingPublishStub struct {
	err error
}

func (p *landingPublishStub) SubmitLaunch(context.Context, string, runview.LaunchSpec, *ir.Workflow, *runview.CompiledSource) (int, error) {
	return 1, p.err
}
func (p *landingPublishStub) CancelRun(context.Context, string) error { return nil }
func (p *landingPublishStub) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (p *landingPublishStub) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, *runview.CompiledSource) error {
	return p.err
}

func queueLandedError() error {
	return &runview.QueueUnavailableError{Cause: errors.New("PROBE: ack timeout after the message landed")}
}

func freshRuns(t *testing.T, pub runview.LaunchPublisher) *runview.Service {
	t.Helper()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
}

// TestTriggerSpineKeepsTheSlotOnlyWhenARunMayHaveStarted: the spine's two
// directions back to back — a launch refused before anything durable
// refunds the unit (a looping failure must not burn the org's month), and
// a publish that landed but reported failure keeps it.
func TestTriggerSpineKeepsTheSlotOnlyWhenARunMayHaveStarted(t *testing.T) {
	s, counter, ctx, _ := newQuotaReleaseServer(t)
	newLauncher := func(publishErr error) *serviceLauncher {
		s.runs = freshRuns(t, &landingPublishStub{err: publishErr})
		return &serviceLauncher{
			runs:   s.runs,
			logger: s.logger,
			gate:   s.gateLaunch,
			resolveBot: func(context.Context, string, string) (*launchBot, error) {
				return &launchBot{BotID: "review-pr", Origin: "catalog", Path: "catalog/review-pr/main.bot", Source: aWorkflow}, nil
			},
		}
	}
	plan := trigger.LaunchPlan{BotID: "review-pr", TenantID: "t1"}

	// A publish that never landed: nothing durable exists, the slot goes back.
	if _, err := newLauncher(errors.New("PROBE: NATS refused the publish")).Launch(ctx, plan); err == nil {
		t.Fatal("the failing publish did not fail the launch")
	}
	if got := meteredRuns(t, counter); got != 0 {
		t.Fatalf("monthly runs = %d after a publish that landed nothing, want 0 — a looping failure must not burn the org's month", got)
	}

	// A publish that LANDED and then reported failure: the slot stays.
	if _, err := newLauncher(queueLandedError()).Launch(ctx, plan); err == nil {
		t.Fatal("the landed-but-failed publish did not fail the launch")
	}
	if got := meteredRuns(t, counter); got != 1 {
		t.Fatalf("monthly runs = %d after a publish that landed, want 1 — the error says a run may exist, the slot must stay", got)
	}
}

// TestRetrySweeperKeepsTheSlotWhenARunMayHaveStarted: the sweeper's resume
// re-arms the attempt, but the metered slot follows the error's own fact —
// a publish that landed keeps the slot the admission charged.
func TestRetrySweeperKeepsTheSlotWhenARunMayHaveStarted(t *testing.T) {
	s, counter, _, _ := newQuotaReleaseServer(t)
	// The sweeper reads the run doc and claims the retry on cfg.Store, and
	// resolves the resume source beside WorkDir — the shapes
	// newRetrySweeperServer's fixture carries.
	// cfg.Store carries the retry store AND the run-doc reads the sweeper
	// makes: the fake answers them the way the proven harness does (a
	// failed_resumable doc for run-a).
	fake := newFakeRetryStore()
	fake.claimWins["run-a"] = true
	s.cfg.Store = fake
	s.cfg.WorkDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(s.cfg.WorkDir, "bots", "feed-watch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.WorkDir, "bots", "feed-watch", "main.bot"), []byte("workflow w:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// dueRef names the tenant the sweeper launches as; the gate meters on
	// that team, so it is the one seeded here.
	seedGate(t, s, gateSpec{id: "team-1"})

	resumer := &fakeResumer{failing: queueLandedError()}

	at := time.Now().UTC().Add(-time.Minute)
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-a", at)}}, resumer, time.Now().UTC())

	if len(resumer.calls) != 1 {
		t.Fatalf("Resume called %d times, want 1", len(resumer.calls))
	}
	u, err := counter.Usage(context.Background(), orgusage.OrgSubject("team-1"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if u.Runs != 1 {
		t.Fatalf("monthly runs = %d after a resume whose publish landed, want 1 — the slot of a run the runner may be executing must not be handed back", u.Runs)
	}
}

// TestBoardDispatchKeepsTheSlotWhenARunMayHaveStarted drives
// processBoardCard end to end with a queue that landed the message and
// then reported failure: the card takes the refusal, the metered slot
// stays — a runner may already be executing that revision.
func TestBoardDispatchKeepsTheSlotWhenARunMayHaveStarted(t *testing.T) {
	s, counter, _, _ := newQuotaReleaseServer(t)
	botsDir := t.TempDir()
	// worktree: none — the fixture only asserts on metering, and a worktree
	// here would fork one off the live checkout.
	boardBot := "tool noop:\n  command: `printf '{}'`\n\nworkflow board_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n"
	if err := os.WriteFile(filepath.Join(botsDir, "fixer.bot"), []byte(boardBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	s.runs = freshRuns(t, &landingPublishStub{err: queueLandedError()})

	cardCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var cardErr error
	go func() {
		defer close(done)
		cardErr = s.processBoardCard(cardCtx, "t1", native.Issue{ID: "card1", Bot: "fixer", State: native.StateReady})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("processBoardCard did not return after a refused launch")
	}
	cancel()
	<-done

	// The card takes the refusal (launchRefusal): the launch failed, the
	// slot is what the assertion below pins.
	if cardErr == nil {
		t.Fatal("processBoardCard reported success on a launch whose publish failed")
	}
	if got := meteredRuns(t, counter); got != 1 {
		t.Fatalf("monthly runs = %d after a dispatch whose publish landed, want 1 — the refusal's slot must stay", got)
	}
}
