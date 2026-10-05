package runview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sessionboard"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// The D12 guard: a server-side auxiliary LLM surface refuses content from a
// run stamped to a sovereign runner pool, unless the surface's model is
// itself gateway-routed (the operator provided the pool's
// openai_compatible credential to this process). The stamp consulted is
// the run document's frozen stamp — unmapping a team never declassifies
// its past runs' content.
func TestPoolContentRefusalTable(t *testing.T) {
	const pool = "honorabilite"
	cases := []struct {
		name   string
		pool   string
		spec   string
		refuse bool
	}{
		{"no stamp, any spec passes", "", "anthropic/claude-opus-4-8", false},
		{"gateway pin escapes", pool, "openai_compatible/glm-5.2", false},
		{"vendor pin refused", pool, "anthropic/claude-opus-4-8", true},
		{"auto-detect (empty spec) refused", pool, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := poolContentRefusal(tc.pool, tc.spec)
			if tc.refuse && !errors.Is(err, ErrPoolContentRefused) {
				t.Fatalf("poolContentRefusal(%q, %q) = %v, want ErrPoolContentRefused", tc.pool, tc.spec, err)
			}
			if !tc.refuse && err != nil {
				t.Fatalf("poolContentRefusal(%q, %q) = %v, want nil", tc.pool, tc.spec, err)
			}
			if tc.refuse && !strings.Contains(err.Error(), pool) {
				t.Fatalf("refusal %q does not name the pool %q", err, pool)
			}
		})
	}
}

// poolSurvivingSpecs: on a pool-stamped run, vendor-pinned and auto
// (unpinned) supervisors are refused; a gateway-pinned supervisor keeps
// running; with no stamp every spec survives.
func TestPoolSurvivingSpecs(t *testing.T) {
	// The filter consults host env exactly like eval time; a host that
	// exports either var must not flip these verdicts.
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_GLM", "")
	logger := iterlog.Nop()
	all := []supervise.Spec{
		{Name: "vendor-pinned", Model: "anthropic/claude-opus-4-8"},
		{Name: "gateway-pinned", Model: "openai_compatible/glm-5.2"},
		{Name: "env-form gateway", Model: "${ITERION_GLM:-openai_compatible/glm-5.2}"},
		{Name: "auto", Model: ""},
	}
	kept := PoolSurvivingSpecs(all, "", logger)
	if len(kept) != 4 {
		t.Fatalf("no stamp: %d specs kept, want 4", len(kept))
	}
	kept = PoolSurvivingSpecs(all, "honorabilite", logger)
	if len(kept) != 2 || kept[0].Name != "gateway-pinned" || kept[1].Name != "env-form gateway" {
		t.Fatalf("pool run: %v kept, want both gateway forms", kept)
	}
}

// The conflict resolver, at its real site, on a pool-stamped run: a vendor
// pin is refused BEFORE any HTTP request leaves the process, and the
// gateway pin is not the pool refusal (it proceeds to model resolution).
func TestConflictAgentRefusesPoolRunContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "test-key")
	// A host that exports the gateway env would let the escape-hatch leg
	// resolve a real client and send the fixture's conflict content to a
	// live gateway — scrub it for determinism.
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "")
	t.Setenv("ITERION_CONFLICT_RESOLVER_MODEL", "")

	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	repoDir := filepath.Join(dir, "repo")
	logger := iterlog.Nop()
	st, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runID := "run-pool-guard"
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.RunnerPool = "honorabilite"
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	svc, err := NewService(storeDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := seedConflict(t, svc, st, ctx, runID, repoDir); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}

	// Vendor pin: the refusal, and the fake vendor never sees a request.
	_, err = svc.resolveAllConflictsWithAgent(ctx, runID, "openai/gpt-5.5")
	if !errors.Is(err, ErrPoolContentRefused) {
		t.Fatalf("vendor pin on a pool run: err = %v, want ErrPoolContentRefused", err)
	}
	if requests != 0 {
		t.Fatalf("the resolver sent %d request(s) despite the refusal", requests)
	}

	// Gateway pin: NOT the pool refusal — the guard lets it through and
	// model resolution fails on the missing gateway env.
	_, err = svc.resolveAllConflictsWithAgent(ctx, runID, "openai_compatible/glm-5.2")
	if errors.Is(err, ErrPoolContentRefused) {
		t.Fatal("gateway pin was refused by the pool guard; the escape hatch is dead")
	}
	if err == nil {
		t.Fatal("gateway pin unexpectedly resolved with no gateway env; fixture drifted")
	}
}

// seedConflict mirrors the conflict_agent_wire_test fixture: a repo whose
// squash merge left conflicts, bound to the run document, then a merge
// attempt that lands the document in MergeStatusConflicted.
func seedConflict(t *testing.T, svc *Service, st *store.FilesystemRunStore, ctx context.Context, runID, repoDir string) error {
	t.Helper()
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return err
	}
	runGit := func(args ...string) {
		t.Helper()
		gittest.Run(t, repoDir, args...)
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("alpha\nbravo\ncharlie\n"), 0o644); err != nil {
		return err
	}
	runGit("add", "file.txt")
	runGit("commit", "-qm", "base")
	runGit("checkout", "-qb", "iterion/run/pool-guard")
	if err := os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("alpha\nBRAVO-INCOMING\ncharlie\n"), 0o644); err != nil {
		return err
	}
	runGit("commit", "-qam", "feat")
	storageSHA := strings.TrimSpace(captureGitOutput(t, repoDir, "rev-parse", "HEAD"))
	runGit("checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("alpha\nbravo-main\ncharlie\n"), 0o644); err != nil {
		return err
	}
	runGit("commit", "-qam", "main-change")
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		return err
	}
	r.Worktree = true
	r.RepoRoot = repoDir
	r.WorkDir = repoDir
	r.FinalCommit = storageSHA
	r.FinalBranch = "iterion/run/pool-guard"
	r.Status = store.RunStatusFinished
	r.MergeStrategy = store.MergeStrategySquash
	if err := st.SaveRun(ctx, r); err != nil {
		return err
	}
	if _, err := svc.PerformMergeCtx(ctx, runID, MergeRequest{}); err == nil || !strings.Contains(err.Error(), "conflict") {
		return fmt.Errorf("fixture: the merge did not conflict (err=%v)", err)
	}
	return nil
}

// Source witness (the P1b-ii pattern): the three auxiliary surfaces keep
// their D12 guard calls. A site that drops its guard — or a refactor that
// renames the guard away — reddens here, not in production.
func TestPoolGuardCallSitesArePinned(t *testing.T) {
	agent, err := os.ReadFile("conflict_agent.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agent), "poolContentRefusal(r.RunnerPool, spec)") {
		t.Fatal("the conflict resolver no longer consults the run's frozen pool stamp")
	}
	observe, err := os.ReadFile("service_observe.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{
		"specs = PoolSurvivingSpecs(specs, pool, logger)",
		"poolContentRefusal(pool, sessionboard.ModelFromEnv())",
	} {
		if !strings.Contains(string(observe), pin) {
			t.Fatalf("service_observe.go lost its D12 guard pin: %q", pin)
		}
	}
	loop, err := os.ReadFile("../../pkg/runner/loop.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loop), "runview.PoolSurvivingSpecs(supervise.SpecsFromWorkflow(wf, runLogger), msg.RunnerPool, runLogger)") {
		t.Fatal("the runner pod no longer filters declared supervisors by the frozen pool stamp (D12)")
	}
}

// The two coordinator sites, at their real dispositions (the round-1 F3
// finding: the refusal branches had no witness that bit). Each test swaps
// the dispatchable seam, drives the site against a store holding a stamped
// run, and asserts the surface never starts; the unstamped control proves
// the seam would have started it.
func TestStartDeclaredSupervisorsRefusePoolRunContent(t *testing.T) {
	// The kill switch short-circuits BEFORE the guard, and the env pins
	// feed the filter: a host exporting either must not flip the verdicts.
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	logger := iterlog.Nop()
	st, svc, ctx := newGuardTestService(t)
	runID := "run-sup-site"
	seedGuardRun(t, st, ctx, runID, "honorabilite")

	var captured []supervise.Spec
	restore := swapStartDeclared(func(ctx context.Context, obs supervise.Observer, inj supervise.Injector, id string, specs []supervise.Spec, l *iterlog.Logger) (stop func()) {
		captured = specs
		return func() {}
	})
	defer restore()

	wf := &ir.Workflow{Supervisors: []*ir.Supervisor{
		{Name: "vendor", Model: "anthropic/claude-opus-4-8"},
		{Name: "gateway", Model: "openai_compatible/glm-5.2"},
		{Name: "auto"},
	}}
	svc.startDeclaredSupervisors(ctx, runID, wf, logger, "")
	if len(captured) != 1 || captured[0].Name != "gateway" {
		t.Fatalf("pool run: %d specs reached StartDeclared (%v), want only gateway", len(captured), captured)
	}

	// Control: the same workflow on an unstamped run starts all three.
	seedGuardRun(t, st, ctx, "run-sup-site-unstamped", "")
	svc.startDeclaredSupervisors(ctx, "run-sup-site-unstamped", wf, logger, "")
	if len(captured) != 3 {
		t.Fatalf("unstamped control: %d specs reached StartDeclared, want 3", len(captured))
	}
}

func TestStartSessionBoardRefusesPoolRunContent(t *testing.T) {
	logger := iterlog.Nop()
	st, svc, ctx := newGuardTestService(t)
	seedGuardRun(t, st, ctx, "run-sb-site", "honorabilite")

	started := 0
	restore := swapSessionBoardCoordinator(func(obs sessionboard.Observer, emit sessionboard.Emitter, id string, cfg sessionboard.Config, eval sessionboard.Evaluator, l *iterlog.Logger) *sessionboard.Coordinator {
		started++
		return nil
	})
	defer restore()

	t.Setenv("ITERION_SESSION_BOARD", "on")
	t.Setenv("ITERION_DEFAULT_SESSIONBOARD_MODEL", "")
	svc.sbStore = guardStubSBStore{}
	svc.startSessionBoard(ctx, "run-sb-site", "bot", logger)
	if started != 0 {
		t.Fatal("session board started on a pool-stamped run")
	}

	seedGuardRun(t, st, ctx, "run-sb-site-unstamped", "")
	svc.startSessionBoard(ctx, "run-sb-site-unstamped", "bot", logger)
	if started != 1 {
		t.Fatal("unstamped control: the seam never constructed the coordinator")
	}
}

// The D12 pre-stamp cohort: an unstamped run whose tenant is NOW mapped
// refuses too — the current mapping is the only signal left for content
// that predates the stamp.
func TestConflictAgentRefusesPreStampRunOfMappedTenant(t *testing.T) {
	st, svc, ctx := newGuardTestService(t)
	runID := "run-prestamp"
	seedGuardRun(t, st, ctx, runID, "")
	seedTenantMapping(t, svc, "tenant-a", "honorabilite")

	_, err := svc.resolveAllConflictsWithAgent(ctx, runID, "openai/gpt-5.5")
	if !errors.Is(err, ErrPoolContentRefused) {
		t.Fatalf("pre-stamp run of a mapped tenant: err = %v, want ErrPoolContentRefused", err)
	}

	// Unmapped tenant: the refusal does not apply.
	seedGuardRun(t, st, ctx, "run-prestamp-unmapped", "")
	seedTenantMapping(t, svc, "tenant-b", "")
	svc.currentPoolForTenant = func(ctx context.Context, tenantID string) (string, error) {
		if tenantID == "tenant-b" {
			return "", nil
		}
		return "hooland", nil
	}
	if _, err := svc.resolveAllConflictsWithAgent(ctx, "run-prestamp-unmapped", "openai/gpt-5.5"); errors.Is(err, ErrPoolContentRefused) {
		t.Fatal("unmapped tenant was refused; the pre-stamp check over-fires")
	}
}

// --- guard-test helpers ---

// newGuardTestService builds a file-backed store + its Service over the
// same dir, the shape every site test needs.
func newGuardTestService(t *testing.T) (*store.FilesystemRunStore, *Service, context.Context) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "store"), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc, err := NewService(filepath.Join(dir, "store"), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return st, svc, context.Background()
}

// seedRun creates a run and stamps (or leaves empty) its pool stamp and
// tenant. Conflicted-state fields are not needed by the site tests.
func seedGuardRun(t *testing.T, st *store.FilesystemRunStore, ctx context.Context, runID, pool string) {
	t.Helper()
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	if _, err := st.CreateRun(ctx2, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := st.LoadRun(ctx2, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.RunnerPool = pool
	r.TenantID = mapTenant(runID)
	if err := st.SaveRun(ctx2, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

// mapTenant names the tenant for a seeded run so the pre-stamp test can
// bind a mapping to it.
func mapTenant(runID string) string {
	switch runID {
	case "run-prestamp":
		return "tenant-a"
	case "run-prestamp-unmapped":
		return "tenant-b"
	default:
		return ""
	}
}

// seedTenantMapping installs the fresh lookup the pre-stamp check reads.
func seedTenantMapping(t *testing.T, svc *Service, tenant, pool string) {
	t.Helper()
	svc.currentPoolForTenant = func(ctx context.Context, tenantID string) (string, error) {
		if tenantID == tenant {
			return pool, nil
		}
		return "", nil
	}
}

// swapStartDeclared swaps the StartDeclared seam and returns its restore.
func swapStartDeclared(f func(ctx context.Context, obs supervise.Observer, inj supervise.Injector, id string, specs []supervise.Spec, l *iterlog.Logger) (stop func())) (restore func()) {
	prev := startDeclaredImpl
	startDeclaredImpl = f
	return func() { startDeclaredImpl = prev }
}

// swapSessionBoardCoordinator swaps the coordinator-construction seam and
// returns its restore.
func swapSessionBoardCoordinator(f func(sessionboard.Observer, sessionboard.Emitter, string, sessionboard.Config, sessionboard.Evaluator, *iterlog.Logger) *sessionboard.Coordinator) (restore func()) {
	prev := newSessionBoardCoordinator
	newSessionBoardCoordinator = f
	return func() { newSessionBoardCoordinator = prev }
}

// guardStubSBStore satisfies sessionboard.Store for the site test.
type guardStubSBStore struct{}

func (guardStubSBStore) Load(string) (sessionboard.Spec, error) { return sessionboard.Spec{}, nil }
func (guardStubSBStore) Save(string, sessionboard.Spec) error   { return nil }
