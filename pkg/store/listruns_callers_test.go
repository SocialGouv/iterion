package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// listRunsCallers is the ledger of the files allowed to call RunStore.ListRuns,
// each with the reason its whole-store walk is acceptable. ListRuns is a FULL
// SCAN on every backend (a collection scan with an in-memory sort on Mongo),
// so a caller is either operator-driven, one-off, or on a request path whose
// cost an operator sees — never a periodic worker: two of those re-read ten
// thousand run documents on ten replicas every twenty seconds in production,
// pinning the autoscaler at its maximum and the Mongo primary at six CPU
// (#2636). A worker asks the store the question it has (ListRunsBySourceIssue,
// ListDeliverableWatchers, …) and the store answers from an index.
//
// A new caller fails this test until it is listed here WITH its reason. The
// store's own packages (definition, conformance, the filesystem helpers that
// implement the indexed questions by scanning) are listed like everyone else.
var listRunsCallers = map[string]string{
	"cmd/iterion/migrate.go":             "operator-driven one-off: copies every run from one backend to the other",
	"pkg/cli/reliability.go":             "operator-driven one-off: the reliability pilot's baseline over the cloud run store (`iterion reliability report --cloud`), taken once before a promotion and once after",
	"pkg/cli/bench.go":                   "operator-driven benchmark over the whole store",
	"pkg/cli/inspect.go":                 "operator-driven inspection of a local store",
	"pkg/operatormcp/tools_local.go":     "operator tool listing a local store on request",
	"pkg/runops/runops.go":               "operator-driven run operations over a local store",
	"pkg/runview/pipeline_scheduler.go":  "boot-time rebuild of the pipeline queue, once per process lifetime",
	"pkg/runview/service_lifecycle.go":   "orphan reconcile, which lists only with lock authority (a local store); the lock-less cloud server returns before the listing",
	"pkg/runview/service_runs.go":        "the runs listing API: a request path whose filters apply after the load (#1351 made the loads projected)",
	"pkg/runview/admin_list.go":          "the super-admin cross-org runs view (#2148): a request path an operator triggers, behind requireSuperAdmin and the audited cross-org predicate — the same projected-load walk the per-team listing runs, paginated after the sort; never a periodic worker",
	"pkg/server/pipeline_admission.go":   "fork index memoized for a TTL and rebuilt only while a ticket is stuck, with projected loads",
	"pkg/server/run_delegation.go":       "request path: a delegation launch looks for its prior attempts",
	"pkg/store/store_run.go":             "the filesystem store's scan-based indexed questions (filterRunsSorted) — one process over a local tree, where the scan is the index",
	"pkg/store/storetest/conformance.go": "the cross-backend conformance suite exercising the contract",
}

// TestListRunsCallersAreOnTheLedger sweeps every non-test Go file of the
// repository for a call to a method named ListRuns and refuses the ones the
// ledger does not name. Judged on the call, not on a grep: a comment naming
// the method is not a caller.
func TestListRunsCallersAreOnTheLedger(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	callers := map[string]bool{}
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", "testdata":
				return filepath.SkipDir
			case "studio", ".iterion":
				// The Studio tree and the engine's run state live at the
				// root; a package named studio deeper down is Go code like
				// any other and is swept.
				if filepath.Dir(path) == repoRoot {
					return filepath.SkipDir
				}
			}
			// A directory carrying its own .git is a nested checkout (a
			// worktree, a sibling clone): its files belong to another tree.
			if path != repoRoot {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator)))
		if callsListRuns(t, path) {
			callers[rel] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) == 0 {
		t.Fatal("the sweep found no ListRuns caller at all — a sweep that sees nothing passes over anything")
	}

	var offenders, stale []string
	for rel := range callers {
		if _, ok := listRunsCallers[rel]; !ok {
			offenders = append(offenders, rel)
		}
	}
	for rel := range listRunsCallers {
		if !callers[rel] {
			stale = append(stale, rel)
		}
	}
	sort.Strings(offenders)
	sort.Strings(stale)
	for _, rel := range offenders {
		t.Errorf("%s calls ListRuns and is not on the ledger — ListRuns walks the whole store; a worker asks the store its question (ListRunsBySourceIssue, ListDeliverableWatchers, …), and a one-off or request path is listed in listRunsCallers with its reason (#2636)", rel)
	}
	for _, rel := range stale {
		t.Errorf("%s is on the ledger but no longer calls ListRuns — drop the entry, so the ledger says only what is true", rel)
	}
}

// callsListRuns reports whether the file reaches a ListRuns through a
// selector — `<expr>.ListRuns(...)`, or `<expr>.ListRuns` taken as a method
// value and called elsewhere (`list := rs.ListRuns; list(ctx)`,
// `poll(ctx, rs.ListRuns)`). A method NAMED ListRuns (its declaration) and
// a mention in a comment or a string are not reaches.
func callsListRuns(t *testing.T, path string) bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "ListRuns" {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestListRunsDetectorSeesEveryReach pins the detector on the shapes a caller
// can take, so the ledger cannot be slipped past by a method value, and does
// not accuse a declaration or a comment.
func TestListRunsDetectorSeesEveryReach(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"direct call", "package p\nfunc f(rs interface{ ListRuns() }) { rs.ListRuns() }\n", true},
		{"method value", "package p\nfunc f(rs interface{ ListRuns() }) { list := rs.ListRuns; list() }\n", true},
		{"method value as argument", "package p\nfunc g(fn func()) {}\nfunc f(rs interface{ ListRuns() }) { g(rs.ListRuns) }\n", true},
		{"declaration only", "package p\ntype s struct{}\nfunc (s) ListRuns() {}\n", false},
		{"comment and string only", "package p\n// ListRuns is a full scan.\nvar _ = \"rs.ListRuns(ctx)\"\n", false},
		{"other method", "package p\nfunc f(rs interface{ ListRunsBySchedule() }) { rs.ListRunsBySchedule() }\n", false},
	}
	for _, c := range cases {
		if got := callsListRuns(t, write(strings.ReplaceAll(c.name, " ", "_")+".go", c.src)); got != c.want {
			t.Errorf("%s: detected = %v, want %v", c.name, got, c.want)
		}
	}
}
