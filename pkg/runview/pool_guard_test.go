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
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
	logger := iterlog.Nop()
	all := []supervise.Spec{
		{Name: "vendor-pinned", Model: "anthropic/claude-opus-4-8"},
		{Name: "gateway-pinned", Model: "openai_compatible/glm-5.2"},
		{Name: "auto", Model: ""},
	}
	kept := poolSurvivingSpecs(all, "", logger)
	if len(kept) != 3 {
		t.Fatalf("no stamp: %d specs kept, want 3", len(kept))
	}
	kept = poolSurvivingSpecs(all, "honorabilite", logger)
	if len(kept) != 1 || kept[0].Name != "gateway-pinned" {
		t.Fatalf("pool run: %v kept, want only gateway-pinned", kept)
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
		"specs = poolSurvivingSpecs(specs, pool, logger)",
		"poolContentRefusal(pool, sessionboard.ModelFromEnv())",
	} {
		if !strings.Contains(string(observe), pin) {
			t.Fatalf("service_observe.go lost its D12 guard pin: %q", pin)
		}
	}
}
