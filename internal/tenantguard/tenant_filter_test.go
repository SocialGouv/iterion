// Package tenantguard holds the gate that keeps the tenant clause of a
// Mongo query from going optional. A store that scopes by tenant has one
// helper building the clause; the question this gate answers is what that
// helper does when the ctx carries no tenant.
//
// Two answers are safe and both are readable from the source: refuse
// (return an error, as the api-key, binding, generic-secret, bot-source,
// plugin-source and run-secrets helpers do) or stop the process (panic,
// as pkg/store/mongo's does). The unsafe answer — return the filter
// unscoped — reads identically to the safe ones at the call site, which
// is why it needs a gate rather than a reviewer: pkg/secrets'
// run_secrets helper carried it for a release while its own doc comment
// claimed it mirrored the panicking one.
//
// Limits, stated. This is a SHAPE gate over the source: it proves the
// helper cannot return a filter without also returning an error (or
// without panicking), not that the caller propagates that error — the
// per-store behaviour tests do that, and pkg/secrets'
// run_secrets_tenant_scope_test.go is the worked example. It reads the
// `bson.M` map-assignment form, which every tenant-scoped filter of the
// tree uses today (the 50 other `Key: "tenant_id"` sites are index key
// specifications, carrying `Value: 1`, not filters); a filter written as
// a `bson.D` element would sit outside it.
package tenantguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// familyName matches the helpers whose single job is to add a tenant_id
// clause derived from the context.
var familyName = regexp.MustCompile(`^with[A-Za-z0-9_]*TenantFilter$`)

// tenantClauseKey is the filter key the family writes.
const tenantClauseKey = `"tenant_id"`

// minFamilySize floors the walk: a broken enumeration must redden
// instead of passing on an empty set. Measured at 7 (2026-10-29).
const minFamilySize = 7

// conditionalByDesign lists the sites that write a tenant_id clause
// WITHOUT being a family helper, each with the reason the conditional is
// not a ctx pass-through. A site absent from both the family and this
// table is a new optional tenant clause and reddens.
//
// The three runstream entries are the honest residual: they do read the
// clause off the ctx and skip it when absent. They are reached by run id
// behind the run-view's own ownership resolution, and nobody has proved
// the tenant-less shape safe — they are named here so the next helper
// written by copy-paste has to look at them, not so they are blessed.
var conditionalByDesign = map[string]string{
	"pkg/runwatch/mongo.go":               "the tenant comes from the episode record, not the ctx; the conditional handles the omitempty empty-tenant shape of a local watch",
	"pkg/secrets/byok.go":                 "MarkFingerprintUsed deliberately escapes the run's tenant filter: a lent key's row lives in the donor's tenant (documented at pkg/secrets/run_secrets.go's lending comment)",
	"pkg/store/mongo/memory.go":           "spaceDocFilter takes the tenant from the SpaceRef, not the ctx; the global space's documents carry none",
	"pkg/store/mongo/retry_circuit.go":    "writes the field on upsert ($set), it is not a read filter",
	"pkg/runview/runstream/mongo.go":      "RESIDUAL, not a decision: backfillEvents scopes by run_id and adds the ctx tenant when present",
	"pkg/runview/runstream/mongo_logs.go": "RESIDUAL, not a decision: backfillLogs and runIsTerminal scope by run id and add the ctx tenant when present",
}

// repoRoot climbs to the module root so the walk is independent of where
// `go test` was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		t.Fatal("go env GOMOD is empty: the gate cannot locate the module root")
	}
	return filepath.Dir(gomod)
}

// goFiles lists the first-party, non-test Go files of the tree.
func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", "testdata", ".works", ".claude", ".iterion":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

type helper struct {
	rel        string
	line       int
	name       string
	returnsErr bool
	panics     bool
}

// familyHelpers parses the tree and returns every function whose name
// says it builds a tenant filter, with the two properties that make a
// silent unscoped return impossible.
func familyHelpers(t *testing.T, root string) []helper {
	t.Helper()
	fset := token.NewFileSet()
	var found []helper
	for _, path := range goFiles(t, root) {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("rel %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil || !familyName.MatchString(fn.Name.Name) {
				continue
			}
			h := helper{rel: filepath.ToSlash(rel), line: fset.Position(fn.Pos()).Line, name: fn.Name.Name}
			if fn.Type.Results != nil {
				for _, res := range fn.Type.Results.List {
					if id, ok := res.Type.(*ast.Ident); ok && id.Name == "error" {
						h.returnsErr = true
					}
				}
			}
			if fn.Body != nil {
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "panic" {
						h.panics = true
					}
					return true
				})
			}
			found = append(found, h)
		}
	}
	return found
}

// TestEveryTenantFilterHelperFailsClosed is the gate. A helper that can
// hand back a filter without a tenant clause and without saying so is
// named, with its file and line.
func TestEveryTenantFilterHelperFailsClosed(t *testing.T) {
	root := repoRoot(t)
	family := familyHelpers(t, root)
	if len(family) < minFamilySize {
		t.Fatalf("found %d tenant-filter helpers, want at least %d: the walk is blind, not the tree clean", len(family), minFamilySize)
	}
	for _, h := range family {
		if !h.returnsErr && !h.panics {
			t.Errorf("%s:%d %s returns a filter with no error result and no panic: a tenant-less ctx is served unscoped", h.rel, h.line, h.name)
		}
	}
}

// TestEveryTenantClauseSiteIsAFamilyHelperOrDeclared is the other
// direction: a new optional tenant clause written inline, outside the
// family, must declare why. Without this half the family gate is easy to
// walk around — write the clause in the query method instead of a helper.
func TestEveryTenantClauseSiteIsAFamilyHelperOrDeclared(t *testing.T) {
	root := repoRoot(t)

	familyFiles := map[string]bool{}
	for _, h := range familyHelpers(t, root) {
		familyFiles[h.rel] = true
	}

	fset := token.NewFileSet()
	clauseFiles := map[string]bool{}
	for _, path := range goFiles(t, root) {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("rel %s: %v", path, err)
		}
		rel = filepath.ToSlash(rel)
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				idx, ok := lhs.(*ast.IndexExpr)
				if !ok {
					continue
				}
				lit, ok := idx.Index.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || lit.Value != tenantClauseKey {
					continue
				}
				clauseFiles[rel] = true
			}
			return true
		})
	}

	if len(clauseFiles) == 0 {
		t.Fatal("no tenant_id clause site found at all: the scan is broken")
	}

	var undeclared []string
	for rel := range clauseFiles {
		if familyFiles[rel] {
			continue
		}
		if _, ok := conditionalByDesign[rel]; ok {
			continue
		}
		undeclared = append(undeclared, rel)
	}
	sort.Strings(undeclared)
	for _, rel := range undeclared {
		t.Errorf("%s writes a tenant_id clause outside the fail-closed helper family and is not declared in conditionalByDesign: add the helper, or declare the reason", rel)
	}

	// A declaration the tree no longer backs makes the table read wider
	// than the code it describes.
	var stale []string
	for rel := range conditionalByDesign {
		if !clauseFiles[rel] {
			stale = append(stale, rel)
		}
	}
	sort.Strings(stale)
	for _, rel := range stale {
		t.Errorf("conditionalByDesign declares %s, which writes no tenant_id clause any more: drop the entry", rel)
	}
}
