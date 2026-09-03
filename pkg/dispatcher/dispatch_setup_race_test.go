package dispatcher

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// nopLeaser is a lock-free tracker that also satisfies tracker.ClaimLeaser,
// so the dispatcher resolves c.leaser and dispatch attaches a claimSession
// to the runningEntry — the native/Mongo shape, minus any internal mutex
// that would forge a happens-before edge the race detector could absorb.
type nopLeaser struct{}

func (nopLeaser) Name() string { return "nop" }
func (nopLeaser) ListCandidates(context.Context) ([]tracker.Issue, error) {
	return nil, nil
}
func (nopLeaser) RefreshStates(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		out[id] = "in_progress"
	}
	return out, nil
}
func (nopLeaser) UpdateState(context.Context, string, string) error { return nil }
func (nopLeaser) Comment(context.Context, string, string) error     { return nil }
func (nopLeaser) Claim(context.Context, string, string) error       { return nil }
func (nopLeaser) Release(context.Context, string, string) error     { return nil }
func (nopLeaser) ClaimLease(context.Context, string, string) (tracker.ClaimToken, error) {
	return tracker.ClaimToken{Marker: "m", Epoch: 1}, nil
}
func (nopLeaser) RenewClaim(context.Context, string, tracker.ClaimToken) error   { return nil }
func (nopLeaser) ReleaseOwned(context.Context, string, tracker.ClaimToken) error { return nil }
func (nopLeaser) UpdateStateOwned(context.Context, string, string, tracker.ClaimToken) error {
	return nil
}

// TestRVAT18_RaceSetupWorkerVsShutdownStopClaimSession
//
// runDispatchSetup runs OFF the actor (ADR-028 Step 4) and reads
// plan.entry.claim (loop.go, fencedUpdateState call site). shutdown()'s
// per-card drain goroutine WRITES the same field (stopClaimSession sets
// r.claim = nil). Both touch the same *runningEntry with no
// synchronisation. Run with -race.
func TestRVAT18_RaceSetupWorkerVsShutdownStopClaimSession(t *testing.T) {
	ws, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Name: "x", Workflow: t.TempDir() + "/f.bot",
		Agent:     AgentConfig{MaxConcurrent: 4, RunningState: "in_progress"},
		Workspace: WorkspaceConfig{Root: t.TempDir()}}
	cfg.applyDefaults()
	cfg.Agent.RunningState = "in_progress"

	for i := 0; i < 50; i++ {
		c, err := New(Options{Config: cfg, Tracker: nopLeaser{}, Runner: &StubRunner{},
			Workspaces: ws, Logger: quietLogger(), HostMarker: "h-1"})
		if err != nil {
			t.Fatal(err)
		}
		if c.leaser == nil {
			t.Fatal("leaser not resolved — probe premise broken")
		}
		sess := StartClaimSession(c.leaser, "i1", tracker.ClaimToken{Marker: "h-1", Epoch: 1},
			func(string, ...any) {}, nil)
		entry := &runningEntry{IssueID: "i1", Identifier: "i1", WorkflowState: "ready", claim: sess}
		c.state.running["i1"] = entry

		// The plan is built ON the actor, exactly like dispatch() does.
		plan := dispatchSetupPlan{
			issueID: "i1", identifier: "i1",
			sourceState: "ready", runningTarget: "in_progress",
			runCtx: context.Background(), entry: entry, session: entry.claim,
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.runDispatchSetup(plan)
		}()
		go func() {
			defer wg.Done()
			c.shutdown()
		}()
		wg.Wait()
		time.Sleep(time.Millisecond)
	}
}

// conflictLeaser is nopLeaser whose fenced state write reports the claim
// is no longer ours — the shape of another daemon, an operator, or the
// watchdog having taken the card, and of the release-N mixed fleet where
// an old binary's full-document write strips the epoch (ADR §6).
type conflictLeaser struct{ nopLeaser }

func (conflictLeaser) UpdateStateOwned(context.Context, string, string, tracker.ClaimToken) error {
	return tracker.ErrClaimConflict
}

// TestRunDispatchSetup_ClaimConflictAbortsTheLaunch: the in-progress
// transition is best-effort "because the claim is already taken" — a
// premise ErrClaimConflict INVERTS. It is the one error proving this
// worker no longer owns the card, so continuing starts a second run on
// it while every later fenced write (the finish transition, the release)
// is refused: the card ends up neither filed nor released, and the
// heartbeat only notices a lease-third later.
func TestRunDispatchSetup_ClaimConflictAbortsTheLaunch(t *testing.T) {
	ws, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Name: "x", Workflow: t.TempDir() + "/f.bot",
		Agent:     AgentConfig{MaxConcurrent: 4, RunningState: "in_progress"},
		Workspace: WorkspaceConfig{Root: t.TempDir()}}
	cfg.applyDefaults()
	cfg.Agent.RunningState = "in_progress"
	c, err := New(Options{Config: cfg, Tracker: conflictLeaser{}, Runner: &StubRunner{},
		Workspaces: ws, Logger: quietLogger(), HostMarker: "h-1"})
	if err != nil {
		t.Fatal(err)
	}
	sess := StartClaimSession(c.leaser, "i1", tracker.ClaimToken{Marker: "h-1", Epoch: 1},
		func(string, ...any) {}, nil)
	defer sess.Stop()
	entry := &runningEntry{IssueID: "i1", Identifier: "i1", WorkflowState: "ready", claim: sess}
	c.state.running["i1"] = entry

	created, ok := c.runDispatchSetup(dispatchSetupPlan{
		issueID: "i1", identifier: "i1",
		sourceState: "ready", runningTarget: "in_progress",
		runCtx: context.Background(), entry: entry, session: sess,
	})

	if ok {
		t.Fatal("setup reported OK after the fence refused the move — the run starts on a card this worker no " +
			"longer owns, and every later fenced write is refused: never filed, never released")
	}
	if created {
		t.Fatal("a workspace was created for a launch that must not happen")
	}
}
