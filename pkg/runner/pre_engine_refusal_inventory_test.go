package runner

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestEveryPreEngineReturnHasARecorder(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			t.Fatal("repository root not found from cwd")
		}
		cwd = parent
	}
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, filepath.Join(cwd, "pkg/runner/loop.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == "executeRun" {
			fn = f
		}
	}
	if fn == nil {
		t.Fatal("executeRun absent")
	}
	callName := func(expr ast.Expr) string {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		receiver, ok := sel.X.(*ast.Ident)
		if !ok {
			return ""
		}
		return receiver.Name + "." + sel.Sel.Name
	}
	checkPreEnginePaths(t, fs, fn, callName)
	firstEngine := token.Pos(0)
	phases := map[string]int{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		if call, ok := node.(*ast.CallExpr); ok {
			name := callName(call)
			if name == "engine.Run" || name == "engine.ResumeWithHostInputs" {
				if firstEngine == 0 || call.Pos() < firstEngine {
					firstEngine = call.Pos()
				}
			}
			if name == "r.refuseBeforeEngine" {
				if len(call.Args) != 4 {
					t.Fatalf("recorder arguments at %s", fs.Position(call.Pos()))
				}
				phase, ok := call.Args[2].(*ast.BasicLit)
				if !ok {
					t.Fatal("phase must be an explicit inventory name")
				}
				value, err := strconv.Unquote(phase.Value)
				if err != nil {
					t.Fatal(err)
				}
				phases[value]++
			}
		}
		return true
	})
	if firstEngine == 0 {
		t.Fatal("no engine boundary")
	}
	for _, phase := range []string{"workflow", "budget", "credentials", "repository_pin", "workspace", "mcp", "executor", "bundle", "launch_inputs"} {
		if phases[phase] != 1 {
			t.Errorf("phase %s has %d recorder calls", phase, phases[phase])
		}
		delete(phases, phase)
	}
	if len(phases) != 0 {
		t.Errorf("new pre-engine phases require pipeline coverage: %v", phases)
	}
	unvisited := map[token.Pos]bool{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		if ret, ok := node.(*ast.ReturnStmt); ok && ret.Pos() < firstEngine {
			unvisited[ret.Pos()] = true
		}
		return true
	})
	direct, specialized := 0, map[string]int{}
	var walkBlock func(*ast.BlockStmt, string)
	walkBlock = func(block *ast.BlockStmt, guard string) {
		for i, stmt := range block.List {
			switch v := stmt.(type) {
			case *ast.ReturnStmt:
				if v.Pos() >= firstEngine {
					continue
				}
				delete(unvisited, v.Pos())
				if len(v.Results) == 1 && callName(v.Results[0]) == "r.refuseBeforeEngine" {
					direct++
					continue
				}
				recorded := ""
				if i > 0 {
					if prev, ok := block.List[i-1].(*ast.ExprStmt); ok {
						recorded = callName(prev.X)
					}
				}
				if recorded == "r.failUnloadableIR" {
					specialized[recorded]++
					continue
				}
				if guard == "r.admitAttempt" || guard == "r.guardEngineRequirement" {
					specialized[guard]++
					continue
				}
				t.Errorf("unrecorded pre-engine return at %s", fs.Position(v.Pos()))
			case *ast.IfStmt:
				nested := ""
				if assign, ok := v.Init.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 {
					nested = callName(assign.Rhs[0])
				}
				walkBlock(v.Body, nested)
				if other, ok := v.Else.(*ast.BlockStmt); ok {
					walkBlock(other, "")
				}
			case *ast.BlockStmt:
				walkBlock(v, guard)
			}
		}
	}
	walkBlock(fn.Body, "")
	for pos := range unvisited {
		t.Errorf("new control flow before engine needs recorder inventory: %s", fs.Position(pos))
	}
	if direct != 8 {
		t.Errorf("direct refusal exits=%d want8", direct)
	}
	for _, name := range []string{"r.failUnloadableIR", "r.admitAttempt", "r.guardEngineRequirement"} {
		if specialized[name] != 1 {
			t.Errorf("specialized %s=%d", name, specialized[name])
		}
	}
}

// checkPreEnginePaths tracks each branch independently: a textual earlier
// Engine call on the other branch is not this path's engine boundary.
type preEnginePathReporter interface {
	Helper()
	Errorf(string, ...any)
}

func checkPreEnginePaths(t preEnginePathReporter, fs *token.FileSet, fn *ast.FuncDecl, callName func(ast.Expr) string) {
	t.Helper()
	covers := func(n ast.Node) bool {
		// Only an immediate call is evidence of execution. A deferred call,
		// goroutine, closure body or short-circuited operand is not a boundary.
		var values []ast.Expr
		switch x := n.(type) {
		case *ast.ExprStmt:
			values = []ast.Expr{x.X}
		case *ast.AssignStmt:
			values = x.Rhs
		case *ast.ReturnStmt:
			values = x.Results
		}
		for _, value := range values {
			switch callName(value) {
			case "engine.Run", "engine.ResumeWithHostInputs", "r.refuseBeforeEngine", "r.failUnloadableIR":
				return true
			}
		}
		return false
	}
	var walk func([]ast.Stmt, bool) []bool
	walk = func(stmts []ast.Stmt, covered bool) []bool {
		states := []bool{covered}
		for _, stmt := range stmts {
			var next []bool
			for _, state := range states {
				switch s := stmt.(type) {
				case *ast.ReturnStmt:
					if !state && !covers(s) {
						t.Errorf("path reaches unrecorded pre-engine return at %s", fs.Position(s.Pos()))
					}
				case *ast.IfStmt:
					state = state || covers(s.Init)
					guarded := false
					if a, ok := s.Init.(*ast.AssignStmt); ok && len(a.Rhs) == 1 {
						n := callName(a.Rhs[0])
						guarded = n == "r.admitAttempt" || n == "r.guardEngineRequirement"
					}
					next = append(next, walk(s.Body.List, state || guarded)...)
					switch e := s.Else.(type) {
					case nil:
						next = append(next, state)
					case *ast.BlockStmt:
						next = append(next, walk(e.List, state)...)
					case *ast.IfStmt:
						next = append(next, walk([]ast.Stmt{e}, state)...)
					}
				case *ast.SwitchStmt:
					state = state || covers(s.Init)
					hasDefault := false
					for _, part := range s.Body.List {
						c := part.(*ast.CaseClause)
						hasDefault = hasDefault || c.List == nil
						next = append(next, walk(c.Body, state)...)
					}
					if !hasDefault {
						next = append(next, state)
					}
				case *ast.TypeSwitchStmt:
					state = state || covers(s.Init)
					hasDefault := false
					for _, part := range s.Body.List {
						c := part.(*ast.CaseClause)
						hasDefault = hasDefault || c.List == nil
						next = append(next, walk(c.Body, state)...)
					}
					if !hasDefault {
						next = append(next, state)
					}
				case *ast.BlockStmt:
					next = append(next, walk(s.List, state)...)
				case *ast.ForStmt:
					state = state || covers(s.Init)
					next = append(next, state)
					next = append(next, walk(s.Body.List, state)...)
				case *ast.RangeStmt:
					next = append(next, state)
					next = append(next, walk(s.Body.List, state)...)
				case *ast.SelectStmt:
					for _, part := range s.Body.List {
						c := part.(*ast.CommClause)
						next = append(next, walk(c.Body, state)...)
					}
				case *ast.LabeledStmt:
					next = append(next, walk([]ast.Stmt{s.Stmt}, state)...)
				case *ast.BranchStmt:
					// No current pre-engine loop/control transfer needs this. Fail closed
					// for a new transfer whose target this small checker cannot follow.
					if !state {
						t.Errorf("unclassified pre-engine transfer at %s", fs.Position(s.Pos()))
					}
				default:
					next = append(next, state || covers(stmt))
				}
			}
			// States express only the two frontier facts, never enumerate paths.
			states = nil
			seen := [2]bool{}
			for _, v := range next {
				i := 0
				if v {
					i = 1
				}
				if !seen[i] {
					states = append(states, v)
					seen[i] = true
				}
			}
		}
		return states
	}
	walk(fn.Body.List, false)
}

// The checker is deliberately local to executeRun, not a general Go CFG.
// These opposite paths prevent textual position or unexecuted calls from
// weakening its contract when the implementation grows another branch.
type preEnginePathFindings struct{ messages []string }

func (*preEnginePathFindings) Helper() {}
func (p *preEnginePathFindings) Errorf(format string, args ...any) {
	p.messages = append(p.messages, fmt.Sprintf(format, args...))
}
func TestPreEngineInventoryFollowsExecutedBranches(t *testing.T) {
	cases := []struct {
		name, body string
		wantBad    bool
	}{
		{"if_engine_init", "if err := engine.Run(); err != nil { return err }; return nil", false},
		{"switch_engine_init", "switch err := engine.Run(); { case err != nil: return err }; return nil", false},
		{"type_switch_engine_init", "switch err := engine.Run(); value := any(err).(type) { case error: return value }; return nil", false},
		{"for_engine_init", "for engine.Run(); false; {}; return nil", false},
		{"if_closure_init", "if f := func(){ engine.Run() }; bad { _ = f }; return err", true},
		{"switch_closure_init", "switch f := func(){ engine.Run() }; { case bad: _ = f }; return err", true},
		{"type_switch_closure_init", "switch f := func(){ engine.Run() }; value := any(f).(type) { case func(): _ = value }; return err", true},
		{"for_closure_init", "for f := func(){ engine.Run() }; false; { _ = f }; return err", true},
		{"short_circuited_init", "if ok := false && engine.Run(); ok {}; return err", true},
		{"late_launch", "if resume { engine.ResumeWithHostInputs(); return nil } else { if bad { return err }; engine.Run() }; return nil", true},
		{"after_resume", "if resume { engine.ResumeWithHostInputs(); if stop { return err } } else { engine.Run() }; return nil", false},
		{"both_branches", "if resume { engine.ResumeWithHostInputs() } else { engine.Run() }; return nil", false},
		{"missing_branch", "if resume { engine.ResumeWithHostInputs() }; return nil", true},
		{"recorded_refusal", "if bad { return r.refuseBeforeEngine() }; engine.Run(); return nil", false},
		{"switch_late_return", "switch mode { case 1: engine.Run(); default: return err }; return nil", true},
		{"deferred_engine", "defer engine.Run(); return err", true},
		{"goroutine_engine", "go engine.Run(); return err", true},
		{"closure_engine", "f := func(){ engine.Run() }; _ = f; return err", true},
		{"zero_iteration_loop", "for more { engine.Run() }; return err", true},
		{"unclassified_transfer", "goto exit; exit: return err", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := token.NewFileSet()
			f, err := parser.ParseFile(fs, "fixture.go", "package runner; func executeRun() error {"+tc.body+"}", 0)
			if err != nil {
				t.Fatal(err)
			}
			findings := &preEnginePathFindings{}
			checkPreEnginePaths(findings, fs, f.Decls[0].(*ast.FuncDecl), func(expr ast.Expr) string {
				c, ok := expr.(*ast.CallExpr)
				if !ok {
					return ""
				}
				sel, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return ""
				}
				receiver, ok := sel.X.(*ast.Ident)
				if !ok {
					return ""
				}
				return receiver.Name + "." + sel.Sel.Name
			})
			if (len(findings.messages) > 0) != tc.wantBad {
				t.Fatalf("findings=%v want refusal=%v", findings.messages, tc.wantBad)
			}
		})
	}
}
