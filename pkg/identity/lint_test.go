package identity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lintProductionCallers lists, relative to root, every non-test Go file
// outside pkg/identity whose body calls `.<method>(` — the whole-row writers
// this package replaced with a patch — skipping the paths named in allowed.
//
// The walk covers the TRACKED Go tree only. vendor/ and node_modules/ hold
// third-party source, and a dot-directory holds gitignored tooling or a
// sibling checkout — .git, .repos (this project's convention for upstream
// clones), .works, .claude, .iterion, .devbox — which filepath.WalkDir would
// otherwise read, since it cannot honour a .gitignore. No tracked Go file
// lives under a dot-directory, so nothing project-owned escapes the lint.
func lintProductionCallers(root, method string, allowed map[string]string) ([]string, error) {
	needle := "." + method + "("
	prefix := filepath.ToSlash(root) + "/"
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The root is named with the same syntax as its children
			// ("../.." has a dot-prefixed base), so it is decided first:
			// skipping it would make the lint walk nothing and pass.
			if path == root {
				return nil
			}
			name := filepath.Base(path)
			if name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		slashed := strings.TrimPrefix(filepath.ToSlash(path), prefix)
		if slashed == "pkg/identity" || strings.HasPrefix(slashed, "pkg/identity/") {
			return nil
		}
		if _, ok := allowed[slashed]; ok {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(body), needle) {
			hits = append(hits, slashed)
		}
		return nil
	})
	return hits, err
}

// assertNoProductionCallers fails the test for every caller
// lintProductionCallers finds in the repository this package is checked out
// in. Shared by the UpdateUser and UpdateTeam lints so the skip set, the
// pkg/identity exclusion and the allow-list lookup live once.
func assertNoProductionCallers(t *testing.T, method, remedy string, allowed map[string]string) {
	t.Helper()
	hits, err := lintProductionCallers("../..", method, allowed)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range hits {
		t.Errorf("%s calls %s — %s", path, method, remedy)
	}
}

// UpdateTeam replaces a team document WHOLE (ReplaceOneChecked). It is how
// a stale read silently erases a field its build predates — the sovereign
// runner-pool mapping among them (#2029 F2). Every production writer is a
// PatchTeam now; this lint keeps it that way: any new non-test caller of
// UpdateTeam outside this package must justify itself here, not in code.
// (The memory twin's aliasing hides the erase from value-based tests, and
// the mongo leg of the patch conformance only bites the patch path — the
// lint is the durable gate.)
func TestUpdateTeamHasNoProductionCallers(t *testing.T) {
	assertNoProductionCallers(t, "UpdateTeam",
		"a whole-document team replace outside pkg/identity (use PatchTeam; see the F2 hazard)", nil)
}

// TestLintProductionCallersReadsTheTrackedTreeOnly drives the walker over a
// fixture root, because the two lints above can only ever report what this
// machine's checkout happens to contain. The two directions that matter: a
// gitignored sibling clone must not be judged — the operator's own checkout
// holds thousands of third-party Go files under .repos/, and reporting one
// would redden the owed gate on code the project does not own — and every
// tracked package must still be, root included.
//
// The fixture root is DOT-NAMED on purpose: the real root is "../..", whose
// base is "..", so a walker applying its dot-directory rule to the root
// itself walks nothing and passes vacuously.
func TestLintProductionCallersReadsTheTrackedTreeOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".fixture-root")
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const caller = "package p\n\nfunc f(s S) { s.UpdateUser(nil) }\n"
	write("pkg/server/handler.go", caller)         // tracked: must be reported
	write("cmd/iterion/server.go", caller)         // tracked: must be reported
	write("pkg/server/handler_test.go", caller)    // a test: not production
	write("pkg/identity/mongo.go", caller)         // the implementation itself
	write("pkg/server/allowed.go", caller)         // named in `allowed`
	write(".repos/seaweedfs/weed/dash.go", caller) // a gitignored upstream clone
	write(".works/sibling/pkg/x.go", caller)       // a sibling worktree
	write("vendor/example.com/lib/lib.go", caller) // vendored third party
	write("node_modules/pkg/index.go", caller)

	got, err := lintProductionCallers(root, "UpdateUser",
		map[string]string{"pkg/server/allowed.go": "the fixture's allow-list entry"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"pkg/server/handler.go": true, "cmd/iterion/server.go": true}
	for _, path := range got {
		if !want[path] {
			t.Errorf("%s is not project source the lint may judge", path)
			continue
		}
		delete(want, path)
	}
	for path := range want {
		t.Errorf("%s is tracked project source and the lint did not report its UpdateUser call", path)
	}
}

// TestLintProductionCallersReportsTheReadError is the other half of the walk:
// an unreadable tree must not pass as "no callers found".
func TestLintProductionCallersReportsTheReadError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-tree")
	hits, err := lintProductionCallers(missing, "UpdateUser", nil)
	if err == nil {
		t.Fatalf("walking %s reported no error and %d hits", missing, len(hits))
	}
	if !strings.Contains(fmt.Sprint(err), "no-such-tree") {
		t.Errorf("error does not name the tree it could not walk: %v", err)
	}
}
