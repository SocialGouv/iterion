package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every Manager outside this package has to say which servers its launcher
// may start, because the zero value refuses the ones the operator did not
// install — the safe answer, and a silent behaviour change for a harness that
// meant "start everything".
//
// This guard exists because of WHERE such harnesses live: two of them are
// `//go:build live` e2e tests that cost real LLM money and that no CI job
// runs. A missing start policy there compiles, passes review, and is
// discovered months later by whoever next pays for a live run. The sweep
// parses every Go file in the repository, build tags included, so the live
// tags are covered by the free layer.
//
// An entry in the allowlist is a decision on the record, not an exemption.
func TestEveryManagerOutsideThisPackageDeclaresItsStartPolicy(t *testing.T) {
	exempt := map[string]string{
		// The one production constructor: it computes the policy from the
		// run's predicted sandbox and passes it positionally.
		"pkg/runview/executor.go": "buildMCPManager passes the predicted policy as an argument",
		// Exercises the zero value on purpose: that an UNARMED manager is
		// what SetSandbox(nil) has to open is the property under test.
		"pkg/backend/model/executor_mcp_start_policy_test.go": "asserts the zero value, then that the engine opens it",
	}

	repoRoot := filepath.Join("..", "..", "..")
	var offenders []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", ".iterion", "studio":
				return filepath.SkipDir
			}
			// A nested checkout (a git worktree, a sibling clone an operator
			// keeps on disk) holds another tree's files: none are tracked
			// here, and its older copies would report as offenders of a rule
			// they predate.
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
		rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator)))
		// This package's own tests exercise the zero value on purpose.
		if strings.HasPrefix(rel, "pkg/backend/mcp/") {
			return nil
		}
		if _, ok := exempt[rel]; ok {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		if managerBuiltWithoutAStartPolicy(file, mcpImportName(file)) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("these build an mcp.Manager without declaring which servers the launcher may start, so they "+
			"inherit the zero value (operator-installed servers only) — which silently refuses a "+
			"workflow-declared server: %v\nPass mcp.WithStartPolicy(…) (StartAllServers for a harness that "+
			"models an unsandboxed run), or add the file to this test's exempt map with a reason.", offenders)
	}
}

// managerBuiltWithoutAStartPolicy reports whether the file calls NewManager
// without a WithStartPolicy argument. It reads the SOURCE rather than
// scanning text: `NewManager(` inside a comment or a string satisfies a grep,
// and a call split across lines defeats a line-oriented one.
func managerBuiltWithoutAStartPolicy(file *ast.File, mcpPkg string) bool {
	if mcpPkg == "" {
		return false // the file does not import this package at all
	}
	missing := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isMCPNewManagerCall(call.Fun, mcpPkg) {
			return true
		}
		if len(call.Args) == 0 {
			return true // not this constructor after all
		}
		// Args[0] is the catalog; the options are the variadic tail. Judging
		// Args[0] was the first version's bug: it is an identifier at every
		// real call site, and the "an identifier could be a pre-built option,
		// give up" rule below then fired on EVERY call — a guard that matched
		// nothing and read exactly like one that worked.
		for _, arg := range call.Args[1:] {
			switch a := arg.(type) {
			case *ast.CallExpr:
				// The option may come from a helper, so any callee named
				// WithStartPolicy counts, qualified or not.
				if calleeName(a.Fun) == "WithStartPolicy" {
					return true
				}
			case *ast.Ident, *ast.SelectorExpr:
				// A pre-built option value or a spread (`opts...`): the file
				// assembled its options elsewhere and this walk cannot follow
				// it. Give it up rather than guess — in a gate a false
				// accusation costs as much as a miss.
				return true
			}
		}
		missing = true
		return true
	})
	return missing
}

// isMCPNewManagerCall matches `<mcpPkg>.NewManager(…)`, where mcpPkg is the
// name THIS FILE imports pkg/backend/mcp under.
//
// Matching a bare `NewManager` ident was the first version and it was wrong:
// pkg/alert, pkg/dispatcher and pkg/runview each have a NewManager of their
// own, and the guard accused nine innocent files. Reading the file's imports
// also catches an aliased import, which a hardcoded "mcp" would miss.
func isMCPNewManagerCall(fun ast.Expr, mcpPkg string) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewManager" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == mcpPkg
}

// mcpImportName returns the local name under which the file imports
// pkg/backend/mcp, or "" when it does not import it.
func mcpImportName(file *ast.File) string {
	const path = `"github.com/SocialGouv/iterion/pkg/backend/mcp"`
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "mcp"
	}
	return ""
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
