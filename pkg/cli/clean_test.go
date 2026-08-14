package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// cleanFixture builds a real git repository plus an iterion store whose
// worktrees are genuine linked worktrees of it. The classification this
// command makes is entirely a set of claims about git state, so the tests
// exercise git rather than a model of it.
type cleanFixture struct {
	t     *testing.T
	repo  string
	store string
}

func newCleanFixture(t *testing.T) *cleanFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	f := &cleanFixture{
		t:     t,
		repo:  filepath.Join(root, "repo"),
		store: filepath.Join(root, "store"),
	}
	mustMkdir(t, f.repo)
	mustMkdir(t, filepath.Join(f.store, "worktrees"))

	f.git(f.repo, "init", "--initial-branch=main")
	f.git(f.repo, "config", "user.email", "test@example.com")
	f.git(f.repo, "config", "user.name", "Test")
	mustWrite(t, filepath.Join(f.repo, "README.md"), "base\n")
	f.git(f.repo, "add", "README.md")
	f.git(f.repo, "commit", "-m", "base")
	return f
}

func (f *cleanFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"LC_ALL=C", "LANG=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// addWorktree creates <store>/worktrees/<runID> as a linked worktree on
// its own branch, with one commit on top of main.
func (f *cleanFixture) addWorktree(runID string) string {
	f.t.Helper()
	path := filepath.Join(f.store, "worktrees", runID)
	f.git(f.repo, "worktree", "add", "-b", "iterion/run/"+runID, path, "main")
	mustWrite(f.t, filepath.Join(path, runID+".txt"), "work by "+runID+"\n")
	f.git(path, "add", ".")
	f.git(path, "commit", "-m", "work by "+runID)
	return path
}

// mergeIntoMain merges a worktree's branch into main, which is what makes
// its HEAD reachable from a ref other than its own.
func (f *cleanFixture) mergeIntoMain(runID string) {
	f.t.Helper()
	f.git(f.repo, "merge", "--no-ff", "-m", "merge "+runID, "iterion/run/"+runID)
}

func (f *cleanFixture) seedRun(runID string, status store.RunStatus) {
	f.t.Helper()
	s, err := store.New(f.store)
	if err != nil {
		f.t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		f.t.Fatalf("CreateRun(%s): %v", runID, err)
	}
	if status != store.RunStatusRunning {
		if err := s.UpdateRunStatus(ctx, runID, status, ""); err != nil {
			f.t.Fatalf("UpdateRunStatus(%s): %v", runID, err)
		}
	}
}

func (f *cleanFixture) run(opts CleanOptions) CleanResult {
	f.t.Helper()
	opts.StoreDir = f.store
	if opts.Level == "" {
		opts.Level = CleanConservative
	}
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputJSON}
	if err := RunClean(opts, p); err != nil {
		f.t.Fatalf("RunClean: %v", err)
	}
	return decodeCleanResult(f.t, buf.Bytes())
}

func decodeCleanResult(t *testing.T, raw []byte) CleanResult {
	t.Helper()
	var r CleanResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode result: %v\nraw: %s", err, raw)
	}
	return r
}

func deletedPaths(r CleanResult) map[string]bool {
	out := map[string]bool{}
	for _, wt := range r.Deleted {
		out[filepath.Base(wt.Path)] = true
	}
	return out
}

func sparedReason(r CleanResult, runID string) string {
	for _, wt := range r.Spared {
		if filepath.Base(wt.Path) == runID {
			return wt.SkipReason
		}
	}
	return ""
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --- the guards -----------------------------------------------------------

// A run that has not reached a terminal status owns its worktree, and no
// level may take it. This is the guard that protects a campaign in
// flight, and it must not depend on the worktree's mtime: a run can sit
// in a single agent turn for hours without writing to its checkout.
func TestClean_NeverDeletesWorktreeOfNonTerminalRun(t *testing.T) {
	for _, status := range []store.RunStatus{
		store.RunStatusRunning,
		store.RunStatusQueued,
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
	} {
		t.Run(string(status), func(t *testing.T) {
			f := newCleanFixture(t)
			f.addWorktree("live")
			f.mergeIntoMain("live") // landed: eligible on every other count
			f.seedRun("live", status)

			// Back-date the directory so age cannot be what spares it.
			old := time.Now().Add(-90 * 24 * time.Hour)
			if err := os.Chtimes(filepath.Join(f.store, "worktrees", "live"), old, old); err != nil {
				t.Fatalf("chtimes: %v", err)
			}

			r := f.run(CleanOptions{Level: CleanAggressive, Apply: true})
			if deletedPaths(r)["live"] {
				t.Fatalf("deleted the worktree of a %s run", status)
			}
			if got := sparedReason(r, "live"); got != skipRunActive {
				t.Fatalf("spared for %q, want %q", got, skipRunActive)
			}
			if _, err := os.Stat(filepath.Join(f.store, "worktrees", "live")); err != nil {
				t.Fatalf("worktree of a %s run was removed from disk: %v", status, err)
			}
		})
	}
}

// Commits no ref can reach would survive only in the reflog. No level
// deletes them — that is the promise that makes the command usable
// without reading its source first.
func TestClean_NeverDeletesUnlandedWorktree(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("orphaned")
	// Detach HEAD and drop the branch: the commit is now reachable from
	// nothing but this worktree's HEAD.
	f.git(path, "checkout", "--detach")
	f.git(f.repo, "branch", "-D", "iterion/run/orphaned")
	f.seedRun("orphaned", store.RunStatusFailed)

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["orphaned"] {
		t.Fatal("deleted a worktree whose commits no ref contains")
	}
	if got := sparedReason(r, "orphaned"); got != skipUnlanded {
		t.Fatalf("spared for %q, want %q", got, skipUnlanded)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unlanded worktree was removed from disk: %v", err)
	}
}

// The gate state under worktrees/.state is shared across runs and is not
// any run's checkout; reclaiming it is a different decision with a
// different cost.
func TestClean_SkipsDotPrefixedEntries(t *testing.T) {
	f := newCleanFixture(t)
	stateDir := filepath.Join(f.store, "worktrees", ".state")
	mustMkdir(t, stateDir)
	mustWrite(t, filepath.Join(stateDir, "app.jar"), "binary")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if r.Scanned != 0 {
		t.Fatalf("scanned %d entries, want 0 — .state must not be a candidate", r.Scanned)
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf(".state was removed: %v", err)
	}
}

// --- the levels -----------------------------------------------------------

func TestClean_ConservativeTakesLandedAndOrphanOnly(t *testing.T) {
	f := newCleanFixture(t)

	// landed-elsewhere, clean tree -> conservative takes it
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	// landed-elsewhere but dirty -> needs moderate
	dirty := f.addWorktree("merged-dirty")
	f.mergeIntoMain("merged-dirty")
	mustWrite(t, filepath.Join(dirty, "scratch.txt"), "uncommitted\n")
	f.seedRun("merged-dirty", store.RunStatusFinished)

	// own branch only -> needs aggressive
	f.addWorktree("solo")
	f.seedRun("solo", store.RunStatusFinished)

	// orphan: a plain directory that is not a worktree at all
	orphan := filepath.Join(f.store, "worktrees", "stale")
	mustMkdir(t, orphan)
	mustWrite(t, filepath.Join(orphan, "leftover.txt"), "junk\n")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	got := deletedPaths(r)
	if !got["merged"] {
		t.Error("conservative did not take a landed, clean worktree")
	}
	if !got["stale"] {
		t.Error("conservative did not take an orphan directory")
	}
	if got["merged-dirty"] {
		t.Error("conservative took a dirty worktree; that is moderate's job")
	}
	if got["solo"] {
		t.Error("conservative took an own-branch worktree; that is aggressive's job")
	}
	if reason := sparedReason(r, "merged-dirty"); reason != skipLevel {
		t.Errorf("merged-dirty spared for %q, want %q", reason, skipLevel)
	}
	if reason := sparedReason(r, "solo"); reason != skipLevel {
		t.Errorf("solo spared for %q, want %q", reason, skipLevel)
	}
}

func TestClean_ModerateAddsDirtyLanded(t *testing.T) {
	f := newCleanFixture(t)
	dirty := f.addWorktree("merged-dirty")
	f.mergeIntoMain("merged-dirty")
	mustWrite(t, filepath.Join(dirty, "scratch.txt"), "uncommitted\n")
	f.seedRun("merged-dirty", store.RunStatusFinished)

	f.addWorktree("solo")
	f.seedRun("solo", store.RunStatusFinished)

	r := f.run(CleanOptions{Level: CleanModerate, OlderThan: 0, Apply: true})
	if !deletedPaths(r)["merged-dirty"] {
		t.Error("moderate did not take a landed worktree with uncommitted files")
	}
	if deletedPaths(r)["solo"] {
		t.Error("moderate took an own-branch worktree")
	}
}

// Aggressive takes own-branch worktrees — and the point of allowing it is
// that the commits survive: the branch is a ref in the parent repo.
func TestClean_AggressiveTakesOwnBranchAndCommitsSurvive(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("solo")
	f.seedRun("solo", store.RunStatusFinished)
	head := f.git(path, "rev-parse", "HEAD")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if !deletedPaths(r)["solo"] {
		t.Fatal("aggressive did not take an own-branch worktree")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree still on disk: %v", err)
	}
	// The commit must still be reachable from the branch in the repo.
	if got := f.git(f.repo, "rev-parse", "iterion/run/solo"); got != head {
		t.Fatalf("branch tip is %s, want the deleted worktree's HEAD %s", got, head)
	}
}

// --- filters and effects --------------------------------------------------

func TestClean_DryRunIsTheDefaultAndDeletesNothing(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	r := f.run(CleanOptions{OlderThan: 0}) // Apply left false
	if !r.DryRun {
		t.Error("result does not report a dry run")
	}
	if r.DeletedCount != 1 {
		t.Fatalf("reported %d candidates, want 1", r.DeletedCount)
	}
	if r.Deleted[0].Deleted {
		t.Error("a dry run marked an entry as deleted")
	}
	if r.BytesReclaimed <= 0 {
		t.Error("dry run reported no reclaimable bytes; the preview is the whole point")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dry run removed the worktree: %v", err)
	}
}

func TestClean_OlderThanSparesRecentWorktrees(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("fresh")
	f.mergeIntoMain("fresh")
	f.seedRun("fresh", store.RunStatusFinished)

	r := f.run(CleanOptions{OlderThan: 168 * time.Hour, Apply: true})
	if deletedPaths(r)["fresh"] {
		t.Fatal("deleted a worktree younger than --older-than")
	}
	if got := sparedReason(r, "fresh"); got != skipTooRecent {
		t.Fatalf("spared for %q, want %q", got, skipTooRecent)
	}
}

func TestClean_KeepLastSparesTheMostRecentEligible(t *testing.T) {
	f := newCleanFixture(t)
	base := time.Now().Add(-90 * 24 * time.Hour)
	for i, id := range []string{"old", "mid", "new"} {
		f.addWorktree(id)
		f.mergeIntoMain(id)
		f.seedRun(id, store.RunStatusFinished)
		ts := base.Add(time.Duration(i) * 24 * time.Hour)
		p := filepath.Join(f.store, "worktrees", id)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	r := f.run(CleanOptions{OlderThan: 0, KeepLast: 1, Apply: true})
	got := deletedPaths(r)
	if !got["old"] || !got["mid"] {
		t.Errorf("expected the two oldest to go, deleted %v", got)
	}
	if got["new"] {
		t.Error("--keep-last 1 did not spare the most recent worktree")
	}
	if reason := sparedReason(r, "new"); reason != skipKeepLast {
		t.Errorf("spared for %q, want %q", reason, skipKeepLast)
	}
}

// Deleting the directory without pruning leaves an administrative entry
// behind; enough of those and `git worktree list` stops being readable.
func TestClean_PrunesStaleWorktreeRegistrations(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	before := f.git(f.repo, "worktree", "list")
	if !strings.Contains(before, "merged") {
		t.Fatalf("fixture did not register the worktree:\n%s", before)
	}

	r := f.run(CleanOptions{OlderThan: 0, Apply: true})
	if len(r.ReposPruned) != 1 {
		t.Fatalf("pruned %d repos, want 1: %v", len(r.ReposPruned), r.ReposPruned)
	}
	after := f.git(f.repo, "worktree", "list")
	if strings.Contains(after, "worktrees/merged") {
		t.Fatalf("stale registration survived:\n%s", after)
	}
}

// The run record is evidence — the journal of what the agent did. It is
// small, and it outlives the checkout by default.
func TestClean_KeepsRunRecordUnlessWithRuns(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	f.run(CleanOptions{OlderThan: 0, Apply: true})
	s, err := store.New(f.store)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := s.LoadRun(context.Background(), "merged"); err != nil {
		t.Fatalf("run record was deleted without --with-runs: %v", err)
	}
}

func TestClean_WithRunsDeletesThePairedRecord(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	r := f.run(CleanOptions{OlderThan: 0, Apply: true, WithRuns: true})
	if len(r.Deleted) != 1 || !r.Deleted[0].RunDeleted {
		t.Fatalf("--with-runs did not report the run record as deleted: %+v", r.Deleted)
	}
	s, err := store.New(f.store)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := s.LoadRun(context.Background(), "merged"); err == nil {
		t.Fatal("--with-runs left the run record behind")
	}
}

// A worktree whose run.json cannot be read must be spared: "unreadable"
// is not "absent", and guessing the permissive way is how a sweep eats a
// live run.
func TestClean_SparesWorktreeWhoseRunRecordIsUnreadable(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	runJSON := filepath.Join(f.store, "runs", "merged", "run.json")
	if _, err := os.Stat(runJSON); err != nil {
		t.Skipf("run.json not at the expected path: %v", err)
	}
	mustWrite(t, runJSON, "{ this is not json")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["merged"] {
		t.Fatal("deleted a worktree whose run record could not be read")
	}
	if got := sparedReason(r, "merged"); got != skipRunActive {
		t.Fatalf("spared for %q, want %q", got, skipRunActive)
	}
}

func TestClean_RejectsUnknownLevel(t *testing.T) {
	f := newCleanFixture(t)
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputJSON}
	err := RunClean(CleanOptions{StoreDir: f.store, Level: CleanLevel("nuke")}, p)
	if err == nil {
		t.Fatal("an unknown level was accepted")
	}
	if !strings.Contains(err.Error(), "conservative") {
		t.Errorf("error does not name the allowed levels: %v", err)
	}
}

func TestClean_RejectsAllProjectsWithStoreDir(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputJSON}
	err := RunClean(CleanOptions{
		StoreDir:    t.TempDir(),
		Level:       CleanConservative,
		AllProjects: true,
	}, p)
	if err == nil {
		t.Fatal("--all-projects with --store-dir was accepted")
	}
}

// A store with no worktrees directory is an ordinary state, not a failure.
func TestClean_EmptyStoreIsNotAnError(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputJSON}
	if err := RunClean(CleanOptions{
		StoreDir: t.TempDir(),
		Level:    CleanConservative,
	}, p); err != nil {
		t.Fatalf("RunClean on an empty store: %v", err)
	}
	r := decodeCleanResult(t, buf.Bytes())
	if r.Scanned != 0 || r.DeletedCount != 0 {
		t.Fatalf("empty store reported scanned=%d deleted=%d", r.Scanned, r.DeletedCount)
	}
}
