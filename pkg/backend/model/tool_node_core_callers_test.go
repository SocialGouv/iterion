package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// toolNodeCoreReaders is the INVENTORY of every function that runs a tool
// node's recipe through [ClawExecutor.runToolNodeCore], each with the family
// its verdict belongs to — keyed by file:function, so a reader is read where
// it lives.
//
// The family is the whole point. runToolNodeCore reads the drain bound's cut
// ONCE, for all of them, as the exit-status verdict it is: the success and
// the answer stand and the abandoned drain is reported (#2855, ARB-118). That
// reading is correct only for a caller whose verdict IS the exit status. A
// caller whose answer is the BYTES — a `git` output, a version line, a report
// parsed for its fields — must not take it: it calls runWithSeparateStreams
// itself, where exec's sentinel comes back unwrapped, and fails on a cut like
// the rest of that family ([proc.DrainCutShort] holds both readings).
//
// Before this inventory the rule lived in a doc comment alone, and the round
// that found that had already paid for it once: the previous reading was
// placed inside the shared runner on a count of its callers taken by hand.
var toolNodeCoreReaders = map[string]string{
	"executor_tool.go:executeToolNodeCommon": "the recipe of a plain tool node: a non-zero exit status fails the node, which is the exit-status verdict itself",
	"executor_verified_action.go:runVerifiedRecipe": "rung 2 of the Verified Action ladder: the recipe's status is recorded and the postcondition is what judges it (ADR-044). " +
		"Two call sites, one per recipe shape (script, shell), one family",
	"executor_verified_action.go:runPostcondition": "the oracle of a Verified Action: `exit 0 = met` is ADR-044's single source of truth, so a check that exited 0 while a process it left held its output is MET",
	"executor_verified_action.go:selfRepair":       "the corrected command's re-run, judged by that same oracle",
}

// walkFloor is what a lost walk would hide. This package held 215
// deterministic test files and ~200 production files when the inventory was
// written (see parallel_campaign_test.go for the same reading), so a walk
// that comes back with a handful read nothing and its clean answer means
// nothing.
const walkFloor = 150

// TestEveryReaderOfRunToolNodeCoreNamesItsFamily pins the caller set of
// runToolNodeCore, so a fifth reader has to state which family its verdict
// belongs to instead of inheriting the exit-status reading in silence.
//
// The class is CLOSED by the language: runToolNodeCore is a method with a
// lowercase name, so nothing outside this package can call it — asserted
// below, because an exported twin would make this package's own files the
// wrong net. The call is found through the Go syntax tree, called or taken as
// a value, never by the spelling of a line; what it does not resolve is
// types, so a same-named method on another receiver would read as this one.
// That direction is a false positive, fails loudly, and is answered by a
// rename; the direction that matters is a call the walk does not see.
//
// Mutation seen red: add a fifth method that calls runToolNodeCore — the
// inventory names it and says what it owes.
func TestEveryReaderOfRunToolNodeCoreNamesItsFamily(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's own directory: %v", err)
	}
	visited := 0
	declared := 0
	exported := false
	seen := map[string]bool{}
	var offenders []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		src, rerr := os.ReadFile(e.Name()) // #nosec G304 -- this package's own files
		if rerr != nil {
			// One unreadable file costs one file of coverage, never the
			// whole inventory: aborting here would hide every offender
			// behind an unrelated read failure.
			t.Errorf("%s could not be read, so it was NOT inventoried: %v", e.Name(), rerr)
			continue
		}
		visited++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, e.Name(), src, 0)
		if perr != nil {
			t.Errorf("%s could not be parsed, so it was NOT inventoried: %v", e.Name(), perr)
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				// A reference from the package's own scope belongs to no
				// function, so no entry can describe its family.
				if referencesToolNodeCore(decl) {
					offenders = append(offenders, e.Name()+":<package scope>")
				}
				continue
			}
			if fn.Name.Name == runToolNodeCoreName && fn.Recv != nil {
				declared++
				exported = exported || ast.IsExported(fn.Name.Name)
				continue // its own body is the reading, not a reader
			}
			if !referencesToolNodeCore(fn.Body) {
				continue
			}
			key := e.Name() + ":" + fn.Name.Name
			seen[key] = true
			if _, listed := toolNodeCoreReaders[key]; !listed {
				offenders = append(offenders, key)
			}
		}
	}

	// The offender list is this test's product: reported FIRST, and every
	// check below it is an Errorf — an abort here would delete the answer.
	sort.Strings(offenders)
	for _, key := range offenders {
		t.Errorf("%s runs a tool node's recipe through runToolNodeCore and is not in toolNodeCoreReaders: that call takes the EXIT-STATUS reading of a cut drain — the success and the answer are kept and the abandoned drain is reported. Add the entry with the family your verdict belongs to; if your answer is the BYTES, call runWithSeparateStreams instead, where exec's sentinel comes back unwrapped, and fail on the cut", key)
	}
	var stale []string
	for key := range toolNodeCoreReaders {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		// The discriminator, not a remedy: each cause has a different one,
		// and the remedy for the wrong cause silences this test.
		t.Errorf("toolNodeCoreReaders names %s, which the walk did not see call runToolNodeCore: check whether that function still exists and still calls it. If it does, the walk lost the file — fix the walk. Only if the call is gone does the entry come out", key)
	}
	if declared != 1 {
		t.Errorf("the walk found %d declarations of a runToolNodeCore method, want exactly 1: the inventory above describes the readers of ONE reading", declared)
	}
	if exported {
		t.Error("runToolNodeCore is exported: this package's own files are then no longer the whole class of its callers, and this inventory would certify a net it does not have")
	}
	if visited < walkFloor {
		t.Errorf("the inventory read %d Go files of this package, fewer than the %d floor: what it did not read reports no caller", visited, walkFloor)
	}
}

// runToolNodeCoreName is the method whose caller set is pinned. Named once:
// the detector, the declaration check and the fixtures below must all mean
// the same method.
const runToolNodeCoreName = "runToolNodeCore"

// referencesToolNodeCore reports whether a syntax tree calls runToolNodeCore
// or takes it as a value, under any receiver expression.
func referencesToolNodeCore(n ast.Node) bool {
	if n == nil {
		return false
	}
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == runToolNodeCoreName {
			found = true
			return false
		}
		return true
	})
	return found
}

// The detector finds the call however it is spelled, and only this method.
func TestTheToolNodeCoreDetectorReadsTheTreeNotTheLine(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"plain":         "package x\nfunc f(e *E) { e.runToolNodeCore(nil) }\n",
		"value":         "package x\nfunc f(e *E) { c := e.runToolNodeCore; _ = c }\n",
		"in a closure":  "package x\nfunc f(e *E) { go func() { e.runToolNodeCore(nil) }() }\n",
		"deep receiver": "package x\nfunc f(s S) { s.ex.runToolNodeCore(nil) }\n",
		"in a defer":    "package x\nfunc f(e *E) { defer e.runToolNodeCore(nil) }\n",
	} {
		if !referencesToolNodeCore(parseFixture(t, src)) {
			t.Errorf("%s: the detector missed the call", name)
		}
	}
	for name, src := range map[string]string{
		"prose only":       "package x\n// e.runToolNodeCore( is prose here\nfunc f() {}\n",
		"a longer name":    "package x\nfunc f(e *E) { e.runToolNodeCoreTwice(nil) }\n",
		"a shorter name":   "package x\nfunc f(e *E) { e.runToolNode(nil) }\n",
		"the runner below": "package x\nfunc f(c *C) { runWithSeparateStreams(c) }\n",
		"a string":         "package x\nvar s = \"runToolNodeCore\"\n",
	} {
		if referencesToolNodeCore(parseFixture(t, src)) {
			t.Errorf("%s: the detector read a call that is not there", name)
		}
	}
}

func parseFixture(t *testing.T, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return f
}
