// Package sweepwalk is the one walk every repository-wide AST sweep reads:
// the skip list and the nested-checkout rule below, and one visit per Go
// file they reach. The runview ExecutorSpec sweep, the MCP start-policy
// sweep and the auxllm registry-channel sweep each carried a verbatim copy
// of these rules — three lists required to agree, and no reader able to
// tell which one was right once they didn't. Drift here is a paid class: a
// walk that read operator-local junk reddened `task check` on a clean main
// (#2898, the fact internal/treeskip was extracted for).
//
// The set is the SWEEP set, deliberately narrower than treeskip's: a sweep
// judges this repository's compilable source, so it prunes by name only the
// trees whose .go files cannot be that, and hands operator-parked checkouts
// to the nested-checkout rule instead of naming their homes — where someone
// parks their worktrees and clones is their business, not a test's.
// treeskip stays the generators' set; a name belongs to one of the two
// packages when its reason does, and to both only by carrying both reasons.
//
// Every .go file the rules reach reaches visit, test files included:
// whether a sweep judges test files is its own predicate's policy. The MCP
// sweep parses tests because two of the sites it guards are `live` e2e
// tests no CI job runs; the auxllm sweep skips them as fixtures.
package sweepwalk

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skips is the set itself. Unexported: a caller that could range over a
// mutable map could also write to it, and the whole point is one answer.
var skips = map[string]bool{
	// Third-party or foreign-frontend code: no sweep judges another tree's
	// sources.
	"vendor":       true,
	"node_modules": true,
	"studio":       true,
	// Git's own metadata.
	".git": true,
	// The engine's own run scratch.
	".iterion": true,
	// Deliberately broken parser fixtures, skipped for the reason the
	// toolchain skips them: a .go file there is never compiled, so judging
	// it would accuse source that cannot run.
	"testdata": true,
}

// Skip reports whether a directory of this name holds nothing a sweep
// judges. It is the question the walk asks about each directory it stands
// in, and the answer is `filepath.SkipDir`.
func Skip(name string) bool { return skips[name] }

// GoFiles walks repoRoot and calls visit for every Go file the rules reach,
// with its repository-relative slash-separated path and the path as the walk
// reached it. repoRoot is the root exactly as the caller reaches it — for a
// test living in pkg/a/b, filepath.Join("..", "..").
//
// A walk error is fatal: the tree the sweep is required to judge went
// unread, and judging part of it in silence is the defect this package
// exists to prevent.
func GoFiles(t *testing.T, repoRoot string, visit func(rel, path string)) {
	t.Helper()
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if Skip(d.Name()) {
				return filepath.SkipDir
			}
			// A directory carrying its own .git is a NESTED CHECKOUT — a git
			// worktree or a sibling clone an operator keeps on disk. Its files
			// belong to another tree (none are tracked here), and its older
			// copies would report as offenders of a rule they predate.
			// Detected by that marker rather than by directory name. The root
			// itself is exempt: the repository's own .git sits directly in it.
			if path != repoRoot {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		visit(filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator))), path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
}

// ModuleRoot resolves the module root of the tree the test PROCESS runs in:
// go test executes each package's tests with the working directory set to
// that package's directory inside the tree under test, so the root is a
// fixed walk upward from there — never runtime.Caller, whose absolute paths
// are recorded at compile time and reused across checkouts by the shared
// build cache (a cached package compiled elsewhere walks the other tree).
func ModuleRoot(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", wd)
		}
		dir = parent
	}
}
