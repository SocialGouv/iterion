package runview

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// buildExecutorWithFallback drives the REAL BuildExecutor and reports what
// the run-level codex route materialised onto the workflow's one agent
// node. Anything short of this — asserting on ApplyRunFallback directly —
// leaves the wiring between the launch surface and the screen untested,
// which is exactly where the screen was inert.
func buildExecutorWithFallback(t *testing.T, wfSandbox *ir.SandboxSpec, sandboxDefault, sandboxOverride string) int {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	agent := &ir.AgentNode{}
	agent.ID = "work"
	agent.Backend = "claude_code"
	agent.Model = "claude-opus-5"
	wf := &ir.Workflow{
		Name:    "canary",
		Nodes:   map[string]ir.Node{"work": agent},
		Sandbox: wfSandbox,
	}
	spec := ExecutorSpec{
		Ctx:      context.Background(),
		Store:    st,
		RunID:    "run-1",
		Workflow: wf,
		RunFallback: []ir.Fallback{{
			Name: ir.RunFallbackName, Backend: "codex", Model: "gpt-5.4",
		}},
		SandboxDefault:  sandboxDefault,
		SandboxOverride: sandboxOverride,
	}
	if _, err := BuildExecutor(spec); err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}
	return len(wf.Nodes["work"].(*ir.AgentNode).Fallbacks)
}

// The screen only protects anything if the launch surface hands it the
// same sandbox knobs it hands the engine. It shipped reading a bare
// SandboxDefault that two of the four RunFallback surfaces never set, so
// on the shipped `sandbox: auto` default the codex stage sailed through a
// screen that had resolved "unsandboxed" — while the same process started
// a sandbox.
func TestBuildExecutor_codexStageScreenedAgainstTheEngineVerdict(t *testing.T) {
	cases := []struct {
		name       string
		wfSandbox  *ir.SandboxSpec
		def        string
		override   string
		wantStages int
	}{{
		// The shipped shape: no workflow block, sandbox-by-default.
		name: "sandbox-by-default refuses", def: "auto", wantStages: 0,
	}, {
		name: "workflow sandbox block refuses", wfSandbox: &ir.SandboxSpec{Mode: "auto"}, wantStages: 0,
	}, {
		// The cloud-runner shape (ITERION_SANDBOX_OVERRIDE=none): the pod
		// IS the boundary, task.Sandbox is nil, codex runs — so refusing
		// here would drop the operator's explicit --fallback.
		name:      "CLI-strength override none takes the stage",
		wfSandbox: &ir.SandboxSpec{Mode: "auto"}, def: "auto", override: "none", wantStages: 1,
	}, {
		name: "no sandbox anywhere takes the stage", wantStages: 1,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildExecutorWithFallback(t, tc.wfSandbox, tc.def, tc.override); got != tc.wantStages {
				t.Fatalf("materialised %d fallback stages, want %d", got, tc.wantStages)
			}
		})
	}
}

// A launch surface that hands BuildExecutor a RunFallback without the
// sandbox pair silently disarms the codex screen: WorkflowSandboxActive
// then resolves "unsandboxed" from empty knobs, so every codex stage is
// accepted on a host that will sandbox it. That is not a compile error
// and no test of the screen itself can see it — the two CLI surfaces
// shipped exactly this way, which is what this guard exists to catch.
//
// Gated on RunFallback on purpose: a spec that carries no run-level chain
// never reaches the screen, so the five such sites stay unflagged.
// Parses the source rather than scanning it, for the reasons spelled out
// on TestEveryExecutorConstructionDecidesTheBotIdentity.
func TestEveryRunFallbackConstructionDecidesTheSandbox(t *testing.T) {
	required := []string{"SandboxDefault", "SandboxOverride"}

	repoRoot := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", ".iterion", "studio", "testdata":
				return filepath.SkipDir
			}
			// A nested checkout (git worktree / sibling clone) belongs to
			// another tree; its older copies would report as offenders of a
			// rule they predate. Same marker-based detection as the bot-id sweep.
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
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		if missing := executorSpecFallbackMissesSandbox(file, required); len(missing) > 0 {
			offenders = append(offenders, rel+" (missing "+strings.Join(missing, ", ")+")")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("these hand BuildExecutor a run-level fallback chain without deciding whether the run is "+
			"sandboxed, so ir.ApplyRunFallback screens it as UNSANDBOXED and accepts a codex stage the "+
			"dispatch guard will hard-error on: %v\n"+
			"Set SandboxDefault and SandboxOverride to the same pair you hand runtime.WithSandboxDefault / "+
			"WithSandboxOverride — writing \"\" explicitly when the surface has no such tier.",
			offenders)
	}
}

// executorSpecFallbackMissesSandbox returns the required field names a
// file leaves unset on an ExecutorSpec that DOES set RunFallback, by
// either construction shape: a composite literal (including the elided
// form inside a container literal) or a variable assigned field by field.
func executorSpecFallbackMissesSandbox(file *ast.File, required []string) []string {
	var missing []string
	note := func(names map[string]bool) {
		for _, want := range required {
			if !names[want] {
				missing = append(missing, want)
			}
		}
	}

	// Shape 1 — `ExecutorSpec{…}`, including the elided form Go allows
	// inside a map/slice literal (`map[string]ExecutorSpec{"a": {…}}`),
	// where the inner literal carries no type of its own.
	elided := map[ast.Node]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		var elem ast.Expr
		switch t := lit.Type.(type) {
		case *ast.MapType:
			elem = t.Value
		case *ast.ArrayType:
			elem = t.Elt
		}
		if elem != nil && isExecutorSpecType(elem) {
			for _, el := range lit.Elts {
				inner := el
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					inner = kv.Value
				}
				if c, ok := inner.(*ast.CompositeLit); ok && c.Type == nil {
					elided[c] = true
				}
			}
		}
		return true
	})
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || (!isExecutorSpecType(lit.Type) && !elided[lit]) {
			return true
		}
		set := map[string]bool{}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok {
					set[id.Name] = true
				}
			}
		}
		if set["RunFallback"] {
			note(set)
		}
		return true
	})

	// Shape 2 — `var spec ExecutorSpec` / `new(ExecutorSpec)` then
	// `spec.Field = …`: the shape an ordinary refactor of an existing
	// site produces, which the literal search above cannot see.
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		declared := map[string]bool{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			switch v := m.(type) {
			case *ast.ValueSpec:
				if isExecutorSpecType(v.Type) {
					for _, name := range v.Names {
						declared[name.Name] = true
					}
				}
			case *ast.AssignStmt:
				for i, rhs := range v.Rhs {
					call, ok := rhs.(*ast.CallExpr)
					if !ok || len(call.Args) != 1 {
						continue
					}
					if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "new" {
						continue
					}
					if isExecutorSpecType(call.Args[0]) && i < len(v.Lhs) {
						if id, ok := v.Lhs[i].(*ast.Ident); ok {
							declared[id.Name] = true
						}
					}
				}
			}
			return true
		})
		if len(declared) == 0 {
			return true
		}
		// Only a spec this function itself hands to BuildExecutor is
		// judged: one filled by a helper through a pointer sets its fields
		// where this walk cannot see them, and a false accusation in a gate
		// costs as much as a miss.
		handed := map[string]bool{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok || !isBuildExecutorCall(call.Fun) {
				return true
			}
			for _, arg := range call.Args {
				if id, ok := arg.(*ast.Ident); ok {
					handed[id.Name] = true
				}
			}
			return true
		})
		assigned := map[string]map[string]bool{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			as, ok := m.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				if assigned[id.Name] == nil {
					assigned[id.Name] = map[string]bool{}
				}
				assigned[id.Name][sel.Sel.Name] = true
			}
			return true
		})
		for name := range declared {
			if handed[name] && assigned[name]["RunFallback"] {
				note(assigned[name])
			}
		}
		return true
	})
	return missing
}
