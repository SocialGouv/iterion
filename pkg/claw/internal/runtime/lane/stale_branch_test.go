package lane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Mock GitRunner for unit tests ---

type mockGitRunner struct {
	revListCounts map[string]int
	logSubjects   map[string][]string
}

func newMockGitRunner() *mockGitRunner {
	return &mockGitRunner{
		revListCounts: make(map[string]int),
		logSubjects:   make(map[string][]string),
	}
}

func (m *mockGitRunner) setCount(a, b string, count int) {
	m.revListCounts[fmt.Sprintf("%s..%s", b, a)] = count
}

func (m *mockGitRunner) setSubjects(a, b string, subjects []string) {
	m.logSubjects[fmt.Sprintf("%s..%s", b, a)] = subjects
}

func (m *mockGitRunner) RevListCount(_ context.Context, _, a, b string) int {
	return m.revListCounts[fmt.Sprintf("%s..%s", b, a)]
}

func (m *mockGitRunner) LogSubjects(_ context.Context, _, a, b string) []string {
	return m.logSubjects[fmt.Sprintf("%s..%s", b, a)]
}

// --- Unit tests using mock ---

func TestFreshBranchPasses(t *testing.T) {
	runner := newMockGitRunner()
	runner.setCount("main", "topic", 0)
	runner.setCount("topic", "main", 0)

	freshness := CheckFreshness(context.Background(), runner, ".", "topic", "main")

	if freshness.Kind != FreshnessFresh {
		t.Errorf("expected Fresh, got %s", freshness.Kind)
	}
}

func TestFreshBranchAheadOfMainStillFresh(t *testing.T) {
	runner := newMockGitRunner()
	runner.setCount("main", "topic", 0)
	runner.setCount("topic", "main", 1)

	freshness := CheckFreshness(context.Background(), runner, ".", "topic", "main")

	if freshness.Kind != FreshnessFresh {
		t.Errorf("expected Fresh, got %s", freshness.Kind)
	}
}

func TestStaleBranchDetected(t *testing.T) {
	runner := newMockGitRunner()
	runner.setCount("main", "topic", 2)
	runner.setCount("topic", "main", 0)
	runner.setSubjects("main", "topic", []string{"fix: handle null pointer", "fix: resolve timeout"})

	freshness := CheckFreshness(context.Background(), runner, ".", "topic", "main")

	if freshness.Kind != FreshnessStale {
		t.Fatalf("expected Stale, got %s", freshness.Kind)
	}
	if freshness.CommitsBehind != 2 {
		t.Errorf("CommitsBehind = %d, want 2", freshness.CommitsBehind)
	}
	if len(freshness.MissingFixes) != 2 {
		t.Errorf("MissingFixes count = %d, want 2", len(freshness.MissingFixes))
	}
}

func TestDivergedBranchDetected(t *testing.T) {
	runner := newMockGitRunner()
	runner.setCount("main", "topic", 1)
	runner.setCount("topic", "main", 1)
	runner.setSubjects("main", "topic", []string{"main fix"})

	freshness := CheckFreshness(context.Background(), runner, ".", "topic", "main")

	if freshness.Kind != FreshnessDiverged {
		t.Fatalf("expected Diverged, got %s", freshness.Kind)
	}
	if freshness.CommitsAhead != 1 {
		t.Errorf("CommitsAhead = %d, want 1", freshness.CommitsAhead)
	}
	if freshness.CommitsBehind != 1 {
		t.Errorf("CommitsBehind = %d, want 1", freshness.CommitsBehind)
	}
	if len(freshness.MissingFixes) != 1 || freshness.MissingFixes[0] != "main fix" {
		t.Errorf("MissingFixes = %v", freshness.MissingFixes)
	}
}

func TestPolicyNoopForFresh(t *testing.T) {
	freshness := &BranchFreshness{Kind: FreshnessFresh}
	action := ApplyPolicy(freshness, PolicyWarnOnly)
	if action.Kind != ActionNoop {
		t.Errorf("expected Noop, got %s", action.Kind)
	}
}

func TestPolicyWarnForStale(t *testing.T) {
	freshness := &BranchFreshness{
		Kind:          FreshnessStale,
		CommitsBehind: 3,
		MissingFixes:  []string{"fix: timeout", "fix: null ptr"},
	}
	action := ApplyPolicy(freshness, PolicyWarnOnly)
	if action.Kind != ActionWarn {
		t.Fatalf("expected Warn, got %s", action.Kind)
	}
	if !strings.Contains(action.Message, "3 commit(s) behind") {
		t.Errorf("message missing behind count: %s", action.Message)
	}
	if !strings.Contains(action.Message, "fix: timeout") {
		t.Errorf("message missing fix: %s", action.Message)
	}
}

func TestPolicyBlockForStale(t *testing.T) {
	freshness := &BranchFreshness{
		Kind:          FreshnessStale,
		CommitsBehind: 1,
		MissingFixes:  []string{"hotfix"},
	}
	action := ApplyPolicy(freshness, PolicyBlock)
	if action.Kind != ActionBlockBranch {
		t.Fatalf("expected Block, got %s", action.Kind)
	}
	if !strings.Contains(action.Message, "1 commit(s) behind") {
		t.Errorf("message = %s", action.Message)
	}
}

func TestPolicyAutoRebaseForStale(t *testing.T) {
	freshness := &BranchFreshness{
		Kind:          FreshnessStale,
		CommitsBehind: 2,
	}
	action := ApplyPolicy(freshness, PolicyAutoRebase)
	if action.Kind != ActionRebase {
		t.Errorf("expected Rebase, got %s", action.Kind)
	}
}

func TestPolicyAutoMergeForwardForDiverged(t *testing.T) {
	freshness := &BranchFreshness{
		Kind:          FreshnessDiverged,
		CommitsAhead:  5,
		CommitsBehind: 2,
		MissingFixes:  []string{"fix: merge main"},
	}
	action := ApplyPolicy(freshness, PolicyAutoMergeForward)
	if action.Kind != ActionMFwd {
		t.Errorf("expected MergeForward, got %s", action.Kind)
	}
}

func TestPolicyWarnForDiverged(t *testing.T) {
	freshness := &BranchFreshness{
		Kind:          FreshnessDiverged,
		CommitsAhead:  3,
		CommitsBehind: 1,
		MissingFixes:  []string{"main hotfix"},
	}
	action := ApplyPolicy(freshness, PolicyWarnOnly)
	if action.Kind != ActionWarn {
		t.Fatalf("expected Warn, got %s", action.Kind)
	}
	if !strings.Contains(action.Message, "diverged") {
		t.Errorf("message missing 'diverged': %s", action.Message)
	}
	if !strings.Contains(action.Message, "3 commit(s) ahead") {
		t.Errorf("message missing ahead: %s", action.Message)
	}
	if !strings.Contains(action.Message, "1 commit(s) behind") {
		t.Errorf("message missing behind: %s", action.Message)
	}
	if !strings.Contains(action.Message, "main hotfix") {
		t.Errorf("message missing fix: %s", action.Message)
	}
}

// --- Integration tests using real git (skip if git unavailable) ---

func initTestRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(os.TempDir(), fmt.Sprintf("stale-branch-test-%d", time.Now().UnixNano()))
	os.MkdirAll(root, 0o755)
	t.Cleanup(func() { os.RemoveAll(root) })

	gitRun(t, root, "init", "--quiet", "-b", "main")
	gitRun(t, root, "config", "user.email", "tests@example.com")
	gitRun(t, root, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(root, "init.txt"), []byte("init\n"), 0o644)
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-m", "initial", "--quiet")
	return root
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func commitFile(t *testing.T, dir, name, msg string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, name), []byte(msg+"\n"), 0o644)
	gitRun(t, dir, "add", name)
	gitRun(t, dir, "commit", "-m", msg, "--quiet")
}

func TestIntegrationFreshBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := initTestRepo(t)
	gitRun(t, root, "checkout", "-b", "topic")

	runner := &ExecGitRunner{}
	freshness := CheckFreshness(context.Background(), runner, root, "topic", "main")

	if freshness.Kind != FreshnessFresh {
		t.Errorf("expected Fresh, got %s", freshness.Kind)
	}
}

func TestIntegrationStaleBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := initTestRepo(t)
	gitRun(t, root, "checkout", "-b", "topic")
	gitRun(t, root, "checkout", "main")
	commitFile(t, root, "fix1.txt", "fix: resolve timeout")
	commitFile(t, root, "fix2.txt", "fix: handle null pointer")

	runner := &ExecGitRunner{}
	freshness := CheckFreshness(context.Background(), runner, root, "topic", "main")

	if freshness.Kind != FreshnessStale {
		t.Fatalf("expected Stale, got %s", freshness.Kind)
	}
	if freshness.CommitsBehind != 2 {
		t.Errorf("CommitsBehind = %d, want 2", freshness.CommitsBehind)
	}
	if len(freshness.MissingFixes) != 2 {
		t.Errorf("MissingFixes = %d, want 2", len(freshness.MissingFixes))
	}
}

func TestIntegrationDivergedBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := initTestRepo(t)
	gitRun(t, root, "checkout", "-b", "topic")
	commitFile(t, root, "topic_work.txt", "topic work")
	gitRun(t, root, "checkout", "main")
	commitFile(t, root, "main_fix.txt", "main fix")

	runner := &ExecGitRunner{}
	freshness := CheckFreshness(context.Background(), runner, root, "topic", "main")

	if freshness.Kind != FreshnessDiverged {
		t.Fatalf("expected Diverged, got %s", freshness.Kind)
	}
	if freshness.CommitsAhead != 1 || freshness.CommitsBehind != 1 {
		t.Errorf("ahead=%d behind=%d", freshness.CommitsAhead, freshness.CommitsBehind)
	}
}
