package mongo

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Cloud-parity gate (deterministic, no Mongo needed): every per-run Mongo
// collection MUST be swept by DeleteRun, or a deleted run's data lingers
// until its TTL (or forever, for a TTL-less collection) — a silent leak
// and a broken DeleteRun contract. This exact gap shipped once (run_logs
// was omitted from the children list, caught only by an out-of-band audit
// — commit c1324a196). This test is the guard so it can't recur: add a
// new col* collection and forget the DeleteRun wiring → red CI, in the
// same PR, before review.
//
// The check is a static parse of this package's own source:
//   - every `colXxx = "name"` constant in a production Go file is a collection this
//     store owns;
//   - DeleteRun (runs.go) sweeps a `children` slice of `{"name", …}` rows;
//   - a per-run collection must appear in that slice, or in the verified
//     namespace-selected inbox/stream cleanup routes. Real Mongo lifecycle
//     qualifications prove these routes preserve other births and legacy rows.
//
// nonPerRunCollections are the only collections exempt: `runs` is the run
// document itself (DeleteRun removes it via a direct DeleteOne, not the
// children sweep). Anything else added here needs an explicit reason.
var nonPerRunCollections = map[string]bool{
	"runs": true, // the run doc; deleted by DeleteRun's own DeleteOne
	// MongoMemoryStore shares this package but its knowledge spaces/documents
	// and tenant usage ledger outlive individual runs (memory.go).
	"memory_spaces":       true,
	"memory_docs":         true,
	"memory_tenant_usage": true,
	// retry_circuits is keyed by tenant + workflow revision, not run_id;
	// deleting one run must not erase the shared breaker used by siblings.
	"retry_circuits": true,
	// run_seq is keyed by the compound _id {tenant_id, run_id} and carries no
	// top-level run_id: DeleteRun sweeps it with its own DeleteMany on that key
	// (runs.go), before the children loop, so the run_id sweep is not its path.
	"run_seq": true,
	// staged_uploads holds uploads no run has claimed yet (POST /api/runs/uploads
	// before the launch); the promotion discards the record and the reaper
	// sweeps the rest after the TTL — a run has nothing of its own in it.
	"staged_uploads": true,
	// cred_slots is keyed by credential FINGERPRINT and names runs inside its
	// holders array, tenant-blind: DeleteRun pulls the run from every row by
	// that key with its own UpdateMany (runs.go), so the run_id sweep is not
	// its path.
	"cred_slots": true,
}

func TestDeleteRunCoversEveryPerRunCollection(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, filename := range files {
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file := productionAST(t, filename)
		for _, declaration := range file.Decls {
			group, ok := declaration.(*ast.GenDecl)
			if !ok || group.Tok != token.CONST {
				continue
			}
			var values []ast.Expr
			for _, spec := range group.Specs {
				value := spec.(*ast.ValueSpec)
				if len(value.Values) != 0 {
					values = value.Values
				}
				for i, name := range value.Names {
					if !strings.HasPrefix(name.Name, "col") {
						continue
					}
					if i >= len(values) {
						t.Fatalf("collection %s has no initializer in %s", name.Name, filename)
					}
					literal, ok := values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						t.Fatalf("extend collection inventory for initializer of %s in %s", name.Name, filename)
					}
					name, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					declared[name] = true
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no col* collection constants found in production sources — parser drifted; fix this test")
	}

	deleteBody := productionFunction(t, "runs.go", "DeleteRun")
	selectorBody := productionFunction(t, "inbox_namespace.go", "inboxNamespace")
	// Enumerate the children initializer itself, never comments or unrelated
	// composites elsewhere in runs.go.
	swept := map[string]bool{}
	ast.Inspect(deleteBody, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 || syntax(t, assignment.Lhs[0]) != "children" {
			return true
		}
		list, ok := assignment.Rhs[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("DeleteRun children initializer changed; extend inventory")
		}
		for _, element := range list.Elts {
			row, ok := element.(*ast.CompositeLit)
			if !ok || len(row.Elts) != 2 {
				t.Fatal("DeleteRun child row changed; extend inventory")
			}
			name, err := strconv.Unquote(syntax(t, row.Elts[0]))
			if err != nil {
				t.Fatal(err)
			}
			swept[name] = true
		}
		return false
	})
	if len(swept) == 0 {
		t.Fatal("no DeleteRun children rows found — extend inventory")
	}

	// This structural guard checks the namespace route; real Mongo tests
	// prove its birth/tenant isolation. AST nodes omit comments and positions,
	// so equivalent source formatting cannot change the result.
	for _, expression := range []string{
		`s.inboxNamespace(ctx, id, original)`,
		`inbox.collection.DeleteMany(ctx, inbox.filter(ctx, bson.M{"run_id": id}))`,
	} {
		found := false
		ast.Inspect(deleteBody, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && syntax(t, call) == expression {
				found = true
			}
			return true
		})
		if !found {
			t.Fatalf("namespace-selected inbox cleanup route missing %q", expression)
		}
	}
	for _, expression := range []string{"s.userMessages", "s.db.Collection(colIncarnationUserMessages)"} {
		found := false
		ast.Inspect(selectorBody, func(node ast.Node) bool {
			ret, ok := node.(*ast.ReturnStmt)
			if !ok || len(ret.Results) == 0 {
				return true
			}
			value, ok := ret.Results[0].(*ast.CompositeLit)
			if !ok || syntax(t, value.Type) != "inboxDataNamespace" {
				return true
			}
			for _, element := range value.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if ok && syntax(t, field.Key) == "collection" && syntax(t, field.Value) == expression {
					found = true
				}
			}
			return true
		})
		if !found {
			t.Fatalf("inbox namespace selector no longer returns collection %s", expression)
		}
	}
	swept[colUserMessages] = true
	swept[colIncarnationUserMessages] = true

	for name := range verifiedStreamCleanup(t, deleteBody) {
		swept[name] = true
	}

	for name := range declared {
		if nonPerRunCollections[name] {
			continue
		}
		if !swept[name] {
			t.Errorf("collection %q is declared but NOT swept by DeleteRun (children slice or verified namespace route). "+
				"Add {%q, s.<field>} to the children list, or exempt it in nonPerRunCollections with a reason. "+
				"Without this, deleting a run leaks its %q data.", name, name, name)
		}
	}
}

// Positions and comments carry layout, not wiring. Print individual AST nodes
// with an empty FileSet to compare syntax independently of source line breaks.
func syntax(t *testing.T, node ast.Node) string {
	t.Helper()
	var out bytes.Buffer
	if err := printer.Fprint(&out, token.NewFileSet(), node); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func productionAST(t *testing.T, filename string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func productionFunction(t *testing.T, filename, name string) *ast.BlockStmt {
	t.Helper()
	for _, declaration := range productionAST(t, filename).Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn.Body
		}
	}
	t.Fatalf("function %s missing in %s", name, filename)
	return nil
}

// verifiedStreamCleanup recognizes only the selected stream route. This is a
// wiring check, not a control-flow or tenant-isolation proof: the real Mongo
// lifecycle tests remain responsible for those guarantees. No new collection
// is exempted just because it has the run_data_v1 prefix.
func verifiedStreamCleanup(t *testing.T, deletion *ast.BlockStmt) map[string]bool {
	t.Helper()
	hasCall := func(body ast.Node, expression string) bool {
		found := false
		ast.Inspect(body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && syntax(t, call) == expression {
				found = true
			}
			return true
		})
		return found
	}
	selectionAssigned := false
	ast.Inspect(deletion, func(node ast.Node) bool {
		a, ok := node.(*ast.AssignStmt)
		if ok && len(a.Lhs) == 2 && len(a.Rhs) == 1 && syntax(t, a.Lhs[0]) == "streamNS" && syntax(t, a.Lhs[1]) == "err" && syntax(t, a.Rhs[0]) == "s.streamNamespace(ctx, id, original)" {
			selectionAssigned = true
		}
		return true
	})
	if !selectionAssigned {
		t.Fatal("namespace-selected stream cleanup lost assignment of original selection")
	}
	selector := productionFunction(t, "stream_namespace.go", "streamNamespace")
	selected := map[string]bool{}
	ast.Inspect(selector, func(node ast.Node) bool {
		a, ok := node.(*ast.AssignStmt)
		if ok && len(a.Lhs) == 1 && len(a.Rhs) == 1 {
			selected[syntax(t, a.Lhs[0])+" = "+syntax(t, a.Rhs[0])] = true
		}
		return true
	})
	sweptFields := map[string]bool{}
	counter := false
	ast.Inspect(deletion, func(node ast.Node) bool {
		branch, ok := node.(*ast.IfStmt)
		if !ok || syntax(t, branch.Cond) != `streamNS.namespace != ""` {
			return true
		}
		counter = counter || hasCall(branch.Body, `streamNS.seq.DeleteMany(ctx, bson.M{"_id": streamNS.counterKey(ctx)})`)
		ast.Inspect(branch.Body, func(node ast.Node) bool {
			loop, ok := node.(*ast.RangeStmt)
			if !ok || loop.Value == nil || syntax(t, loop.Value) != "coll" {
				return true
			}
			list, ok := loop.X.(*ast.CompositeLit)
			if !ok || syntax(t, list.Type) != "[]*mongo.Collection" || !hasCall(loop.Body, "coll.DeleteMany(ctx, streamNS.filter(ctx, nil))") {
				return true
			}
			for _, field := range list.Elts {
				sweptFields[syntax(t, field)] = true
			}
			return false
		})
		return false
	})
	out := map[string]bool{}
	for _, row := range []struct{ field, constant, collection string }{
		{"events", "colIncarnationEvents", colIncarnationEvents},
		{"logs", "colIncarnationLogs", colIncarnationLogs},
		{"plans", "colIncarnationPlans", colIncarnationPlans},
		{"notes", "colIncarnationNotes", colIncarnationNotes},
		{"seq", "colIncarnationSeq", colIncarnationSeq},
	} {
		if !selected["n."+row.field+" = s.db.Collection("+row.constant+")"] {
			t.Errorf("stream selector no longer selects %s for %s", row.constant, row.field)
			continue
		}
		if row.field == "seq" && counter || row.field != "seq" && sweptFields["streamNS."+row.field] {
			out[row.collection] = true
		}
	}
	return out
}
