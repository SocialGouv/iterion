package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

// trustCapturingPublisher records the trust a launch or a resume was published
// with, and the trust the workflow's MCP catalog was resolved for.
type trustCapturingPublisher struct {
	mu           sync.Mutex
	launched     bool
	launchTrust  store.RunTrust
	launchWF     string
	resumed      bool
	resumeWF     string
	resumeWFNone bool
}

func preparedTrust(wf *ir.Workflow) (string, bool) {
	if wf == nil || wf.MCPPrepared == nil {
		return "", false
	}
	return wf.MCPPrepared.Trust, true
}

func (p *trustCapturingPublisher) SubmitLaunch(_ context.Context, _ string, spec LaunchSpec, wf *ir.Workflow, _ *CompiledSource) (LaunchSubmission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.launched, p.launchTrust = true, spec.Trust
	p.launchWF, _ = preparedTrust(wf)
	return LaunchSubmission{QueuePosition: 1}, nil
}
func (p *trustCapturingPublisher) CancelRun(context.Context, string) error { return nil }
func (p *trustCapturingPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (p *trustCapturingPublisher) SubmitResume(_ context.Context, _ ResumeSpec, wf *ir.Workflow, _ *CompiledSource) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ok bool
	p.resumed = true
	p.resumeWF, ok = preparedTrust(wf)
	p.resumeWFNone = !ok
	return nil
}

const sourceTrustBot = "\nworkflow source_trust:\n  entry: done\n"

func seedSourceRun(t *testing.T, st store.RunStore, id string, trust store.RunTrust) {
	t.Helper()
	r := storetest.SeedParentRun(t, st, id)
	r.Trust = trust
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// TestLaunch_ARunLaunchedFromAnotherIsNeverMoreTrustedThanIt: every surface
// that launches a run FROM another — a REST launch's parent_run_id, a cloud
// shard of `__scan-shards`, a delegation's source run — crosses
// Service.Launch, and the new run takes the lower of its own trust and its
// source's, read off the source's document. The compile is the one the
// launch publishes: its MCP catalog is resolved for that same trust. A source
// that cannot be read is refused, typed, and nothing is published — the one
// change a trusted launch sees.
func TestLaunch_ARunLaunchedFromAnotherIsNeverMoreTrustedThanIt(t *testing.T) {
	for _, row := range []struct {
		name    string
		seed    store.RunTrust
		seeded  bool
		via     func(spec *LaunchSpec)
		want    store.RunTrust
		refused bool
	}{
		{name: "a trusted parent (the control)", seeded: true, seed: store.RunTrustDefault, via: func(s *LaunchSpec) { s.ParentRunID = "source" }, want: store.RunTrustDefault},
		{name: "a fork parent", seeded: true, seed: store.RunTrustFork, via: func(s *LaunchSpec) { s.ParentRunID = "source" }, want: store.RunTrustFork},
		{name: "a fork delegation source", seeded: true, seed: store.RunTrustFork, via: func(s *LaunchSpec) {
			s.Delegation = &store.RunDelegation{SourceRunID: "source", Kind: "run_failure"}
		}, want: store.RunTrustFork},
		{name: "a parent that cannot be read", via: func(s *LaunchSpec) { s.ParentRunID = "source" }, refused: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			botPath := filepath.Join(dir, "source_trust.bot")
			if err := os.WriteFile(botPath, []byte(sourceTrustBot), 0o644); err != nil {
				t.Fatal(err)
			}
			pub := &trustCapturingPublisher{}
			svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
			if err != nil {
				t.Fatal(err)
			}
			if row.seeded {
				seedSourceRun(t, svc.store, "source", row.seed)
			}
			spec := LaunchSpec{FilePath: botPath}
			row.via(&spec)
			_, err = svc.Launch(context.Background(), spec)
			if row.refused {
				if !errors.Is(err, store.ErrChildParentUnreadable) || !errors.Is(err, store.ErrRunNotFound) {
					t.Fatalf("Launch = %v, want %v wrapping the read's %v", err, store.ErrChildParentUnreadable, store.ErrRunNotFound)
				}
				if pub.launched {
					t.Fatal("a launch naming an unreadable parent was published")
				}
				return
			}
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if !pub.launched {
				t.Fatal("nothing was published — this row proves nothing")
			}
			if pub.launchTrust != row.want {
				t.Errorf("published with trust %q, want %q", pub.launchTrust, row.want)
			}
			if pub.launchWF != string(row.want) {
				t.Errorf("the published workflow's catalog was resolved for %q, the launch is %q", pub.launchWF, row.want)
			}
		})
	}

	// Decision 10: the in-process path persists no marker, so it keeps
	// refusing an untrusted launch — a fork parent's child included.
	t.Run("in process, a fork parent: refused", func(t *testing.T) {
		dir := t.TempDir()
		botPath := filepath.Join(dir, "source_trust.bot")
		if err := os.WriteFile(botPath, []byte(sourceTrustBot), 0o644); err != nil {
			t.Fatal(err)
		}
		svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
		if err != nil {
			t.Fatal(err)
		}
		seedSourceRun(t, svc.store, "source", store.RunTrustFork)
		_, err = svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, ParentRunID: "source", RunID: "in-process-child"})
		if err == nil || !strings.Contains(err.Error(), `trust="fork"`) {
			t.Fatalf("Launch = %v, want the in-process refusal naming the fork trust", err)
		}
		if _, lerr := svc.store.LoadRun(context.Background(), "in-process-child"); lerr == nil {
			t.Fatal("the refused in-process launch created its run")
		}
	})
}

// TestResume_IsCompiledForTheRunsRecordedTrust: a resume compiles the
// workflow again, and the catalog it publishes is resolved for the trust the
// run DOCUMENT records — a resume arrives through whatever surface, long
// after the lane that admitted the run, and must not re-earn trust by it.
func TestResume_IsCompiledForTheRunsRecordedTrust(t *testing.T) {
	for _, row := range []struct {
		name  string
		trust store.RunTrust
	}{{"a trusted run", store.RunTrustDefault}, {"a fork run", store.RunTrustFork}} {
		trust := row.trust
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			botPath := filepath.Join(dir, "operator_resume.bot")
			if err := os.WriteFile(botPath, []byte("\nworkflow operator_resume:\n  entry: done\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			pub := &trustCapturingPublisher{}
			svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
			if err != nil {
				t.Fatal(err)
			}
			const runID = "run-resume-trust"
			_, hash, err := CompileWorkflowWithHash(botPath, "", AnalysisTrust)
			if err != nil {
				t.Fatal(err)
			}
			seedPausedOperatorRun(t, svc, runID, hash)
			r, err := svc.store.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			r.Trust = trust
			if err := svc.store.SaveRun(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Resume(context.Background(), ResumeSpec{RunID: runID, FilePath: botPath}); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if !pub.resumed || pub.resumeWFNone {
				t.Fatalf("resumed=%v, catalog recorded=%v — this row proves nothing", pub.resumed, !pub.resumeWFNone)
			}
			if pub.resumeWF != string(trust) {
				t.Errorf("the resumed workflow's catalog was resolved for %q, the run records %q", pub.resumeWF, trust)
			}
		})
	}
}

// tenantProbeStore records, per store method, whether the context the service
// handed it carried a tenant. On the Mongo store a tenant-scoped method given
// a context without one does not fail soft — withTenantFilter PANICS — so
// "did the identity survive" is the question, not "did the call succeed". The
// Mongo twin of this row runs in pkg/store/mongo (TestServiceLaunch_…).
type tenantProbeStore struct {
	store.RunStore
	mu   sync.Mutex
	seen map[string][]bool // method → ctx carried a tenant, per call
}

func (p *tenantProbeStore) note(method string, ctx context.Context) {
	_, ok := store.TenantFromContext(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = map[string][]bool{}
	}
	p.seen[method] = append(p.seen[method], ok)
}

func (p *tenantProbeStore) LockRun(ctx context.Context, id string) (store.RunLock, error) {
	p.note("LockRun", ctx)
	return p.RunStore.LockRun(ctx, id)
}

func (p *tenantProbeStore) CreateChildRun(ctx context.Context, id, wf, parentRunID string, inputs map[string]any) (*store.Run, error) {
	p.note("CreateChildRun", ctx)
	return store.AsParentedRunCreator(p.RunStore).CreateChildRun(ctx, id, wf, parentRunID, inputs)
}

func (p *tenantProbeStore) PatchRunBudgetOverrides(ctx context.Context, id string, o *store.RunBudgetOverrides) error {
	p.note("PatchRunBudgetOverrides", ctx)
	return store.AsRunBudgetOverridesPatcher(p.RunStore).PatchRunBudgetOverrides(ctx, id, o)
}

func (p *tenantProbeStore) calls(method string) []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.seen[method]...)
}

// TestSpawnRun_StoreCallsCarryTheLaunchIdentity: spawnRun detaches its store
// calls from the request so a finished request cannot cancel them — and keeps
// the request's identity, which context.Background() threw away with the
// cancellation. The parented create reads the parent to inherit its workspace
// provenance, a tenant-scoped read.
func TestSpawnRun_StoreCallsCarryTheLaunchIdentity(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "pause_demo.bot")
	if err := os.WriteFile(botPath, []byte(pausingBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if store.AsRunBudgetOverridesPatcher(svc.store) == nil {
		t.Fatal("the filesystem store no longer patches budget overrides — the probe below would not see that call")
	}
	probe := &tenantProbeStore{RunStore: svc.store}
	svc.store = probe
	storetest.SeedParentRun(t, probe, "run-tenant-parent")

	ctx := store.WithIdentity(context.Background(), "tenant-42", "owner-7")
	res, err := svc.Launch(ctx, LaunchSpec{
		RunID:       "run-tenant-child",
		FilePath:    botPath,
		ParentRunID: "run-tenant-parent",
		Budget:      &ir.BudgetOverrides{MaxCostUSD: 7},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case <-res.Done:
	case <-runWaitContext(t).Done():
		t.Fatal("run goroutine did not exit (expected an immediate human pause)")
	}
	for _, method := range []string{"LockRun", "CreateChildRun", "PatchRunBudgetOverrides"} {
		got := probe.calls(method)
		if len(got) == 0 {
			t.Fatalf("%s was never called — the probe proves nothing", method)
		}
		for i, carried := range got {
			if !carried {
				t.Errorf("%s call %d received a context with NO tenant: on the Mongo store a tenant-scoped call there panics", method, i)
			}
		}
	}
}
