package gittest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
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
// package's LIVE worktree. The guard therefore gives the process its own
// temp root — the shape internal/hometest.IsolateUserHome uses for HOME:
// TMPDIR
// (plus TMP/TEMP for Windows, and GOTMPDIR, which t.TempDir honours before
// TMPDIR) points at a directory this process alone sees, so every
// t.TempDir() of the suite lands under it. A registration pointing under
// that root is this process's to judge, ALIVE or not: m.Run() has returned,
// every test is done, and a live directory there is exactly the #2901 shape
// — the engine's `git worktree add` ran after the owning test returned, so
// t.TempDir()'s cleanup already missed it. Entries under the shared,
// inherited temp root keep the old rule — claimed only once their directory
// is gone — because a live one may be a sibling process's worktree. An
// operator's checkout on an unmounted volume — the case that makes
// `git worktree prune` unusable — is never under either.
//
// The redirect is never undone. The process exits as soon as this returns,
// and the after-run scans of the guards this composes with
// (internal/hometest) derive the prefix they match from os.TempDir() —
// restoring the variables would point those scans at the shared root, past
// everything the suite wrote under its own.
//
// That leaves one honest imprecision, observed on this repository: the
// registry is shared by every checkout of it (a linked worktree's .git points
// at the main repository's), so a DEAD entry a guard-less process left under
// the shared temp root appears inside this window and is reported here. The
// report names the test behind each path, which is what makes it actionable
// regardless of who ran it.
func NoWorktreeLeaks(m *testing.M) int {
	root, ok := worktreeAdminRoot()
	if !ok {
		// Not inside a git repository (or git is unavailable): there is no
		// registry to leak into.
		return m.Run()
	}
	return noWorktreeLeaks(root, m.Run, os.Stderr)
}

// noWorktreeLeaks is the seam behind NoWorktreeLeaks: root is the registry
// to judge, run the suite, report where the verdict and the warnings go. Its
// contract is documented on NoWorktreeLeaks.
func noWorktreeLeaks(root string, run func() int, report io.Writer) int {
	before := adminEntries(root)
	// The shared, inherited temp root is captured BEFORE the redirection
	// below: afterwards os.TempDir() answers the process root, and the
	// inherited one — where a guard-less process's residue sits — would
	// never be judged.
	inheritedTmp := os.TempDir()

	// The per-process root is load-bearing, not best-effort: without it every
	// t.TempDir() of the suite lands in the shared root, where the only rule
	// that can judge a LIVE directory is the sibling rule, and the #2901
	// leak — an add that ran after its test returned — goes unreported. The
	// suite is not run at all when the root cannot be made: a guard that
	// silently judges less than it claims is worse than one that refuses.
	parent, processTmp, err := isolateTempRoot()
	if err != nil {
		fmt.Fprintf(report, "gittest: create the per-process temp root: %v — the suite is not run, nothing would be judged\n", err)
		return 1
	}

	code := run()

	leaked := reclaimLeaked(root, before, processTmp, inheritedTmp)
	removeTempRoot(parent, processTmp, report)
	// Second pass: a stray `git worktree add` that landed in the
	// reclaim→remove window was never judged, and the removal just made its
	// directory dead — claim it now rather than leave a permanent dead entry
	// the next suite inherits. The retry also re-judges an entry whose
	// removal already failed — it gets its second chance, but pass 1 already
	// reported it, so the report counts each recorded path once. This
	// stray-append leg has no committed witness: landing a registration
	// inside the reclaim→remove window needs the removeTempRoot retry
	// window, so any witness of it is timing-based, and the repository
	// declines flaky tests over pinning a race.
	for _, line := range reclaimLeaked(root, before, processTmp, inheritedTmp) {
		path, _, _ := strings.Cut(line, " -> ")
		if i := slices.IndexFunc(leaked, func(prev string) bool {
			prevPath, _, _ := strings.Cut(prev, " -> ")
			return prevPath == path
		}); i >= 0 {
			// The pass-2 retry reclaimed what pass 1 could not: replace the
			// stale failure line — the registry no longer holds the entry.
			if !strings.Contains(line, " (reclaim failed: ") {
				leaked[i] = line
			}
			continue
		}
		leaked = append(leaked, line)
	}

	if len(leaked) > 0 {
		fmt.Fprintf(report, `
FAIL: %d worktree registration(s) appeared in %s while this package ran.

A run with `+"`worktree: auto`"+` (the IR default) used the checkout the tests
run in as its source repository, so git registered the run's worktree HERE
while the checkout itself lived under t.TempDir() — deleted on the test's
return, or never reached by that cleanup when the add outlived the test
(#2901). Give the run a workspace the test owns —
internal/gittest.SourceRepo(t) passed as runtime.WithWorkDir /
runview.WithWorkDir — or, when the real checkout is genuinely the point,
unregister it with gittest.RemoveWorktree(t, repo, path).

The entries have been reclaimed by recorded path, so the repository is clean
again; the failure is the leak, not its residue.

Each line names the test that owns the path. That test is not necessarily in
THIS package: a process without this guard (an older revision, a guard-less
binary) still leaks into this shared registry, and the window is all of
m.Run(). Fix the test the path names.

Leaked (recorded path -> the test that owns it):
  %s
`, len(leaked), root, strings.Join(leaked, "\n  "))
		if code == 0 {
			code = 1
		}
	}
	return code
}

// tempRootPrefix names the per-process temp root isolateTempRoot creates.
const tempRootPrefix = "iterion-gittest-"

// isolateTempRoot points the process's temp dir at a directory it creates
// for this process alone, and returns the parent it was created under
// together with that directory. t.TempDir() resolves through os.TempDir, so
// every temp path the suite makes from here on lands under the returned
// root — which is what makes a registration pointing there this process's
// to judge. TMPDIR is what os.TempDir reads on unix; TMP and TEMP cover
// Windows. t.TempDir honours GOTMPDIR before TMPDIR (go/src/testing), so a
// GOTMPDIR inherited from the environment is redirected too — left alone,
// every temp dir would land outside the root and the guard would judge
// nothing.
//
// parent is the absolute inherited temp dir at creation time; removeTempRoot
// refuses any directory that is not a direct child of it carrying
// tempRootPrefix.
func isolateTempRoot() (parent, dir string, err error) {
	parent, err = filepath.Abs(os.TempDir())
	if err != nil {
		return "", "", err
	}
	dir, err = os.MkdirTemp(parent, tempRootPrefix)
	if err != nil {
		return "", "", err
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if err := os.Setenv(name, dir); err != nil {
			return "", "", err
		}
	}
	if os.Getenv("GOTMPDIR") != "" {
		if err := os.Setenv("GOTMPDIR", dir); err != nil {
			return "", "", err
		}
	}
	return parent, dir, nil
}

// removeTempRoot removes the per-process root at the end of the suite,
// retrying while a writer that outlived its test may still be adding to it
// (the #2781 shape). It refuses anything that is not exactly what
// isolateTempRoot returns — an absolute direct child of parent carrying
// tempRootPrefix, the same shape internal/hometest.removeHome refuses on —
// so a miscomputed root removes nothing and says so. A residue is warned
// about, not failed on: the leak verdict above is the guard's job, and a
// file a test left under the root by another path than t.TempDir() is not a
// registration.
func removeTempRoot(parent, dir string, report io.Writer) {
	if !filepath.IsAbs(dir) || filepath.Dir(dir) != filepath.Clean(parent) || !strings.HasPrefix(filepath.Base(dir), tempRootPrefix) {
		fmt.Fprintf(report, "gittest: refusing to remove %q: not a %s directory under %s\n", dir, tempRootPrefix, parent)
		return
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		if err = os.RemoveAll(dir); err == nil {
			return
		}
	}
	fmt.Fprintf(report, "gittest: the per-process temp root %s is left behind: %v\n", dir, err)
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

// removeAdminEntry removes one administrative entry directory. It is a
// variable so a test can inject a reclaim failure deterministically.
var removeAdminEntry = os.RemoveAll

// reclaimLeaked removes the administrative entries that appeared since
// `before` and are this process's to judge. Two zones, per the guard's
// doc comment: under processTmp (this process's own temp root) every entry
// is claimed, its directory alive or not; under inheritedTmp (the shared
// temp root the process was started with) only an entry whose directory no
// longer exists is claimed. Returns one human line per reclaimed entry. An
// empty processTmp degrades to the shared-root rule alone.
//
// inheritedTmp is a parameter rather than os.TempDir() read here: the guard
// calls this AFTER redirecting the process's temp dir, when os.TempDir()
// would answer the process root and the shared root would never be judged.
func reclaimLeaked(root string, before map[string]bool, processTmp, inheritedTmp string) []string {
	shared := resolveTempPrefix(inheritedTmp)
	// The process root matches under BOTH its forms: the exact string
	// isolateTempRoot set (what t.TempDir() hands to the run) and the
	// symlink-resolved one — git records the RESOLVED path in gitdir, and
	// on macOS the two differ at the very first segment (/var →
	// /private/var).
	var own []string
	if processTmp != "" {
		own = append(own, filepath.Clean(processTmp)+string(filepath.Separator))
		if resolved := resolveTempPrefix(processTmp); resolved != own[0] {
			own = append(own, resolved)
		}
	}

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
		var remainder string
		if prefix := matchingPrefix(wt, own); prefix != "" {
			// Under this process's own root: ours to judge, alive or not —
			// every test has returned, so a live directory there is the
			// registration t.TempDir()'s cleanup could not reach.
			remainder = strings.TrimPrefix(wt, prefix)
		} else if !strings.HasPrefix(wt+string(filepath.Separator), shared) {
			// Outside the test temp roots: not ours to judge, let alone
			// remove.
			continue
		} else {
			if _, err := os.Stat(wt); err == nil {
				// Still on disk — a sibling process's live worktree, or one
				// its own guard is about to judge. Absence is what makes a
				// shared-root entry dead.
				continue
			}
			remainder = strings.TrimPrefix(wt, shared)
		}
		if err := removeAdminEntry(admin); err != nil {
			leaked = append(leaked, fmt.Sprintf("%s -> %s (reclaim failed: %v)", wt, ownerTest(remainder), err))
			continue
		}
		leaked = append(leaked, fmt.Sprintf("%s -> %s", wt, ownerTest(remainder)))
	}
	sort.Strings(leaked)
	return leaked
}

// matchingPrefix returns the first prefix of prefixes that contains dir —
// compared on separator boundaries, so a sibling's root sharing the name
// stem (iterion-gittest-<other-pid>) never matches.
func matchingPrefix(dir string, prefixes []string) string {
	for _, p := range prefixes {
		if strings.HasPrefix(dir+string(filepath.Separator), p) {
			return p
		}
	}
	return ""
}

// resolveTempPrefix returns dir with symlinks evaluated, cleaned, and
// terminated by a separator — the form a prefix comparison against a
// recorded path needs. Falls back to the unresolved dir.
func resolveTempPrefix(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return filepath.Clean(dir) + string(filepath.Separator)
}

// ownerTest names the test that owns a leaked path, given the path's
// remainder under the temp root it was judged against. t.TempDir() roots at
// <TestName><random>/<n>, so the first element of the remainder carries the
// test's name — the only breadcrumb left once the directory is gone.
func ownerTest(remainder string) string {
	first, _, _ := strings.Cut(filepath.ToSlash(remainder), "/")
	name := strings.TrimRight(first, "0123456789")
	if name == "" {
		return "unknown test"
	}
	return name
}
