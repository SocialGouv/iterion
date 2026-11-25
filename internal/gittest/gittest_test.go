package gittest

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/testfs"
)

// The oracle is git's OWN configuration resolution, not the argv the helper
// assembled: `git config --get` answers with the value git actually parsed
// and applied for that invocation. An argv shim would only prove the flag
// reached the command line — the lesson this repo paid a production
// regression for — and on the host's git 2.43 no maintenance process detaches
// either way, so a timing observation proves nothing here.
//
// Falsified by construction: without `-c maintenance.auto=…`, both keys are
// unset and `git config --get` exits 1 (checked below on the same repo).
func TestCmd_GitItselfReportsAutoMaintenanceOff(t *testing.T) {
	// Keep this control independent of InitRepo's persisted fixture policy.
	repo := t.TempDir()
	Run(t, repo, "init", "-q", "-b", "main")

	for _, tc := range []struct{ key, want string }{
		{"maintenance.auto", "false"},
		{"gc.auto", "0"},
	} {
		if got := Run(t, repo, "config", "--get", tc.key); got != tc.want {
			t.Errorf("git resolved %s = %q, want %q — a writing command may detach `git maintenance run --auto` into a directory the test is about to delete", tc.key, got, tc.want)
		}
		// Without Cmd's flags, Git must report each key as unset (exit 1).
		bare := exec.Command("git", "config", "--get", tc.key) // #nosec G204 -- fixed config keys
		bare.Dir = repo
		bare.Env = Env()
		out, err := bare.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(out) != 0 {
			t.Fatalf("control git config --get %s = %q, %v; want unset (exit 1)", tc.key, out, err)
		}
	}
}

// Production commands spawned during tests do not use Cmd. The repository's
// common config must protect both its checkout and linked run worktrees.
func TestSourceRepo_BareGitReportsAutoMaintenanceOff(t *testing.T) {
	repo := SourceRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	Run(t, repo, "worktree", "add", "--detach", linked, "HEAD")
	for _, checkout := range []struct{ name, dir string }{{"source", repo}, {"linked", linked}} {
		t.Run(checkout.name, func(t *testing.T) {
			for _, tc := range []struct{ key, want string }{
				{"maintenance.auto", "false"},
				{"gc.auto", "0"},
			} {
				bare := exec.Command("git", "config", "--get", tc.key) // #nosec G204 -- fixed config keys
				bare.Dir = checkout.dir
				bare.Env = Env()
				out, err := bare.CombinedOutput()
				if got := strings.TrimSpace(string(out)); err != nil || got != tc.want {
					t.Errorf("bare git resolved %s = %q, %v; want %q from fixture config", tc.key, got, err, tc.want)
				}
			}
		})
	}
}

// A commit must go through, on a machine with no global git identity — which
// is every CI runner. Env carries the identity; the repo config carries it
// for the commands iterion itself spawns during a test.
func TestSourceRepo_CommitsWithoutAGlobalIdentity(t *testing.T) {
	repo := SourceRepo(t)

	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	Run(t, repo, "add", "f.txt")
	Run(t, repo, "commit", "-q", "-m", "work")

	if branch := Run(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); branch != "main" {
		t.Errorf("branch = %q, want main (a fixture that builds on `main` must not depend on the host's init.defaultBranch)", branch)
	}
	if n := len(strings.Split(Run(t, repo, "log", "--format=%H"), "\n")); n != 2 {
		t.Errorf("commit count = %d, want 2 (the seed plus this one)", n)
	}
	// The repository's own config must carry the identity too: a git command
	// iterion spawns during the run does not inherit Env.
	if got := Run(t, repo, "config", "--get", "user.email"); got != "test@iterion.invalid" {
		t.Errorf("repo user.email = %q, want the pinned test identity", got)
	}
}

// The operator's ~/.gitconfig must not be able to decide whether a test
// passes. commit.gpgsign is the setting that actually does it: with a real
// global config in reach, every commit a fixture makes would try to sign and
// fail on a machine with no key.
func TestEnv_TheOperatorsGlobalConfigCannotReachTheCommand(t *testing.T) {
	hostile := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(hostile, []byte("[commit]\n\tgpgsign = true\n[core]\n\thooksPath = /nonexistent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", hostile)

	repo := SourceRepo(t)
	if got := Run(t, repo, "config", "--get", "commit.gpgsign"); got != "false" {
		t.Errorf("commit.gpgsign = %q, want the repository's own false — the operator's global config reached the command", got)
	}

	// And `git init` with no -b still lands on main: cutting the global config
	// off also removes init.defaultBranch, which Cmd puts back.
	bare := t.TempDir()
	Run(t, bare, "init", "-q")
	if got := Run(t, bare, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Errorf("default branch = %q, want main — a fixture's branch name must not depend on the host", got)
	}
}

func TestTry_ReturnsGitsDiagnosticInsteadOfEndingTheTest(t *testing.T) {
	repo := SourceRepo(t)
	out, err := Try(repo, "rev-parse", "--verify", "refs/heads/nope")
	if err == nil {
		t.Fatalf("resolving a missing ref succeeded: %q", out)
	}
	if out == "" {
		t.Error("Try returned an empty diagnostic — the caller has nothing to report")
	}
}

// The observable effect: after RemoveWorktree the repository holds exactly
// the administrative entries it held before, and the OTHER worktree's entry
// is untouched — the property `git worktree prune` cannot promise.
func TestRemoveWorktree_DropsOnlyItsOwnRegistration(t *testing.T) {
	repo := SourceRepo(t)
	before := countAdmin(t, repo)

	mine := filepath.Join(t.TempDir(), "mine")
	other := filepath.Join(t.TempDir(), "other")
	Run(t, repo, "worktree", "add", "--detach", mine, "HEAD")
	Run(t, repo, "worktree", "add", "--detach", other, "HEAD")
	if n := countAdmin(t, repo); n != before+2 {
		t.Fatalf("admin entries after two adds = %d, want %d", n, before+2)
	}

	// The directory vanishes first — exactly what t.TempDir() does at the end
	// of a test, and the state in which the registration is orphaned.
	if err := os.RemoveAll(mine); err != nil {
		t.Fatal(err)
	}
	RemoveWorktree(t, repo, mine)

	if n := countAdmin(t, repo); n != before+1 {
		t.Errorf("admin entries after RemoveWorktree = %d, want %d (its own entry gone, the other kept)", n, before+1)
	}
	if _, err := os.Stat(filepath.Join(other, ".git")); err != nil {
		t.Errorf("the sibling worktree lost its pointer: %v", err)
	}
	if out, err := Try(repo, "rev-parse", "--verify", "HEAD"); err != nil {
		t.Errorf("the repository is no longer readable after the prune: %v\n%s", err, out)
	}
}

func countAdmin(t *testing.T, repo string) int {
	t.Helper()
	common, err := commonDir(repo)
	if err != nil {
		t.Fatalf("common dir: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read admin dir: %v", err)
	}
	return len(entries)
}

// reclaimLeaked is what the TestMain guard runs. With no per-process root
// (the degraded mode) it must claim a dead temp-rooted entry, and refuse the
// two shapes that make `git worktree prune` unusable: a LIVE entry (a
// sibling package's worktree, mid-run) and an entry outside the test temp
// root (an operator's checkout on an unmounted volume, which is absent for
// reasons that are none of this guard's business).
func TestReclaimLeaked_ClaimsDeadTempEntriesOnly(t *testing.T) {
	repo := SourceRepo(t)
	common, err := commonDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(common, "worktrees")

	// The temp root the guard compares against, resolved the same way.
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}

	live := filepath.Join(t.TempDir(), "live")
	dead := filepath.Join(tmp, "TestSomethingLeaky1234", "001", "worktrees", "run-x")
	outside := filepath.Join(t.TempDir(), "elsewhere")
	Run(t, repo, "worktree", "add", "--detach", live, "HEAD")
	Run(t, repo, "worktree", "add", "--detach", outside, "HEAD")
	// `dead` never existed as a directory; only its registration does. That
	// is the end state of a leaked run worktree.
	writeAdmin(t, root, "dead-entry", filepath.Join(dead, ".git"))
	// An entry outside the temp root whose directory is also gone: the
	// unmounted-volume shape.
	unmounted := filepath.Join(string(filepath.Separator)+"mnt", "detached-volume", "checkout")
	writeAdmin(t, root, "unmounted-entry", filepath.Join(unmounted, ".git"))

	leaked := reclaimLeaked(root, map[string]bool{}, "", os.TempDir())

	if len(leaked) != 1 || !strings.Contains(leaked[0], "TestSomethingLeaky") {
		t.Fatalf("reclaimLeaked = %v, want exactly the dead temp entry named after its test", leaked)
	}
	if _, err := os.Stat(filepath.Join(root, "dead-entry")); !os.IsNotExist(err) {
		t.Errorf("the dead entry survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "unmounted-entry")); err != nil {
		t.Errorf("an entry outside the test temp root was reclaimed — that is the operator's checkout on an unmounted volume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(live, ".git")); err != nil {
		t.Errorf("a LIVE worktree lost its registration — a sibling package's run would die: %v", err)
	}
}

// The #2901 shape: the engine's `git worktree add` ran AFTER the owning test
// returned, so t.TempDir()'s cleanup already missed the directory — the
// registration points at a live directory, which the shared-root rule reads
// as a sibling's. Under the process's own temp root the guard claims it
// anyway (m.Run() has returned; nothing of this process is live any more),
// while a sibling process's live worktree under the shared root stays
// untouched.
func TestReclaimLeaked_TheProcessRootIsOursAliveOrNot(t *testing.T) {
	repo := SourceRepo(t)
	common, err := commonDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(common, "worktrees")

	shared, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		shared = os.TempDir()
	}
	processTmp := filepath.Join(t.TempDir(), tempRootPrefix+"12345")

	// Alive under the process root: the add outlived its test. The path
	// keeps the t.TempDir() shape so the report still names the test.
	lateAlive := filepath.Join(processTmp, "TestRunviewRegisters47820011", "001", "worktrees", "run-9")
	if err := os.MkdirAll(lateAlive, 0o750); err != nil {
		t.Fatal(err)
	}
	writeAdmin(t, root, "late-entry", filepath.Join(lateAlive, ".git"))

	// Dead under the process root: the ordinary leak, still claimed.
	lateDead := filepath.Join(processTmp, "TestRunviewRegisters47820011", "002", "worktrees", "run-10")
	writeAdmin(t, root, "late-dead-entry", filepath.Join(lateDead, ".git"))

	// A sibling process's LIVE worktree under the shared root: not ours.
	siblingLive := filepath.Join(t.TempDir(), "sibling")
	Run(t, repo, "worktree", "add", "--detach", siblingLive, "HEAD")

	// A live directory whose path shares the process root's NAME STEM —
	// a sibling process's root (iterion-gittest-<other-pid>): the prefix
	// boundary must hold, or one suite would reap another's live worktree.
	decoyRoot := processTmp + "evil"
	decoy := filepath.Join(decoyRoot, "TestOtherProcess1", "001", "wt")
	if err := os.MkdirAll(decoy, 0o750); err != nil {
		t.Fatal(err)
	}
	writeAdmin(t, root, "decoy-entry", filepath.Join(decoy, ".git"))

	// A dead entry under the shared root (a guard-less process's residue):
	// still claimed, as before.
	sharedDead := filepath.Join(shared, "TestSomethingLeaky1234", "001", "worktrees", "run-x")
	writeAdmin(t, root, "shared-dead-entry", filepath.Join(sharedDead, ".git"))

	leaked := reclaimLeaked(root, map[string]bool{}, processTmp, os.TempDir())

	if len(leaked) != 3 {
		t.Fatalf("reclaimLeaked = %v, want the two process-root entries and the dead shared one", leaked)
	}
	joined := strings.Join(leaked, "\n")
	if !strings.Contains(joined, "run-9 -> TestRunviewRegisters") {
		t.Errorf("the late LIVE entry under the process root was not claimed, or lost its test name:\n%s", joined)
	}
	if !strings.Contains(joined, "run-10 -> TestRunviewRegisters") {
		t.Errorf("the dead entry under the process root was not claimed:\n%s", joined)
	}
	if !strings.Contains(joined, "run-x -> TestSomethingLeaky") {
		t.Errorf("the dead shared-root entry was not claimed:\n%s", joined)
	}
	for _, name := range []string{"late-entry", "late-dead-entry", "shared-dead-entry"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("the reclaimed entry %s survived: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(siblingLive, ".git")); err != nil {
		t.Errorf("a sibling process's LIVE worktree lost its registration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "decoy-entry")); err != nil {
		t.Errorf("a live entry under a same-stem sibling root was reclaimed: %v", err)
	}
}

// Git records the RESOLVED path in gitdir (verified git 2.55), while
// isolateTempRoot hands t.TempDir() the unresolved string — on macOS the
// two differ at the first segment (/var → /private/var). The process root
// must match under both forms, or a live leak under it — the #2781 shape
// this guard exists for — reads as a shared-root entry and is judged
// dead-only.
func TestReclaimLeaked_TheProcessRootMatchesItsResolvedForm(t *testing.T) {
	repo := SourceRepo(t)
	common, err := commonDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(common, "worktrees")

	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o750); err != nil {
		t.Fatal(err)
	}
	via := filepath.Join(base, "via")
	if err := os.Symlink(real, via); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The process root as the run spells it (through the symlink)…
	processTmp := filepath.Join(via, tempRootPrefix+"12345")
	// …and the live leak as git records it (resolved).
	recorded := filepath.Join(real, tempRootPrefix+"12345", "TestSymlinkedLeak7", "001", "wt")
	if err := os.MkdirAll(recorded, 0o750); err != nil {
		t.Fatal(err)
	}
	writeAdmin(t, root, "resolved-entry", filepath.Join(recorded, ".git"))

	leaked := reclaimLeaked(root, map[string]bool{}, processTmp, os.TempDir())

	if len(leaked) != 1 || !strings.Contains(leaked[0], "wt -> TestSymlinkedLeak") {
		t.Fatalf("reclaimLeaked = %v, want the resolved-form process-root entry named after its test", leaked)
	}
	if _, err := os.Stat(filepath.Join(root, "resolved-entry")); !os.IsNotExist(err) {
		t.Errorf("the resolved-form entry survived: %v", err)
	}
}

// The wiring witness: this is the exact call a TestMain makes (registry,
// suite, report), so the judgement must hold through the seam itself, not
// only through reclaimLeaked with hand-built roots. Inside run — the
// stand-in for m.Run() — three registrations appear:
//
//   - under the per-process root, directory ALIVE: the #2901 shape, the add
//     ran after its test returned — claimed, and the suite fails;
//   - under the inherited shared root, directory gone: a guard-less
//     process's residue — claimed;
//   - under the inherited shared root, directory alive: a sibling process's
//     worktree — untouched.
//
// Under the old gone-only rule applied everywhere, the first entry read as a
// sibling's and the guard stayed green — the defect this suite exists to
// keep out.
func TestNoWorktreeLeaks_JudgesItsOwnRootAliveAndTheInheritedRootDeadOnly(t *testing.T) {
	repo := SourceRepo(t)
	common, err := commonDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(common, "worktrees")

	// The inherited shared root, as the process started with it.
	shared := t.TempDir()
	t.Setenv("TMPDIR", shared)
	t.Setenv("TMP", shared)
	t.Setenv("TEMP", shared)
	t.Setenv("GOTMPDIR", "")

	// The residue a guard-less process leaves is recorded the way git
	// records paths — resolved.
	resolvedShared, err := filepath.EvalSymlinks(shared)
	if err != nil {
		t.Fatal(err)
	}

	// An entry predating the run is the baseline: concurrent suites share
	// this registry, so the guard judges only entries that appeared during
	// ITS OWN m.Run() — never a registration a crashed session left behind
	// before it started, which may be a concurrent guarded process's to
	// judge.
	writeAdmin(t, root, "pre-existing-entry", filepath.Join(resolvedShared, "TestPreExisting9", "001", "wt", ".git"))

	var processRoot string
	report := &bytes.Buffer{}
	code := noWorktreeLeaks(root, func() int {
		processRoot = os.TempDir()
		if processRoot == shared {
			t.Error("inside run, os.TempDir() still answers the inherited shared root — no per-process root was made")
		}

		// Alive under the process root: the #2901 leak.
		late := filepath.Join(processRoot, "TestLate1", "001", "wt")
		if err := os.MkdirAll(late, 0o750); err != nil {
			t.Fatal(err)
		}
		writeAdmin(t, root, "late-live-entry", filepath.Join(late, ".git"))

		// Dead under the inherited shared root: a guard-less process's
		// residue.
		writeAdmin(t, root, "shared-dead-entry", filepath.Join(resolvedShared, "TestSharedDead1", "001", "wt", ".git"))

		// A sibling process's LIVE worktree under the shared root.
		sibling := filepath.Join(shared, "TestSibling1", "001", "wt")
		if err := os.MkdirAll(sibling, 0o750); err != nil {
			t.Fatal(err)
		}
		writeAdmin(t, root, "sibling-live-entry", filepath.Join(sibling, ".git"))

		return 0
	}, report)

	if processRoot == "" {
		t.Fatal("run never observed a per-process root")
	}
	if code != 1 {
		t.Errorf("noWorktreeLeaks = %d, want 1 — a live registration under the process root is the #2901 leak", code)
	}
	out := report.String()
	if !strings.Contains(out, "TestLate") {
		t.Errorf("the report does not name the live #2901 entry's test:\n%s", out)
	}
	if !strings.Contains(out, "TestSharedDead") {
		t.Errorf("the report does not name the dead shared entry's test:\n%s", out)
	}
	if strings.Contains(out, "TestSibling") {
		t.Errorf("the report names a sibling process's live worktree — a live shared-root entry is not ours to judge:\n%s", out)
	}
	for _, name := range []string{"late-live-entry", "shared-dead-entry"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("the reclaimed entry %s survived: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "sibling-live-entry")); err != nil {
		t.Errorf("a sibling process's live registration was reclaimed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "pre-existing-entry")); err != nil {
		t.Errorf("an entry predating the run was reclaimed — the guard judges only entries that appeared during its own run: %v", err)
	}
	if strings.Contains(out, "TestPreExisting") {
		t.Errorf("the report names an entry that predates the run — not this suite's leak:\n%s", out)
	}
	if _, err := os.Stat(processRoot); !os.IsNotExist(err) {
		t.Errorf("the per-process temp root %s was not removed at the end of the suite: %v", processRoot, err)
	}
}

// A first reclaim pass that fails and a second pass that succeeds must read
// as one SUCCESS line, not as the stale failure: the report says the entry
// was reclaimed, and the registry agrees — keeping the pass-1 "(reclaim
// failed: …)" line would describe an entry that is gone. The seam injected
// here fails exactly the first removal of the flaky entry; the second pass
// re-judges it (its admin dir survived) and removes it for real.
func TestNoWorktreeLeaks_ARetryThatReclaimsReplacesTheFailureLine(t *testing.T) {
	repo := SourceRepo(t)
	common, err := commonDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(common, "worktrees")

	shared := t.TempDir()
	t.Setenv("TMPDIR", shared)
	t.Setenv("TMP", shared)
	t.Setenv("TEMP", shared)
	t.Setenv("GOTMPDIR", "")

	// The residue is recorded the way git records paths — resolved.
	resolvedShared, err := filepath.EvalSymlinks(shared)
	if err != nil {
		t.Fatal(err)
	}

	real := removeAdminEntry
	failed := false
	t.Cleanup(func() { removeAdminEntry = real })
	removeAdminEntry = func(path string) error {
		if strings.Contains(path, "flaky-entry") && !failed {
			failed = true
			return errors.New("injected first-pass reclaim failure")
		}
		return real(path)
	}

	report := &bytes.Buffer{}
	code := noWorktreeLeaks(root, func() int {
		// Dead under the shared root: judged on every pass, and the first
		// removal fails by injection.
		writeAdmin(t, root, "flaky-entry", filepath.Join(resolvedShared, "TestFlakyRetry2", "001", "wt", ".git"))
		return 0
	}, report)

	if code != 1 {
		t.Errorf("noWorktreeLeaks = %d, want 1 — a registration that appeared during the run is a leak however its removal goes:\n%s", code, report.String())
	}
	out := report.String()
	if !strings.Contains(out, "TestFlakyRetry") {
		t.Errorf("the report does not name the retried entry's test:\n%s", out)
	}
	if strings.Contains(out, "reclaim failed") {
		t.Errorf("the report keeps a failure line for an entry the pass-2 retry reclaimed:\n%s", out)
	}
	if n := strings.Count(out, " -> TestFlakyRetry"); n != 1 {
		t.Errorf("the owner column names the retried entry %d times, want one line per recorded path:\n%s", n, out)
	}
	if _, err := os.Stat(filepath.Join(root, "flaky-entry")); !os.IsNotExist(err) {
		t.Errorf("the retried entry survived the second pass: %v", err)
	}
}

// One registration must read as one FAIL line, even when the first reclaim
// pass cannot remove it. The registry is shared by every checkout of the
// repository, so an admin entry dir this process cannot delete — owned by
// another user, or on a read-only mount — survives pass 1 with a
// "(reclaim failed: …)" line, and the second pass re-reads the registry and
// re-judges the very same entry. Reporting it twice counts one registration
// as two.
func TestNoWorktreeLeaks_ReportsAStuckEntryOnce(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root removes a 0500 admin dir anyway — the stuck-entry shape needs an unprivileged process")
	}
	repo := SourceRepo(t)
	common, err := commonDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(common, "worktrees")

	shared := t.TempDir()
	t.Setenv("TMPDIR", shared)
	t.Setenv("TMP", shared)
	t.Setenv("TEMP", shared)
	t.Setenv("GOTMPDIR", "")

	// The entry dir stays stuck through the whole run (that is the shape
	// under test): give the framework's TempDir cleanup back the right to
	// remove it, whatever the verdict turned out to be.
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Join(root, "stuck-entry"), 0o700); err != nil && !os.IsNotExist(err) {
			t.Errorf("the stuck entry's permissions could not be restored: %v", err)
		}
	})

	report := &bytes.Buffer{}
	code := noWorktreeLeaks(root, func() int {
		stuck := filepath.Join(os.TempDir(), "TestRetryDup66", "001", "wt")
		if err := os.MkdirAll(stuck, 0o750); err != nil {
			t.Fatal(err)
		}
		writeAdmin(t, root, "stuck-entry", filepath.Join(stuck, ".git"))
		// Write access is what removing the entry's children needs: without
		// it the guard's RemoveAll fails on every attempt, pass after pass.
		if err := os.Chmod(filepath.Join(root, "stuck-entry"), 0o500); err != nil {
			t.Fatal(err)
		}
		return 0
	}, report)

	if _, err := os.Stat(filepath.Join(root, "stuck-entry")); err != nil {
		t.Fatalf("the stuck entry was removed anyway; the test does not exercise the shape whose removal fails: %v", err)
	}
	if code != 1 {
		t.Errorf("noWorktreeLeaks = %d, want 1 — a stuck registration is a real leak:\n%s", code, report.String())
	}
	out := report.String()
	if n := strings.Count(out, "TestRetryDup66"); n != 1 {
		t.Errorf("the report names the stuck registration %d times, want exactly one — the second reclaim pass must not re-report a recorded path pass 1 already reported:\n%s", n, out)
	}
	if n := strings.Count(out, "reclaim failed"); n != 1 {
		t.Errorf("the report carries %d reclaim-failure lines, want 1:\n%s", n, out)
	}
}

// isolateTempRoot is what makes the process-root judgement sound: after it,
// os.TempDir() answers the root, and every t.TempDir() of the process lands
// under it. A SUBTEST asserts the landing: the outer test already created
// its temp dir before the redirect (t.TempDir caches the parent at first
// call), so this test's own call would answer the inherited root and prove
// nothing.
func TestIsolateTempRoot_RedirectsTempDir(t *testing.T) {
	shared := t.TempDir()
	t.Setenv("TMPDIR", shared)
	t.Setenv("TMP", shared)
	t.Setenv("TEMP", shared)
	t.Setenv("GOTMPDIR", "")

	parent, dir, err := isolateTempRoot()
	if err != nil {
		t.Fatalf("isolateTempRoot: %v", err)
	}
	t.Cleanup(func() { testfs.RemoveAll(t, shared, dir) })

	if got := os.TempDir(); got != dir {
		t.Errorf("os.TempDir() = %q, want the per-process root %q", got, dir)
	}
	if filepath.Dir(dir) != filepath.Clean(parent) || !strings.HasPrefix(filepath.Base(dir), tempRootPrefix) {
		t.Errorf("the root %q is not a %s directory under the inherited temp dir %q", dir, tempRootPrefix, parent)
	}

	t.Run("TempDirLandsUnderTheProcessRoot", func(t *testing.T) {
		inner := t.TempDir()
		if !strings.HasPrefix(inner, filepath.Clean(dir)+string(filepath.Separator)) {
			t.Errorf("t.TempDir() = %q, want a path under the per-process root %q", inner, dir)
		}
	})
}

// t.TempDir honours GOTMPDIR before TMPDIR (go/src/testing), so a root made
// while the environment carries a GOTMPDIR must redirect it too — left
// alone, every temp dir of the suite would land outside the root and the
// guard would judge nothing.
func TestIsolateTempRoot_RedirectsAPresetGOTMPDIR(t *testing.T) {
	shared := t.TempDir()
	elsewhere := t.TempDir()
	t.Setenv("TMPDIR", shared)
	t.Setenv("TMP", shared)
	t.Setenv("TEMP", shared)
	t.Setenv("GOTMPDIR", elsewhere)

	_, dir, err := isolateTempRoot()
	if err != nil {
		t.Fatalf("isolateTempRoot: %v", err)
	}
	t.Cleanup(func() { testfs.RemoveAll(t, shared, dir) })

	t.Run("TempDirIgnoresTheInheritedGOTMPDIR", func(t *testing.T) {
		inner := t.TempDir()
		if !strings.HasPrefix(inner, filepath.Clean(dir)+string(filepath.Separator)) {
			t.Errorf("t.TempDir() = %q, want a path under the per-process root %q", inner, dir)
		}
		if strings.HasPrefix(inner, elsewhere+string(filepath.Separator)) {
			t.Errorf("t.TempDir() = %q honoured the inherited GOTMPDIR %q; the process root would judge nothing", inner, elsewhere)
		}
	})
}

// removeTempRoot removes exactly what isolateTempRoot returns and nothing
// else: no prefix, a prefix on a directory that is not a direct child of
// the parent, or a relative path — none of it is removed, what those
// directories hold survives, and each refusal is reported. A silent no-op
// would hide a miscomputed root; a removal would delete a bystander's data.
func TestRemoveTempRoot_RefusesWhatItDidNotCreate(t *testing.T) {
	parent := t.TempDir()

	stranger := filepath.Join(parent, "someone-elses")
	if err := os.MkdirAll(filepath.Join(stranger, "inside"), 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(stranger, "inside", "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Carries the prefix but is not a direct child of parent.
	nested := filepath.Join(parent, "wrapper", tempRootPrefix+"999")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}

	report := &bytes.Buffer{}
	removeTempRoot(parent, stranger, report)
	removeTempRoot(parent, nested, report)
	removeTempRoot(parent, tempRootPrefix+"998", report) // relative

	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("a directory without the prefix was removed — removeTempRoot touches only what isolateTempRoot created: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("a prefixed directory that is not a direct child of the parent was removed: %v", err)
	}
	if n := strings.Count(report.String(), "refusing"); n != 3 {
		t.Errorf("removeTempRoot reported %d refusals, want 3 — a silent no-op would hide a miscomputed root:\n%s", n, report.String())
	}

	// The one shape it may remove: the root isolateTempRoot returns.
	ours := filepath.Join(parent, tempRootPrefix+"1000")
	if err := os.MkdirAll(filepath.Join(ours, "residue"), 0o750); err != nil {
		t.Fatal(err)
	}
	removeTempRoot(parent, ours, &bytes.Buffer{})
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Errorf("a root isolateTempRoot created was not removed: %v", err)
	}
}

func writeAdmin(t *testing.T, root, name, gitdir string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gitdir"), []byte(gitdir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerTest_NamesTheTestBehindALeakedPath(t *testing.T) {
	got := ownerTest(filepath.Join("TestRewindThenResume_SkipsUpstreamNodes3725362394", "001", "worktrees", "e2e-rewind-mini"))
	if got != "TestRewindThenResume_SkipsUpstreamNodes" {
		t.Errorf("ownerTest = %q, want the test name without t.TempDir()'s random suffix", got)
	}
}

// LinkedWorktrees is the oracle a test asserts the engine's worktree cleanup
// with, so it must name the linked worktrees and NOT the main checkout — and
// it must keep doing so when git writes a line to stderr, which `Try`'s
// combined output folds into the listing. A reading keyed on the first line
// would then report the main checkout as a leak.
func TestLinkedWorktrees_NamesTheLinkedOnesNotTheMainCheckout(t *testing.T) {
	repo := SourceRepo(t)
	if linked := LinkedWorktrees(t, repo); len(linked) != 0 {
		t.Fatalf("a fresh repository reports linked worktrees %v", linked)
	}

	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	Run(t, repo, "worktree", "add", "--detach", first, "HEAD")
	Run(t, repo, "worktree", "add", "--detach", second, "HEAD")

	got := LinkedWorktrees(t, repo)
	if len(got) != 2 {
		t.Fatalf("LinkedWorktrees = %v, want the two linked worktrees", got)
	}
	for _, want := range []string{first, second} {
		if !slices.ContainsFunc(got, func(p string) bool { return canonical(p) == canonical(want) }) {
			t.Errorf("LinkedWorktrees = %v, missing %s", got, want)
		}
	}
	if slices.ContainsFunc(got, func(p string) bool { return canonical(p) == canonical(repo) }) {
		t.Errorf("LinkedWorktrees = %v, which names the main checkout %s", got, repo)
	}

	// Asked about a LINKED worktree: git still leads the listing with the
	// MAIN checkout, so a reading keyed on position drops the main checkout
	// and reports the very worktree it was asked about as a leak.
	fromLinked := LinkedWorktrees(t, first)
	if slices.ContainsFunc(fromLinked, func(p string) bool { return canonical(p) == canonical(first) }) {
		t.Errorf("read from %s, LinkedWorktrees = %v, which names the repository it was asked about", first, fromLinked)
	}
}
