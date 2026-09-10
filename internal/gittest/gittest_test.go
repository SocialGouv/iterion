package gittest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

// reclaimLeaked is what the TestMain guard runs. It must claim a dead
// temp-rooted entry, and refuse the two shapes that make `git worktree prune`
// unusable: a LIVE entry (a sibling package's worktree, mid-run) and an entry
// outside the test temp root (an operator's checkout on an unmounted volume,
// which is absent for reasons that are none of this guard's business).
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

	leaked := reclaimLeaked(root, map[string]bool{})

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
	tmp := string(filepath.Separator) + filepath.Join("tmp") + string(filepath.Separator)
	got := ownerTest(filepath.Join(tmp, "TestRewindThenResume_SkipsUpstreamNodes3725362394", "001", "worktrees", "e2e-rewind-mini"), tmp)
	if got != "TestRewindThenResume_SkipsUpstreamNodes" {
		t.Errorf("ownerTest = %q, want the test name without t.TempDir()'s random suffix", got)
	}
}
