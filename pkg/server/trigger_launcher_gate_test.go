package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The trigger spine's direct launches (#844) — a `mode: direct` board
// trigger, a run-completion chain, the emit fan-out — pass the SAME admission
// every other cloud launch surface passes, under the subscription's own team,
// and meter the same monthly slot. Before this the spine was a hole through
// which an org launched past its suspend, its concurrency cap, its launch rate
// and its monthly caps, unmetered and under no tenant at all.

// spinePublisher records what each launch was published under and how many
// launches were attempted — the oracle for "the run service was never asked".
type spinePublisher struct {
	mu       sync.Mutex
	launches int
	err      error
	tenant   string
	owner    string
}

func (p *spinePublisher) SubmitLaunch(ctx context.Context, _ string, _ runview.LaunchSpec, _ *ir.Workflow, _ string) (int, error) {
	p.mu.Lock()
	p.launches++
	p.tenant, _ = store.TenantFromContext(ctx)
	p.owner, _ = store.OwnerFromContext(ctx)
	p.mu.Unlock()
	return 0, p.err
}

func (p *spinePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.launches
}

func (*spinePublisher) CancelRun(context.Context, string) error { return nil }
func (*spinePublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (*spinePublisher) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, string) error {
	return nil
}

const spineProbeBot = "schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"yes\"}'`\n  output: probe_out\n\nworkflow spine_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n"

// newGatedSpineServer is a cloud-shaped server carrying one org+team, the
// probe bot in its catalog, and a run service whose publisher is the caller's.
func newGatedSpineServer(t *testing.T, spec gateSpec, pub *spinePublisher) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	seedGate(t, s, spec)
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "probe.bot"), []byte(spineProbeBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	return s
}

func spinePlan(tenant string) trigger.LaunchPlan {
	return trigger.LaunchPlan{
		BotID:    "probe",
		TenantID: tenant,
		Mode:     bundle.ExecutionDirect,
		Event: trigger.Event{
			ID: "custom:probe:1", Source: trigger.SourceCustom, Kind: "probe",
			TenantID: tenant, OccurredAt: time.Now().UTC(),
		},
	}
}

func spineCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestServiceLauncher_GateDenialRefusesTheLaunch(t *testing.T) {
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1", maxConcurrentRuns: 1}, pub)
	s.cfg.Store = fakeActiveStore{RunStore: s.cfg.Store, active: 1}

	_, err := s.triggerLauncher().Launch(spineCtx(t), spinePlan("t1"))
	var deny *launchDeniedError
	if !errors.As(err, &deny) || deny.Reason != denyConcurrencyCap {
		t.Fatalf("Launch = %v, want the gate's typed %s denial", err, denyConcurrencyCap)
	}
	if got := pub.count(); got != 0 {
		t.Errorf("the run service was asked %d time(s) at the org's concurrency cap, want 0", got)
	}
}

func TestServiceLauncher_SuspendedTeamCannotLaunch(t *testing.T) {
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1", teamStatus: identity.TeamStatusSuspended}, pub)

	_, err := s.triggerLauncher().Launch(spineCtx(t), spinePlan("t1"))
	var deny *launchDeniedError
	if !errors.As(err, &deny) || deny.Reason != denyOrgSuspended {
		t.Fatalf("Launch = %v, want the gate's typed %s denial", err, denyOrgSuspended)
	}
	if got := pub.count(); got != 0 {
		t.Errorf("a suspended team launched %d run(s) through the spine, want 0", got)
	}
}

func TestServiceLauncher_MetersOnePerLaunchAndRollsBackARefusal(t *testing.T) {
	t.Run("admitted launch meters one slot", func(t *testing.T) {
		pub := &spinePublisher{}
		s := newGatedSpineServer(t, gateSpec{id: "t1", orgRunQuota: 3}, pub)
		counter := orgusage.NewMemoryCounter()
		s.orgUsage = counter

		if _, err := s.triggerLauncher().Launch(spineCtx(t), spinePlan("t1")); err != nil {
			t.Fatalf("Launch = %v, want nil", err)
		}
		u, _ := counter.Usage(context.Background(), "t1", time.Now().UTC())
		if u.Runs != 1 {
			t.Errorf("monthly runs = %d after one spine launch, want 1", u.Runs)
		}
	})
	t.Run("refused launch releases the slot", func(t *testing.T) {
		pub := &spinePublisher{err: errors.New("cloudpublisher: seal run bundle: kms unavailable")}
		s := newGatedSpineServer(t, gateSpec{id: "t1", orgRunQuota: 3}, pub)
		counter := orgusage.NewMemoryCounter()
		s.orgUsage = counter

		if _, err := s.triggerLauncher().Launch(spineCtx(t), spinePlan("t1")); err == nil {
			t.Fatal("Launch = nil, want the run service's refusal")
		}
		u, _ := counter.Usage(context.Background(), "t1", time.Now().UTC())
		if u.Runs != 0 {
			t.Errorf("monthly runs = %d after a refused launch, want 0 — a run that never started consumes no slot", u.Runs)
		}
	})
	t.Run("the quota is enforced, not just counted", func(t *testing.T) {
		pub := &spinePublisher{}
		s := newGatedSpineServer(t, gateSpec{id: "t1", orgRunQuota: 1}, pub)
		s.orgUsage = orgusage.NewMemoryCounter()

		if _, err := s.triggerLauncher().Launch(spineCtx(t), spinePlan("t1")); err != nil {
			t.Fatalf("first Launch = %v, want nil", err)
		}
		_, err := s.triggerLauncher().Launch(spineCtx(t), spinePlan("t1"))
		var deny *launchDeniedError
		if !errors.As(err, &deny) || deny.Reason != denyMonthlyRunQuota {
			t.Fatalf("second Launch = %v, want the gate's typed %s denial", err, denyMonthlyRunQuota)
		}
		if got := pub.count(); got != 1 {
			t.Errorf("the run service was asked %d time(s) for a quota of 1, want 1", got)
		}
	})
}

// The plan's tenant is the run's tenant: the spine launches on behalf of a
// subscription that names its team, and the publisher reads that team off the
// ctx to scope the run and seal its credentials. Launching under the empty
// tenant leaves a cloud run belonging to nobody.
func TestServiceLauncher_StampsThePlansTenantOnTheRun(t *testing.T) {
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1"}, pub)

	if _, err := s.triggerLauncher().Launch(spineCtx(t), spinePlan("t1")); err != nil {
		t.Fatalf("Launch = %v, want nil", err)
	}
	if pub.tenant != "t1" {
		t.Errorf("the run was published under tenant %q, want %q", pub.tenant, "t1")
	}
	if pub.owner != triggerSpineActor {
		t.Errorf("the run was published under owner %q, want %q", pub.owner, triggerSpineActor)
	}
}
