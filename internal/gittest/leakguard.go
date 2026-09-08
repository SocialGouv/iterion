package gittest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// NoWorktreeLeaks runs m and returns the exit code the suite should use,
// turning a leaked worktree registration into a failure.
//
// Wire it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(gittest.NoWorktreeLeaks(m)) }
//
// What it catches. A `worktree: auto` run takes its source repository from
// the workspace it is launched in, and an engine constructed without an
// explicit workDir defaults to the process cwd — the package directory,
// inside the developer's own checkout. `git worktree add` then registers the
// run's worktree in the REAL repository, while the checkout itself lives
// under t.TempDir() and is deleted the moment the test returns. Nothing ever
// comes back for the registration: it keeps an index, a HEAD and a logs/
// tree (~1.3 MB each), and every git command that enumerates worktrees walks
// all of them. Measured on one developer machine after two days of parallel
// agent work: 1 773 dead entries, 2.5 GB (issue #870).
//
// The verdict is the observable effect — the entry count of the repository
// the tests run in — not an argv reading, so it stays true however the run
// reached `git worktree add`.
//
// Concurrency. `go test ./...` runs sibling packages against this same
// repository, so an entry that appeared during m.Run() may be another
// package's LIVE worktree. Only entries whose recorded directory is GONE are
// claimed, and only when that directory sits under the test temp root: a
// live worktree still exists, and an operator's checkout on an unmounted
// volume — the case that makes `git worktree prune` unusable — is never
// under os.TempDir().
//
// That leaves one honest imprecision, observed on this repository: the
// registry is shared by every checkout of it (a linked worktree's .git points
// at the main repository's), so a leak from a sibling package — or from an
// agent's own worktree running the suite at the same time — appears inside
// this window and is reported here. The report names the test behind each
// path, which is what makes it actionable regardless of who ran it.
func NoWorktreeLeaks(m *testing.M) int {
	root, ok := worktreeAdminRoot()
	if !ok {
		// Not inside a git repository (or git is unavailable): there is no
		// registry to leak into.
		return m.Run()
	}
	before := adminEntries(root)

	code := m.Run()

	leaked := reclaimLeaked(root, before)
	if len(leaked) == 0 {
		return code
	}
	fmt.Fprintf(os.Stderr, `
FAIL: %d dead worktree registration(s) appeared in %s while this package ran.

A run with `+"`worktree: auto`"+` (the IR default) used the checkout the tests
run in as its source repository, so git registered the run's worktree HERE
while the checkout itself lived under t.TempDir() and is now gone. Give the
run a workspace the test owns — internal/gittest.SourceRepo(t) passed as
runtime.WithWorkDir / runview.WithWorkDir — or, when the real checkout is
genuinely the point, unregister it with gittest.RemoveWorktree(t, repo, path).

The entries have been reclaimed by recorded path, so the repository is clean
again; the failure is the leak, not its residue.

Each line names the test that owns the path. That test is not necessarily in
THIS package: a sibling package (or a second worktree of the same repository)
running concurrently shares this registry, and the window is all of m.Run().
Fix the test the path names.

Leaked (recorded path -> the test that owns it):
  %s
`, len(leaked), root, strings.Join(leaked, "\n  "))
	if code == 0 {
		return 1
	}
	return code
}

// worktreeAdminRoot returns <git-common-dir>/worktrees for the repository
// containing the process working directory.
func worktreeAdminRoot() (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	common, err := commonDir(cwd)
	if err != nil {
		return "", false
	}
	return filepath.Join(common, "worktrees"), true
}

// adminEntries is the set of administrative entry names present now. A
// missing worktrees/ directory is an empty set, not an error: git creates it
// with the first linked worktree.
func adminEntries(root string) map[string]bool {
	set := map[string]bool{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return set
	}
	for _, e := range entries {
		set[e.Name()] = true
	}
	return set
}

// reclaimLeaked removes the administrative entries that appeared since
// `before`, point at a directory under the test temp root, and whose
// directory no longer exists. Returns one human line per reclaimed entry.
func reclaimLeaked(root string, before map[string]bool) []string {
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}
	tmp = filepath.Clean(tmp) + string(filepath.Separator)

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var leaked []string
	for _, e := range entries {
		if before[e.Name()] {
			continue
		}
		admin := filepath.Join(root, e.Name())
		raw, err := os.ReadFile(filepath.Join(admin, "gitdir")) // #nosec G304 -- under git's own admin dir
		if err != nil {
			continue
		}
		wt := filepath.Clean(filepath.Dir(strings.TrimSpace(string(raw))))
		if !strings.HasPrefix(wt+string(filepath.Separator), tmp) {
			// Outside the test temp root: not ours to judge, let alone remove.
			continue
		}
		if _, err := os.Stat(wt); err == nil {
			// Still on disk — a sibling package's live worktree, or one this
			// package is about to remove itself. Absence is what makes an
			// entry dead.
			continue
		}
		if err := os.RemoveAll(admin); err != nil {
			leaked = append(leaked, fmt.Sprintf("%s -> %s (reclaim failed: %v)", wt, ownerTest(wt, tmp), err))
			continue
		}
		leaked = append(leaked, fmt.Sprintf("%s -> %s", wt, ownerTest(wt, tmp)))
	}
	sort.Strings(leaked)
	return leaked
}

// ownerTest names the test that owns a leaked path. t.TempDir() roots at
// <tmp>/<TestName><random>/<n>, so the first element under the temp root
// carries the test's name — the only breadcrumb left once the directory is
// gone.
func ownerTest(wt, tmpPrefix string) string {
	rest := strings.TrimPrefix(wt, tmpPrefix)
	first, _, _ := strings.Cut(filepath.ToSlash(rest), "/")
	name := strings.TrimRight(first, "0123456789")
	if name == "" {
		return "unknown test"
	}
	return name
}
