package runview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=iterion", "GIT_AUTHOR_EMAIL=iterion@example.test",
		"GIT_COMMITTER_NAME=iterion", "GIT_COMMITTER_EMAIL=iterion@example.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestRewind_RevertsWorkspaceToPreNodeState is the docs-bot case: a node
// whose real product is FILES, not its output map.
//
// `generate_docs` writes a doc set; the run then fails downstream. The
// rewind must put the workspace back to what the node started from —
// otherwise the replayed node builds on top of its own previous output
// and the operator is not testing the new configuration under the prior
// conditions.
func TestRewind_RevertsWorkspaceToPreNodeState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	wt := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	git(t, wt, "init", "-q", "-b", "main")
	git(t, wt, "config", "user.email", "iterion@example.test")
	git(t, wt, "config", "user.name", "iterion")
	// Gitignored build output must survive the revert untouched.
	writeFile(t, wt, ".gitignore", "site/\n")
	writeFile(t, wt, "docs/intro.md", "intro v1\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "base")

	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(linearBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "run-docs"
	if _, err := st.CreateRun(context.Background(), runID, "linear", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// The engine's pre-boundary marker for `implement`: the workspace as
	// it stood before that node ran.
	preRef := store.NodePreSnapshotRef(runID, "implement", 0)
	git(t, wt, "update-ref", preRef, "HEAD")

	// `implement` now produces the doc set — a rewrite, a new file, and a
	// deletion — plus some ignored build output.
	writeFile(t, wt, "docs/intro.md", "intro v2 REWRITTEN\n")
	writeFile(t, wt, "docs/api.md", "generated api page\n")
	writeFile(t, wt, "site/index.html", "<html>built</html>")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "docs: regenerate")
	// ...and leaves one file uncommitted, as an agent mid-run would.
	writeFile(t, wt, "docs/uncommitted.md", "work in progress\n")
	headBefore := git(t, wt, "rev-parse", "HEAD")

	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.FilePath = botPath
	run.Worktree = true
	run.WorkDir = wt
	run.Status = store.RunStatusFailedResumable
	run.Checkpoint = &store.Checkpoint{
		NodeID:  "verify",
		Outputs: outputsOf("survey", "plan", "implement", "verify"),
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}
	svc, err := NewService(storeDir)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "implement"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if result.Files == nil || !result.Files.Reverted {
		t.Fatalf("Files = %+v, want a completed revert", result.Files)
	}

	// The doc set is back to its pre-node state.
	if got := readWorkspaceFile(t, wt, "docs/intro.md"); got != "intro v1\n" {
		t.Errorf("docs/intro.md = %q, want the pre-node content", got)
	}
	if _, err := os.Stat(filepath.Join(wt, "docs/api.md")); !os.IsNotExist(err) {
		t.Error("docs/api.md survived; a file the node CREATED must be removed")
	}
	if _, err := os.Stat(filepath.Join(wt, "docs/uncommitted.md")); !os.IsNotExist(err) {
		t.Error("uncommitted work survived the revert")
	}
	// Gitignored build output is untouched: `git add -A` honours
	// .gitignore, so it was never in the snapshot to restore.
	if got := readWorkspaceFile(t, wt, "site/index.html"); got != "<html>built</html>" {
		t.Errorf("gitignored site/index.html = %q, want it left alone", got)
	}

	// Revert, not reset: history is preserved and extended.
	if result.Files.RevertCommit == "" {
		t.Fatal("expected a revert commit")
	}
	parent := git(t, wt, "rev-parse", "HEAD^")
	if parent != headBefore {
		t.Errorf("revert commit's parent = %s, want the pre-rewind HEAD %s", parent, headBefore)
	}
	if !strings.Contains(git(t, wt, "log", "--oneline"), "docs: regenerate") {
		t.Error("the node's own commit vanished from history; a rewind reverts, it does not rewrite")
	}
	// And nothing was lost: the banked ref still holds the state at the
	// instant of the rewind, uncommitted file included.
	if result.Files.BackupRef == "" {
		t.Fatal("expected a backup ref banking the pre-revert state")
	}
	banked := git(t, wt, "show", "--name-only", "--format=", result.Files.BackupRef)
	if !strings.Contains(banked, "docs/uncommitted.md") {
		t.Errorf("backup ref does not carry the uncommitted work: %q", banked)
	}
}

// TestRewind_RevertsWorkspaceWithoutWorktree is what iterion's own
// versioning buys: a run with NO isolated worktree — the default shape,
// and 17 of 30 catalog bots — can now have its files restored. git cannot
// serve here, because that workspace is the operator's live checkout and
// `git add -A` would stage their own uncommitted work.
func TestRewind_RevertsWorkspaceWithoutWorktree(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "workspace")
	writeFile(t, ws, "docs/intro.md", "intro v1\n")
	writeFile(t, ws, "docs/keep.md", "untouched\n")

	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(linearBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "run-inplace"
	if _, err := st.CreateRun(context.Background(), runID, "linear", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	svc, err := NewService(storeDir)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if svc.workspaceTracker == nil {
		t.Fatal("a filesystem-backed service must wire workspace versioning")
	}
	// The engine's pre-boundary capture for `implement`.
	if _, err := svc.workspaceTracker.Capture(runID, ws, "pre:implement:0"); err != nil {
		t.Fatalf("seed capture: %v", err)
	}

	// `implement` writes the doc set.
	writeFile(t, ws, "docs/intro.md", "intro v2 REWRITTEN\n")
	writeFile(t, ws, "docs/api.md", "generated\n")

	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.FilePath = botPath
	run.Worktree = false // in place — the case git cannot cover
	run.WorkDir = ws
	run.Status = store.RunStatusFailedResumable
	run.Checkpoint = &store.Checkpoint{
		NodeID:  "verify",
		Outputs: outputsOf("survey", "plan", "implement", "verify"),
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "implement"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if result.Files == nil || !result.Files.Reverted {
		t.Fatalf("Files = %+v, want a completed restore", result.Files)
	}
	if got := readWorkspaceFile(t, ws, "docs/intro.md"); got != "intro v1\n" {
		t.Errorf("docs/intro.md = %q, want the pre-node content", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "docs/api.md")); !os.IsNotExist(err) {
		t.Error("a file the node created survived the restore")
	}
	if got := readWorkspaceFile(t, ws, "docs/keep.md"); got != "untouched\n" {
		t.Errorf("docs/keep.md = %q, want it left alone", got)
	}
	// Nothing is destroyed: the pre-rewind state stays a resolvable
	// snapshot, so the discarded work is recoverable.
	if result.Files.BackupRef == "" {
		t.Fatal("expected the pre-rewind workspace to be banked")
	}
	banked, err := svc.workspaceTracker.Load(runID, result.Files.BackupRef)
	if err != nil {
		t.Fatalf("load banked snapshot: %v", err)
	}
	var hasAPI bool
	for _, e := range banked.Entries {
		if e.Path == "docs/api.md" {
			hasAPI = true
		}
	}
	if !hasAPI {
		t.Error("the banked snapshot does not carry the work the rewind discarded")
	}
}

// TestRewind_NoWorkspaceReportsSkip: a run with no recorded workspace has
// nothing to restore, and says so rather than staying silent.
func TestRewind_NoWorkspaceReportsSkip(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID:  "verify",
		Outputs: outputsOf("survey", "plan", "implement", "verify"),
	}
	svc, _, runID := seedRun(t, linearBot, cp, store.RunStatusFailedResumable)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "implement"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if result.Files == nil || result.Files.Reverted {
		t.Fatalf("Files = %+v, want a skip", result.Files)
	}
	if !strings.Contains(result.Files.SkipReason, "no recorded workspace") {
		t.Errorf("SkipReason = %q, want it to name the missing workspace", result.Files.SkipReason)
	}
}

// TestRewind_KeepFilesOptsOut leaves the workspace alone on request.
func TestRewind_KeepFilesOptsOut(t *testing.T) {
	cp := &store.Checkpoint{
		NodeID:  "verify",
		Outputs: outputsOf("survey", "plan", "implement", "verify"),
	}
	svc, _, runID := seedRun(t, linearBot, cp, store.RunStatusFailedResumable)

	result, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "implement", KeepFiles: true})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if result.Files.Reverted {
		t.Error("workspace was reverted despite KeepFiles")
	}
}

// TestSnapshotWorktree_ExportedRoundTrip guards the exported seam the
// rewind depends on for banking state.
func TestSnapshotWorktree_ExportedRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	wt := t.TempDir()
	git(t, wt, "init", "-q", "-b", "main")
	git(t, wt, "config", "user.email", "iterion@example.test")
	git(t, wt, "config", "user.name", "iterion")
	writeFile(t, wt, "a.txt", "one\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "base")

	writeFile(t, wt, "b.txt", "two\n")
	commit, err := runtime.SnapshotWorktree(wt, "refs/iterion/test/snap")
	if err != nil {
		t.Fatalf("SnapshotWorktree: %v", err)
	}
	if commit == "" {
		t.Fatal("expected a snapshot commit for a dirty worktree")
	}
	if !strings.Contains(git(t, wt, "show", "--name-only", "--format=", commit), "b.txt") {
		t.Error("snapshot does not carry the untracked file")
	}
}

func readWorkspaceFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestRewind_RefusesAStalePreSnapshot is Revi's R7c488c.
//
// The probe walked down to iteration 0 and took the first ref that
// resolved, with no bound and no signal. When the pivot had actually
// completed a LATER iteration whose pre-marker was missing, that restored
// the tree from before an earlier pass — while downstreamOf deliberately
// keeps the loop-mates' outputs, so the checkpoint claimed work whose
// files had just been reverted away. Reported as Reverted=true, silently.
//
// Two ordinary paths reach it: a workflow whose Loop.Body is empty, and a
// run resumed from a human pause (markPreNodeBoundary is gated on
// rs.isWorktree, which that path never sets).
func TestRewind_RefusesAStalePreSnapshot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	wt := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	git(t, wt, "init", "-q", "-b", "main")
	git(t, wt, "config", "user.email", "iterion@example.test")
	git(t, wt, "config", "user.name", "iterion")
	writeFile(t, wt, "docs/intro.md", "v1 — before the first pass\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "base")

	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(loopedBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	storeDir := filepath.Join(dir, "store")
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "run-stale"
	if _, err := st.CreateRun(context.Background(), runID, "looped", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Iteration 0 is fully bracketed.
	git(t, wt, "update-ref", store.NodePreSnapshotRef(runID, "implement", 0), "HEAD")
	writeFile(t, wt, "docs/intro.md", "v2 — produced by pass 0\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "pass 0")
	git(t, wt, "update-ref", store.NodeSnapshotRef(runID, "implement", 0), "HEAD")

	// Iteration 1 ran and COMPLETED, but its opening marker was never
	// written — the resumed-from-pause shape.
	writeFile(t, wt, "docs/intro.md", "v3 — produced by pass 1\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "pass 1")
	git(t, wt, "update-ref", store.NodeSnapshotRef(runID, "implement", 1), "HEAD")

	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run.FilePath = botPath
	run.Worktree = true
	run.WorkDir = wt
	run.Status = store.RunStatusFailedResumable
	run.Checkpoint = &store.Checkpoint{
		NodeID:       "verify",
		Outputs:      outputsOf("survey", "plan", "implement", "verify"),
		LoopCounters: map[string]int{"fix": 1},
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save: %v", err)
	}
	svc, err := NewService(storeDir)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Rewind(context.Background(), RewindSpec{RunID: runID, NodeID: "implement"})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if res.Files == nil {
		t.Fatal("expected a files report")
	}
	if res.Files.Reverted {
		t.Errorf("the workspace was reverted to iteration 0's snapshot even though "+
			"%q completed iteration 1 — that discards work the checkpoint still claims", "implement")
	}
	if res.Files.SkipReason == "" {
		t.Error("skipping the revert must say why; a silent no-op reads as success")
	}
	// The tree is untouched: pass 1's production is still there.
	if got := readWorkspaceFile(t, wt, "docs/intro.md"); got != "v3 — produced by pass 1\n" {
		t.Errorf("docs/intro.md = %q, want pass 1's content left in place", got)
	}
}
