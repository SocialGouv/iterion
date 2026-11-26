package runtimeseams

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// mergeSite is what one site that folds a bundle's presets does with what the
// merge finds.
type mergeSite struct {
	logger bool   // the site hands the merge a logger
	why    string // where its findings go
}

// mergeSites is the inventory of every non-test site that folds a bundle's
// presets into a workflow, read in BOTH directions below: the walk must find
// each of these, and every site it finds must be one of these. A new launch
// surface then has to say where the merge's findings go instead of inheriting
// silence.
var mergeSites = map[string]mergeSite{
	"pkg/cli/run.go":                {logger: true, why: "the run's logger, on the command about to pay for the run"},
	"pkg/cli/validate.go":           {logger: false, why: "`validate` holds no logger: it reports the same finding as a bundlelint ERROR with the file and the remedy (checkPresets), which is stronger than a log line"},
	"pkg/runtime/engine_run.go":     {logger: true, why: "the engine's logger, on the backstop pass at run start"},
	"pkg/runview/service_launch.go": {logger: true, why: "the run's logger, on the studio/server launch"},
}

// mergeWalkFloor is what a collapsed walk would hide: the sweep reads the
// repository's compilable Go files (3 000+ at the time of writing), so a walk
// that comes back having read a handful read nothing and its clean answer
// means nothing. Set well under the count — the walk's job is to cover the
// files, not to pin how many there are.
const mergeWalkFloor = 1000

// TestEveryBundlePresetMergeSiteSaysWhereItsFindingsGo pins the caller set of
// MergeBundlePresets. The merge judges the selected preset's body as it folds
// it in, and it can only say so through the logger it is handed (its last
// argument): a site that passes nil is a launch path where the finding dies,
// so each site is on the record with what it does instead. Matched through the Go syntax tree — the call, never
// the spelling of a line; types are not resolved, so a same-named function on
// another package would read as this one (a false positive that fails loudly,
// answered by a rename).
func TestEveryBundlePresetMergeSiteSaysWhereItsFindingsGo(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	found := map[string]bool{}
	var silent, unexpected []string
	read := 0
	sweepwalk.GoFiles(t, sweepwalk.ModuleRoot(t), func(rel, path string) {
		read++
		// Test files pass nil on purpose — exercising the merge without a
		// channel is what some of them are for. Production sites owe the
		// answer.
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			// A file of this repository that does not parse is not a clean
			// verdict on it: say so rather than skipping in silence.
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || calleeName(call) != "MergeBundlePresets" {
				return true
			}
			found[rel] = true
			site, known := mergeSites[rel]
			if !known {
				unexpected = append(unexpected, rel)
				return true
			}
			if gotLogger := len(call.Args) >= 4 && !isNilIdent(call.Args[3]); gotLogger != site.logger {
				silent = append(silent, rel)
			}
			return true
		})
	})

	if read < mergeWalkFloor {
		t.Fatalf("the sweep read %d Go files, under the %d floor — the walk collapsed and its answer means nothing", read, mergeWalkFloor)
	}
	for rel := range mergeSites {
		if !found[rel] {
			t.Errorf("%s is on the record as folding a bundle's presets, and the walk found no call there — the site moved, or the walk no longer sees it", rel)
		}
	}
	sort.Strings(unexpected)
	for _, rel := range unexpected {
		t.Errorf("%s folds a bundle's presets and says nowhere where the merge's findings go — add it to mergeSites with the channel it hands the merge", rel)
	}
	sort.Strings(silent)
	for _, rel := range silent {
		t.Errorf("%s hands the merge a logger the record disagrees with (record: logger=%v, %s) — a nil logger drops every finding the merge makes about a preset body", rel, mergeSites[rel].logger, mergeSites[rel].why)
	}
}

// isNilIdent reports whether an argument is the untyped nil literal — the
// spelling a site uses to hand the merge no channel at all.
func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// calleeName is the name a call spells for its function: the selector of a
// qualified or method call, or the bare identifier.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.Ident:
		return fun.Name
	}
	return ""
}
