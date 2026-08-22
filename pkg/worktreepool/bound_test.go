package worktreepool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// poolFixture builds a real repository and a store whose worktrees are
// genuine linked worktrees of it, in the shape production creates them
// (`git worktree add --detach <path> <sha>`). The bound is a set of claims
// about git state, so the tests exercise git rather than a model of it.
type poolFixture struct {
	t     *testing.T
	root  string
	repo  string
	store string
}

func newPoolFixture(t *testing.T) *poolFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	f := &poolFixture{t: t, root: root,
		repo: filepath.Join(root, "repo"), store: filepath.Join(root, "store")}
	mustMkdir(t, f.repo)
	mustMkdir(t, filepath.Join(f.store, "worktrees"))
	f.git(f.repo, "init", "--initial-branch=main")
	mustWrite(t, filepath.Join(f.repo, "README.md"), "base\n")
	f.git(f.repo, "add", "README.md")
	f.git(f.repo, "commit", "-m", "base")
	return f
}

func (f *poolFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.gitErr(dir, args...)
	if err != nil {
		f.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func (f *poolFixture) gitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitlib.SanitizeEnv(os.Environ()),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"LC_ALL=C", "LANG=C")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// idle is the shape the incident produced and the one the bound exists
// for: a run that created a worktree and failed before committing
// anything, leaving a full checkout parked at a commit the repository's
// own branch already points at.
func (f *poolFixture) idle(runID string) string {
	f.t.Helper()
	return f.worktree(runID, false)
}

// committed adds a commit of its own on top, held by nothing but the
// repository's history — the run produced work and no ref adopted it.
func (f *poolFixture) committed(runID string) string {
	f.t.Helper()
	return f.worktree(runID, true)
}

func (f *poolFixture) worktree(runID string, commit bool) string {
	f.t.Helper()
	path := filepath.Join(f.store, "worktrees", runID)
	base := f.git(f.repo, "rev-parse", "main")
	f.git(f.repo, "worktree", "add", "--detach", path, base)
	if commit {
		mustWrite(f.t, filepath.Join(path, runID+".txt"), "work by "+runID+"\n")
		f.git(path, "add", ".")
		f.git(path, "commit", "-m", "work by "+runID)
	}
	// Age them apart so "oldest first" is a decidable claim rather than a
	// coin toss between entries created in the same millisecond.
	f.age(runID, 0)
	return path
}

// age backdates a worktree so the ordering under test is the one written
// down. Ranks are minutes apart, oldest = highest rank.
func (f *poolFixture) age(runID string, rank int) {
	f.t.Helper()
	ts := time.Now().Add(-time.Duration(rank) * time.Minute)
	p := filepath.Join(f.store, "worktrees", runID)
	if err := os.Chtimes(p, ts, ts); err != nil {
		f.t.Fatalf("chtimes %s: %v", runID, err)
	}
}

// promote mirrors finalizeWorktree: a branch in the REPO at the
// worktree's HEAD. The commits are held by a ref that outlives the
// directory; nothing was built on top of them.
func (f *poolFixture) promote(runID string) {
	f.t.Helper()
	head := f.git(filepath.Join(f.store, "worktrees", runID), "rev-parse", "HEAD")
	f.git(f.repo, "branch", "--", "iterion/run/"+runID, head)
}

// checkpoint records the run's own per-run ref — iterion bookkeeping,
// reaped with the run, never a durable holder.
func (f *poolFixture) checkpoint(runID string) {
	f.t.Helper()
	head := f.git(filepath.Join(f.store, "worktrees", runID), "rev-parse", "HEAD")
	f.git(f.repo, "update-ref", "refs/iterion/runs/"+runID+"/nodes/n/0", head)
}

func (f *poolFixture) seedRun(runID string, status store.RunStatus) {
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

func (f *poolFixture) enforce(budget int) BudgetReport {
	f.t.Helper()
	r, err := EnforceBudget(f.store, budget, SweepOptions{})
	if err != nil {
		f.t.Fatalf("EnforceBudget: %v", err)
	}
	return r
}

func (f *poolFixture) exists(runID string) bool {
	_, err := os.Stat(filepath.Join(f.store, "worktrees", runID))
	return err == nil
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --- the budget dial ------------------------------------------------------

func TestResolveBudget_DefaultsAndOverrides(t *testing.T) {
	cases := []struct {
		env  string
		set  bool
		want int
	}{
		{set: false, want: DefaultBudget},
		{env: "", set: true, want: DefaultBudget},
		{env: "3", set: true, want: 3},
		{env: " 12 ", set: true, want: 12},
		{env: "off", set: true, want: 0},
		{env: "OFF", set: true, want: 0},
		{env: "none", set: true, want: 0},
		{env: "0", set: true, want: 0},
		{env: "-1", set: true, want: 0},
	}
	for _, c := range cases {
		if c.set {
			t.Setenv(BudgetEnv, c.env)
		} else {
			os.Unsetenv(BudgetEnv)
		}
		got, err := ResolveBudget()
		if err != nil {
			t.Fatalf("%s=%q: %v", BudgetEnv, c.env, err)
		}
		if got != c.want {
			t.Errorf("%s=%q resolved to %d, want %d", BudgetEnv, c.env, got, c.want)
		}
	}
}

// A ceiling that cannot be read must say so. Falling back to the default
// would leave an operator who typed `ITERION_WORKTREE_POOL_MAX=20GB`
// believing they had raised a bound that is in fact still at 8.
func TestResolveBudget_MalformedIsAnErrorNotADefault(t *testing.T) {
	t.Setenv(BudgetEnv, "20GB")
	if _, err := ResolveBudget(); err == nil {
		t.Fatal("a malformed budget was silently accepted")
	}
}

// --- the bound ------------------------------------------------------------

// The case the bound exists for. A run that failed before committing
// leaves a checkout at a commit `main` already points at: nothing in it is
// lost by deleting it, and `iterion clean --level conservative` still
// spares it as un-adopted work. Without this the bound would never bite on
// the shape that actually fills a disk.
func TestBound_ReclaimsIdleCheckoutsHeldByADurableRef(t *testing.T) {
	f := newPoolFixture(t)
	for i, id := range []string{"a", "b", "c", "d"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFailed)
		f.age(id, 10-i) // a is oldest
	}

	r := f.enforce(2)

	if r.Before != 4 || r.After != 2 {
		t.Fatalf("pool went from %d to %d, want 4 -> 2 (spared: %v)", r.Before, r.After, r.Spared)
	}
	if f.exists("a") || f.exists("b") {
		t.Error("the two oldest were not reclaimed")
	}
	if !f.exists("c") || !f.exists("d") {
		t.Error("the bound reclaimed past its ceiling")
	}
	if r.OverBudget() {
		t.Error("reported over budget after coming back under it")
	}
}

// Explicit --store-dir values are allowed to be relative. Git reports
// absolute worktree roots, so the bound must normalise before comparing
// them or every real linked worktree is misclassified as an orphan.
func TestBound_ReclaimsFromARelativeStoreDir(t *testing.T) {
	f := newPoolFixture(t)
	for i, id := range []string{"old", "new"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
		f.age(id, 2-i)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relStore, err := filepath.Rel(cwd, f.store)
	if err != nil {
		t.Fatal(err)
	}
	r, err := EnforceBudget(relStore, 1, SweepOptions{})
	if err != nil {
		t.Fatalf("EnforceBudget(relative store): %v", err)
	}
	if f.exists("old") || !f.exists("new") {
		t.Fatalf("relative store reclaimed the wrong entry: old=%v new=%v", f.exists("old"), f.exists("new"))
	}
	if len(r.Reclaimed) != 1 || r.Reclaimed[0].RunID != "old" {
		t.Fatalf("reclaimed = %+v, want old", r.Reclaimed)
	}
}

// The ceiling is a ceiling, not a sweep. An operator who sets 2 is saying
// "keep at most 2", not "empty the pool whenever it goes over".
func TestBound_TakesOnlyTheExcess(t *testing.T) {
	f := newPoolFixture(t)
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
		f.age(id, 10-i)
	}

	r := f.enforce(4)

	if len(r.Reclaimed) != 1 {
		t.Fatalf("reclaimed %d, want exactly the excess (1)", len(r.Reclaimed))
	}
	if r.Reclaimed[0].RunID != "a" {
		t.Errorf("reclaimed %q, want the oldest (a)", r.Reclaimed[0].RunID)
	}
	if r.After != 4 {
		t.Errorf("pool left at %d, want the budget 4", r.After)
	}
}

// Under the ceiling, nothing happens — and nothing is classified. This is
// what makes the bound affordable on the path that creates every worktree.
func TestBound_UnderBudgetIsANoOp(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"a", "b"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
	}

	r := f.enforce(8)

	if len(r.Reclaimed) != 0 || r.OverBudget() {
		t.Fatalf("a pool under its ceiling was acted on: %+v", r)
	}
	if r.Total != 2 || r.After != 2 {
		t.Errorf("Total/After = %d/%d, want 2/2", r.Total, r.After)
	}
	if !f.exists("a") || !f.exists("b") {
		t.Error("a worktree was deleted below the ceiling")
	}
}

// Uncommitted files are the run's own output, and preserving them for
// inspection is a feature. An eviction nobody asked for must never be the
// thing that destroys them — it warns instead, and names the level that
// would.
func TestBound_RefusesDirtyWorktreesAndReportsThemAsOverBudget(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"a", "b", "c"} {
		path := f.idle(id)
		mustWrite(t, filepath.Join(path, "half-written.txt"), "an agent got this far\n")
		f.seedRun(id, store.RunStatusFailed)
	}

	r := f.enforce(1)

	if len(r.Reclaimed) != 0 {
		t.Fatalf("the bound destroyed uncommitted work: %+v", r.Reclaimed)
	}
	if !f.exists("a") || !f.exists("b") || !f.exists("c") {
		t.Fatal("a dirty worktree was deleted")
	}
	if !r.OverBudget() {
		t.Fatal("a pool it could not bring down did not report itself over budget")
	}
	if r.Spared[SkipLevel] != 3 {
		t.Errorf("spared reasons = %v, want 3 under %q", r.Spared, SkipLevel)
	}
	if !strings.Contains(r.Summary(), "uncommitted") {
		t.Errorf("the summary does not say why: %q", r.Summary())
	}
}

func TestBound_DirtyOwnBranchRemedyIsAggressive(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"a", "b"} {
		path := f.idle(id)
		mustWrite(t, filepath.Join(path, "unfinished.txt"), "keep\n")
		f.seedRun(id, store.RunStatusFailed)
	}

	r := f.enforce(1)
	if r.Spared[SkipLevel] != 2 {
		t.Fatalf("spared reasons = %v, want dirty entries", r.Spared)
	}
	if got := r.Remedy(f.store); !strings.Contains(got, "--level aggressive") {
		t.Fatalf("Remedy = %q, want aggressive for a dirty own-branch checkout", got)
	}
}

func TestBound_RefusesIgnoredContent(t *testing.T) {
	f := newPoolFixture(t)
	mustWrite(t, filepath.Join(f.repo, ".gitignore"), "dist/\n")
	f.git(f.repo, "add", ".gitignore")
	f.git(f.repo, "commit", "-m", "ignore generated output")
	for _, id := range []string{"a", "b"} {
		path := f.idle(id)
		mustWrite(t, filepath.Join(path, "dist", "output.txt"), "could be generated or operator-owned\n")
		f.seedRun(id, store.RunStatusFinished)
	}

	r := f.enforce(1)
	if len(r.Reclaimed) != 0 || !f.exists("a") || !f.exists("b") {
		t.Fatalf("the bound deleted ignored content: %+v", r.Reclaimed)
	}
	if r.Spared[SkipIgnored] != 2 {
		t.Fatalf("spared reasons = %v, want ignored content named", r.Spared)
	}
	if got := r.Remedy(f.store); !strings.Contains(got, "--level aggressive") {
		t.Fatalf("Remedy = %q, want the level required by own-branch entries", got)
	}
}

func TestBound_DoesNotRefuseIgnoredRuntimeScaffold(t *testing.T) {
	f := newPoolFixture(t)
	mustWrite(t, filepath.Join(f.repo, ".gitignore"), "**/.claude/\n")
	f.git(f.repo, "add", ".gitignore")
	f.git(f.repo, "commit", "-m", "ignore runtime scaffold")
	for i, id := range []string{"old", "new"} {
		path := f.idle(id)
		mustWrite(t, filepath.Join(path, ".claude", "skills", "x.md"), "mirrored\n")
		f.seedRun(id, store.RunStatusFinished)
		f.age(id, 2-i)
	}

	r := f.enforce(1)
	if len(r.Reclaimed) != 1 || r.Reclaimed[0].RunID != "old" {
		t.Fatalf("runtime scaffold disabled reclamation: %+v", r)
	}
}

func TestBound_DirtyIgnoredMergedRemedyIsModerate(t *testing.T) {
	f := newPoolFixture(t)
	mustWrite(t, filepath.Join(f.repo, ".gitignore"), "dist/\n")
	f.git(f.repo, "add", ".gitignore")
	f.git(f.repo, "commit", "-m", "ignore output")
	paths := map[string]string{}
	for _, id := range []string{"a", "b"} {
		paths[id] = f.idle(id)
		f.seedRun(id, store.RunStatusFailed)
	}
	mustWrite(t, filepath.Join(f.repo, "later.txt"), "advance main\n")
	f.git(f.repo, "add", "later.txt")
	f.git(f.repo, "commit", "-m", "advance main")
	for _, path := range paths {
		mustWrite(t, filepath.Join(path, "README.md"), "dirty\n")
		mustWrite(t, filepath.Join(path, "dist", "output.txt"), "ignored\n")
	}

	r := f.enforce(1)
	if r.Spared[SkipLevel] != 2 {
		t.Fatalf("spared reasons = %v, want dirty entries to retain level reason", r.Spared)
	}
	if got := r.Remedy(f.store); !strings.Contains(got, "--level moderate") {
		t.Fatalf("Remedy = %q, want moderate for dirty merged entries", got)
	}
}

// A worktree whose commits only iterion's own per-run refs hold would lose
// them when the run is reaped, so the bound refuses it even though a ref
// technically contains its HEAD.
func TestBound_RefusesCommitsOnlyIterionsOwnRefsHold(t *testing.T) {
	f := newPoolFixture(t)
	f.committed("solo")
	f.checkpoint("solo")
	f.seedRun("solo", store.RunStatusFailed)
	f.committed("other")
	f.seedRun("other", store.RunStatusFailed)

	r := f.enforce(1)

	if !f.exists("solo") {
		t.Fatal("the bound took a worktree whose commits only a run-scoped ref holds")
	}
	if !r.OverBudget() {
		t.Error("it did not report the pool as still over budget")
	}
	if r.Spared[SkipIterionHeldOnly] == 0 {
		t.Errorf("spared reasons = %v, want one under %q", r.Spared, SkipIterionHeldOnly)
	}
	if got := r.Remedy(f.store); !strings.Contains(got, "--level aggressive") {
		t.Errorf("Remedy = %q, want aggressive clean for run-scoped refs", got)
	}
}

// Orphans carry dirty=true because git cannot account for their content.
// The bound must still name the orphan fact: moderate cannot reclaim one,
// while the aggressive dry-run it suggests can.
func TestBound_OrphanRemedyIsAggressive(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"orphan-a", "orphan-b"} {
		mustWrite(t, filepath.Join(f.store, "worktrees", id, "output.txt"), "unknown\n")
	}

	r := f.enforce(1)
	if r.Spared[SkipOrphan] != 2 {
		t.Fatalf("spared reasons = %v, want both under %q", r.Spared, SkipOrphan)
	}
	if got := r.Remedy(f.store); !strings.Contains(got, "--level aggressive") {
		t.Fatalf("Remedy = %q, want an aggressive dry run", got)
	}
}

// A promoted worktree — the shape finalizeWorktree leaves — is held by a
// branch in the parent repository, so deleting the directory keeps every
// commit. The test asserts the commits, not just the deletion.
func TestBound_TakesPromotedWorktreesAndTheCommitsSurvive(t *testing.T) {
	f := newPoolFixture(t)
	path := f.committed("landed")
	head := f.git(path, "rev-parse", "HEAD")
	f.promote("landed")
	f.seedRun("landed", store.RunStatusFinished)
	f.idle("keeper")
	f.seedRun("keeper", store.RunStatusFinished)
	f.age("landed", 5)
	f.age("keeper", 1)

	f.enforce(1)

	if f.exists("landed") {
		t.Fatal("a promoted worktree was not reclaimed")
	}
	if got := f.git(f.repo, "rev-parse", "iterion/run/landed"); got != head {
		t.Fatalf("branch tip is %s, want the deleted worktree's HEAD %s", got, head)
	}
}

// A live run always keeps its checkout, and it must not count against the
// budget either — otherwise a store running nine bots at once would report
// itself permanently over budget and warn on every launch.
func TestBound_LiveRunsAreNeitherTakenNorCounted(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"live1", "live2", "live3"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusRunning)
	}
	s, err := store.New(f.store)
	if err != nil {
		t.Fatal(err)
	}
	var locks []store.RunLock
	for _, id := range []string{"live1", "live2", "live3"} {
		lock, err := s.LockRun(context.Background(), id)
		if err != nil {
			t.Fatalf("LockRun(%s): %v", id, err)
		}
		locks = append(locks, lock)
	}
	defer func() {
		for _, lock := range locks {
			_ = lock.Unlock()
		}
	}()
	f.idle("dead")
	f.seedRun("dead", store.RunStatusFinished)

	r := f.enforce(2)

	for _, id := range []string{"live1", "live2", "live3"} {
		if !f.exists(id) {
			t.Fatalf("the bound took %s, whose run is still executing", id)
		}
	}
	if r.Held != 3 {
		t.Errorf("Held = %d, want the 3 live runs", r.Held)
	}
	if r.Before != 1 {
		t.Errorf("the budget was applied to %d entries, want only the 1 no run owns", r.Before)
	}
	if r.OverBudget() {
		t.Errorf("a store with 3 live runs reported itself over a budget of 2: %+v", r)
	}
	if !f.exists("dead") {
		t.Error("the one reclaimable entry was taken while under the ceiling")
	}
}

func TestBound_ReclaimsStaleRunningStatusAfterProcessCrash(t *testing.T) {
	f := newPoolFixture(t)
	f.idle("stale")
	f.seedRun("stale", store.RunStatusRunning)
	f.age("stale", 2)
	f.idle("new")
	f.seedRun("new", store.RunStatusFinished)
	f.age("new", 1)

	r := f.enforce(1)
	if f.exists("stale") || !f.exists("new") {
		t.Fatalf("stale running status was treated as live: stale=%v new=%v", f.exists("stale"), f.exists("new"))
	}
	if r.Held != 0 || len(r.Reclaimed) != 1 {
		t.Fatalf("report = %+v, want the stale checkout reclaimed", r)
	}
}

// `iterion resume` restarts these runs in this very worktree. Sparing them
// is the same default `iterion clean` and `runs prune` keep.
func TestBound_SparesWorktreesOfResumableRuns(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"r1", "r2", "r3"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFailedResumable)
	}

	// The exported boundary must enforce this invariant even if a caller
	// accidentally asks the underlying sweep to include resumable runs.
	r, err := EnforceBudget(f.store, 1, SweepOptions{ScanOptions: ScanOptions{IncludeResumable: true}})
	if err != nil {
		t.Fatalf("EnforceBudget: %v", err)
	}

	for _, id := range []string{"r1", "r2", "r3"} {
		if !f.exists(id) {
			t.Fatalf("the bound destroyed the resume of %s", id)
		}
	}
	if r.Spared[SkipResumable] == 0 {
		t.Errorf("spared reasons = %v, want %q", r.Spared, SkipResumable)
	}
	if !strings.Contains(r.Summary(), "resume") {
		t.Errorf("the summary does not name the resume: %q", r.Summary())
	}
}

func TestBound_SparesDormantPausedRunsWithoutIneffectiveRemedy(t *testing.T) {
	for _, status := range []store.RunStatus{
		store.RunStatusPausedOperator,
		store.RunStatusPausedWaitingHuman,
	} {
		t.Run(string(status), func(t *testing.T) {
			f := newPoolFixture(t)
			for _, id := range []string{"paused-a", "paused-b"} {
				f.idle(id)
				f.seedRun(id, status)
			}

			r := f.enforce(1)
			if !f.exists("paused-a") || !f.exists("paused-b") {
				t.Fatalf("the bound destroyed a %s resume", status)
			}
			if r.Spared[SkipPausedRun] != 2 {
				t.Fatalf("spared reasons = %v, want both paused runs named", r.Spared)
			}
			if got := r.Remedy(f.store); got != "" {
				t.Fatalf("Remedy = %q, want none because clean keeps non-terminal pauses", got)
			}
		})
	}
}

func TestBound_SparesUnreadableRunRecords(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"broken-a", "broken-b"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusRunning)
		mustWrite(t, filepath.Join(f.store, "runs", id, "run.json"), "{not json")
	}

	r := f.enforce(1)
	if !f.exists("broken-a") || !f.exists("broken-b") {
		t.Fatal("the bound deleted a checkout whose run record was unreadable")
	}
	if r.Held != 2 || len(r.Reclaimed) != 0 {
		t.Fatalf("report = %+v, want both ambiguous runs held", r)
	}
}

// A failed run normally has both facts at once: it is resumable and its
// tree is dirty and its HEAD is still its own branch. The table keeps one
// primary reason, but the suggested command must carry both the aggressive
// level and resumability flag or it reclaims nothing on the first try.
func TestBound_RemedyIncludesBothFlagsForDirtyResumableEntries(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"r1", "r2"} {
		path := f.idle(id)
		f.seedRun(id, store.RunStatusFailedResumable)
		mustWrite(t, filepath.Join(path, "unfinished.txt"), "keep me\n")
	}

	r := f.enforce(1)
	if got := r.Remedy(f.store); !strings.Contains(got, "--level aggressive") ||
		!strings.Contains(got, "--include-resumable") {
		t.Fatalf("Remedy = %q, want both dirty and resumable flags", got)
	}
}

func TestBound_DoesNotOfferResumableFlagForUnreclaimableEntries(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"a", "b"} {
		f.committed(id)
		f.seedRun(id, store.RunStatusCancelled)
	}

	r := f.enforce(1)
	if r.Spared[SkipUnlanded] != 2 {
		t.Fatalf("spared reasons = %v, want unlanded entries", r.Spared)
	}
	if got := r.Remedy(f.store); got != "" {
		t.Fatalf("Remedy = %q, want none because no clean flag can reclaim these entries", got)
	}
}

func TestLoadRunStatusesReadsOnlyRunsWithWorktrees(t *testing.T) {
	f := newPoolFixture(t)
	f.idle("parked")
	f.seedRun("parked", store.RunStatusFinished)
	f.seedRun("history-only", store.RunStatusRunning)

	got := loadRunStatuses(f.store, []string{"parked"})
	if got["parked"] != store.RunStatusFinished {
		t.Fatalf("parked status = %q, want finished", got["parked"])
	}
	if _, ok := got["history-only"]; ok {
		t.Fatal("loaded an unrelated historical run with no worktree")
	}
}

// DeleteRun leaves a tombstone. It is an intentional absence, not an
// unreadable live run: treating it as running would hide the orphaned
// checkout from both clean and the bound for the tombstone's lifetime.
func TestBound_ReclaimsAWorktreeWhoseRunWasTombstoned(t *testing.T) {
	f := newPoolFixture(t)
	for i, id := range []string{"deleted", "survivor"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
		f.age(id, 2-i)
	}
	s, err := store.New(f.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(context.Background(), "deleted"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}

	r := f.enforce(1)
	if f.exists("deleted") || !f.exists("survivor") {
		t.Fatalf("tombstoned run ownership was misread: deleted=%v survivor=%v", f.exists("deleted"), f.exists("survivor"))
	}
	if r.Held != 0 || len(r.Reclaimed) != 1 {
		t.Fatalf("report = %+v, want one reclaimed and none live-held", r)
	}
}

// A refusal may be stale safely: it can only delay a deletion, never
// admit one. Remembering it for a short interval keeps a degraded pool
// from paying several git subprocesses per leftover on every launch.
func TestBound_MemoizesOnlyRefusalsForAShortInterval(t *testing.T) {
	f := newPoolFixture(t)
	paths := map[string]string{}
	for _, id := range []string{"old", "new"} {
		paths[id] = f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
		mustWrite(t, filepath.Join(paths[id], "unfinished.txt"), "keep me\n")
	}
	at := time.Now()
	opts := SweepOptions{ScanOptions: ScanOptions{Now: func() time.Time { return at }}}

	first, err := EnforceBudget(f.store, 1, opts)
	if err != nil || !first.OverBudget() {
		t.Fatalf("first pass = %+v, err=%v, want refused dirty entries", first, err)
	}
	for _, path := range paths {
		if err := os.Remove(filepath.Join(path, "unfinished.txt")); err != nil {
			t.Fatal(err)
		}
	}

	at = at.Add(time.Second)
	second, err := EnforceBudget(f.store, 1, opts)
	if err != nil || len(second.Reclaimed) != 0 {
		t.Fatalf("memoized refusal was not reused safely: %+v, err=%v", second, err)
	}

	at = at.Add(refusalMemoTTL + time.Second)
	third, err := EnforceBudget(f.store, 1, opts)
	if err != nil || len(third.Reclaimed) != 1 {
		t.Fatalf("expired refusal was not re-derived: %+v, err=%v", third, err)
	}
}

func TestRefusalMemoHasAHardSizeCap(t *testing.T) {
	refusalMemo.Lock()
	original := refusalMemo.entries
	refusalMemo.entries = map[string]memoizedRefusal{}
	refusalMemo.Unlock()
	defer func() {
		refusalMemo.Lock()
		refusalMemo.entries = original
		refusalMemo.Unlock()
	}()

	now := time.Now()
	for i := 0; i < 300; i++ {
		rememberPoolRefusal(Entry{Path: "/memo/" + time.Duration(i).String(), SkipReason: SkipLevel}, now)
	}
	refusalMemo.Lock()
	got := len(refusalMemo.entries)
	refusalMemo.Unlock()
	if got != 256 {
		t.Fatalf("memo size = %d, want hard cap 256", got)
	}
}

func TestBound_TreatsCandidateRemovedBeforeClassificationAsVanished(t *testing.T) {
	f := newPoolFixture(t)
	for i, id := range []string{"old", "new"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
		f.age(id, 2-i)
	}
	removed := false
	r, err := EnforceBudget(f.store, 1, SweepOptions{
		beforeClassification: func(path string) {
			if !removed {
				removed = true
				_ = os.RemoveAll(path)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Spared[SkipUnlanded] != 0 || r.After != 1 || r.OverBudget() {
		t.Fatalf("concurrent disappearance misreported as unlanded: %+v", r)
	}
}

func TestBound_DoesNotCountAConcurrentlyVanishedEntryAfterThePass(t *testing.T) {
	f := newPoolFixture(t)
	for i, id := range []string{"old", "new"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
		f.age(id, 2-i)
	}
	opts := SweepOptions{AfterEligibility: func(path string) { _ = os.RemoveAll(path) }}
	r, err := EnforceBudget(f.store, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Before != 2 || r.After != 1 || r.OverBudget() {
		t.Fatalf("vanished entry still counted after pass: %+v", r)
	}
}

func TestBound_RefreshesLandingWhenHeadMovesDuringEligibility(t *testing.T) {
	f := newPoolFixture(t)
	for i, id := range []string{"old", "new"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
		f.age(id, 2-i)
	}
	mutated := false
	r, err := EnforceBudget(f.store, 1, SweepOptions{
		DuringEligibility: func(path string) {
			if mutated {
				return
			}
			mutated = true
			f.git(path, "commit", "--allow-empty", "-m", "concurrent work")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Spared[SkipUnlanded] != 1 {
		t.Fatalf("spared reasons = %v, want the concurrently changed entry to be unlanded", r.Spared)
	}
	if got := r.Remedy(f.store); got != "" {
		t.Fatalf("Remedy = %q, want none because clean cannot reclaim an unlanded entry", got)
	}
}

func TestBound_ReportsFailedRemovalsInTheAggregateWarning(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"old", "new"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
	}
	r, err := EnforceBudget(f.store, 1, SweepOptions{
		Remove: func(string) error { return os.ErrPermission },
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Spared[SkipRemovalFailed] != 2 || len(r.Errors) != 2 {
		t.Fatalf("failed pass = %+v, want both removal failures named", r)
	}
	if !strings.Contains(r.Summary(), "could not be removed") {
		t.Fatalf("Summary = %q, want the failed-removal reason", r.Summary())
	}
	if got := r.Remedy(f.store); got != "" {
		t.Fatalf("Remedy = %q, want none because no clean flag resolves a filesystem error", got)
	}
}

func TestBound_CancelledContextStopsBeforeClassification(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"old", "new"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r, err := EnforceBudget(f.store, 1, SweepOptions{
		ScanOptions: ScanOptions{Context: ctx},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Incomplete || len(r.Errors) == 0 {
		t.Fatalf("cancelled pass = %+v, want an explicit incomplete report", r)
	}
	if len(r.Reclaimed) != 0 || !f.exists("old") || !f.exists("new") {
		t.Fatalf("cancelled pass reclaimed a worktree: %+v", r.Reclaimed)
	}
}

func TestBound_ContextCancellationInterruptsTheEligibilityRecheck(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"old", "new"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r, err := EnforceBudget(f.store, 1, SweepOptions{
		ScanOptions:       ScanOptions{Context: ctx},
		DuringEligibility: func(string) { cancel() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Incomplete || len(r.Reclaimed) != 0 {
		t.Fatalf("interrupted pass = %+v, want incomplete with no reclamation", r)
	}
	if r.Spared[SkipUnlanded] != 0 || r.Spared[SkipNested] != 0 {
		t.Fatalf("context cancellation was misreported as a git verdict: %v", r.Spared)
	}
}

// Off means off: no classification, no deletion, whatever the pool holds.
func TestBound_DisabledDoesNothing(t *testing.T) {
	f := newPoolFixture(t)
	for _, id := range []string{"a", "b", "c"} {
		f.idle(id)
		f.seedRun(id, store.RunStatusFinished)
	}

	r, err := EnforceBudget(f.store, 0, SweepOptions{})
	if err != nil {
		t.Fatalf("EnforceBudget: %v", err)
	}
	if len(r.Reclaimed) != 0 || r.OverBudget() {
		t.Fatalf("a disabled bound acted: %+v", r)
	}
	for _, id := range []string{"a", "b", "c"} {
		if !f.exists(id) {
			t.Fatalf("%s was deleted with the bound off", id)
		}
	}
}

// The store's own state lives beside the run worktrees under a dot-prefix
// and the sweep never touches it — so it must not count against a budget
// it could never bring back down.
func TestBound_DotPrefixedStateIsNotAPoolEntry(t *testing.T) {
	f := newPoolFixture(t)
	mustWrite(t, filepath.Join(f.store, "worktrees", ".state", "gate.db"), "state\n")
	f.idle("a")
	f.seedRun("a", store.RunStatusFinished)

	r := f.enforce(1)

	if r.Total != 1 {
		t.Fatalf("Total = %d, want 1 — `.state` was counted as a worktree", r.Total)
	}
	if !f.exists("a") {
		t.Error("the one real entry was reclaimed while at the ceiling")
	}
	if _, err := os.Stat(filepath.Join(f.store, "worktrees", ".state", "gate.db")); err != nil {
		t.Errorf("the store's own state was touched: %v", err)
	}
}

// A store that has never run a `worktree: auto` bot has no pool at all.
func TestBound_MissingPoolIsNotAnError(t *testing.T) {
	root := t.TempDir()
	r, err := EnforceBudget(root, 8, SweepOptions{})
	if err != nil {
		t.Fatalf("a store with no worktrees dir errored: %v", err)
	}
	if r.Total != 0 || r.OverBudget() {
		t.Fatalf("an empty store reported %+v", r)
	}
}

// --- the bound's admission is not one of the levels ------------------------

// The load-bearing claim of the whole design, pinned against git rather
// than asserted in prose: an idle checkout is `own-branch`, which the
// LEVELS put at `aggressive` because nothing has adopted the work — while
// the BOUND takes it, because a durable ref holds every commit and there
// is nothing uncommitted to lose.
//
// If this ever collapses into one rule, one of two things breaks: either
// the bound stops biting on the shape that fills disks, or `iterion clean
// --level conservative` starts deleting un-adopted work.
func TestAdmission_TheBoundAndTheLevelsDisagreeOnPurpose(t *testing.T) {
	f := newPoolFixture(t)
	f.idle("idle")
	f.seedRun("idle", store.RunStatusFailed)

	entries, err := Scan(f.store, ScanOptions{Level: LevelConservative})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("scanned %d entries, want 1", len(entries))
	}
	e := entries[0]

	if e.Landing != LandingOwnBranch {
		t.Fatalf("landing = %q, want %q — the fixture is not reproducing an idle checkout", e.Landing, LandingOwnBranch)
	}
	if !e.DurablyHeld {
		t.Fatal("DurablyHeld is false, yet `main` points at this very commit")
	}
	if e.Dirty {
		t.Fatal("an untouched checkout reads as dirty")
	}
	if e.SkipReason != SkipLevel {
		t.Errorf("conservative admitted it (skip=%q); the levels must still spare un-adopted work", e.SkipReason)
	}
	if _, ok := evictionAdmission()(e.Landing, e.Dirty, e.DurablyHeld); !ok {
		t.Error("the bound refused a checkout whose every commit a durable ref holds — it would never bite")
	}
	// And the mirror: the bound refuses what moderate takes.
	if _, ok := evictionAdmission()(LandingMerged, true, true); ok {
		t.Error("the bound admitted a dirty worktree; moderate is the operator's call, not its own")
	}
	if _, ok := evictionAdmission()(LandingOrphan, false, false); ok {
		t.Error("the bound admitted an orphan; git cannot say what is in one")
	}
}

// The suggested command must be one that would actually change the pool.
// `unlanded` and `nested-repo` are refused by every level of `iterion
// clean` too, so offering it for those promises a reclamation it cannot
// perform — the summary names them instead.
func TestRemedy_OnlyOfferedWhenAFlagWouldChangeTheOutcome(t *testing.T) {
	cases := []struct {
		name   string
		spared map[string]int
		want   string
	}{
		{"dirty", map[string]int{SkipLevel: 2}, "iterion clean --store-dir /s --older-than 0 --level moderate"},
		{"resumable", map[string]int{SkipResumable: 3}, "iterion clean --store-dir /s --older-than 0 --include-resumable"},
		{"both", map[string]int{SkipLevel: 1, SkipResumable: 1},
			"iterion clean --store-dir /s --older-than 0 --level moderate --include-resumable"},
		{"orphan", map[string]int{SkipOrphan: 1}, "iterion clean --store-dir /s --older-than 0 --level aggressive"},
		{"ignored", map[string]int{SkipIgnored: 1}, "iterion clean --store-dir /s --older-than 0"},
		{"run-scoped ref only", map[string]int{SkipIterionHeldOnly: 1}, "iterion clean --store-dir /s --older-than 0 --level aggressive"},
		{"unlanded only", map[string]int{SkipUnlanded: 4}, ""},
		{"nested only", map[string]int{SkipNested: 1}, ""},
		{"nothing spared", map[string]int{}, ""},
	}
	for _, c := range cases {
		got := BudgetReport{Spared: c.spared}.Remedy("/s")
		if got != c.want {
			t.Errorf("%s: Remedy = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRemedy_QuotesStoreDirectoryForCopyPaste(t *testing.T) {
	got := BudgetReport{Spared: map[string]int{SkipLevel: 1}}.Remedy("/tmp/iterion store/it's")
	want := `iterion clean --store-dir '/tmp/iterion store/it'"'"'s' --older-than 0 --level moderate`
	if got != want {
		t.Fatalf("Remedy = %q, want %q", got, want)
	}
}

// Every reason the bound can produce must render, or an operator reads
// "3 worktrees exceed the budget;" with nothing after the semicolon.
func TestSummary_NamesEveryReasonTheBoundCanProduce(t *testing.T) {
	for _, reason := range []string{SkipLevel, SkipResumable, SkipOrphan, SkipIgnored, SkipIterionHeldOnly, SkipUnlanded, SkipNested, SkipRunActive, SkipPausedRun, SkipRemovalFailed} {
		got := BudgetReport{Spared: map[string]int{reason: 1}}.Summary()
		if !strings.HasPrefix(got, "1 ") || len(got) < 5 {
			t.Errorf("reason %q rendered as %q", reason, got)
		}
	}
}
