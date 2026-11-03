package runner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// The grant gate's placement, pinned in the source beside the behaviour
// witnesses (TestAWithheldCloneCredentialIsRefusedBeforeTheClone over https
// and ssh, TestAWithheldCloneCredentialIsAckedByTheDeliveryNotRedelivered
// through executeRun), for the shapes they cannot enumerate: the gate is a
// top-level statement of prepareRepoWorkspace — no branch can skip it — it
// precedes the clone (a refused run spends no clone and its token is never
// written), it judges the function's own ctx and wf, its verdict is the
// if's WHOLE condition (an `err != nil && <never>` leaves the gate dead code
// at its one site), and its body is a single return of THAT verdict, bare
// or wrapped with %w: a %v, errors.New(err.Error()) or a shadowing re-wrap
// keeps the words and loses the typed cause the delivery path reads with
// errors.As, so the refusal would be redelivered instead of acked.
func TestTheCloneCredentialGateRunsBeforeTheClone(t *testing.T) {
	const file = "loop_gitws.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var fn *ast.FuncDecl
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == "prepareRepoWorkspace" && fd.Body != nil {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatalf("%s declares no prepareRepoWorkspace: the clone moved, move this pin with it", file)
	}
	body := fn.Body

	cloned := 0
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && cloned == 0 && callsGitVerb(call, "clone") {
			cloned = fset.Position(call.Pos()).Line
		}
		return true
	})
	if cloned == 0 {
		t.Fatalf("prepareRepoWorkspace no longer runs `git clone` itself: move this pin to where the clone happens")
	}

	var gate *ast.IfStmt
	var before []ast.Stmt
	for i, st := range body.List {
		if ifStmt, ok := st.(*ast.IfStmt); ok && funcCalledIn(ifStmt.Init) == "refuseUngrantedCloneCredential" {
			gate, before = ifStmt, body.List[:i]
		}
	}
	if gate == nil {
		t.Fatalf("prepareRepoWorkspace clones (%s:%d) and its body holds no top-level `if err := refuseUngrantedCloneCredential(…); err != nil { return … err }`: a `sinks:` declaration that withholds `file` is served again, or judged only on some branch", file, cloned)
	}
	gated := fset.Position(gate.Pos()).Line
	if gated > cloned {
		t.Errorf("the grant gate (%s:%d) runs after the clone (%s:%d): a refused run spends a clone and writes its token before the verdict", file, gated, file, cloned)
	}
	if !definesErr(gate.Init) {
		t.Fatalf("the grant gate (%s:%d) does not declare its verdict as `err := refuseUngrantedCloneCredential(…)`", file, gated)
	}
	// The verdict is the `err` the if's Init declares: the condition is in
	// its scope with nothing in between, and a body that is one return
	// statement declares nothing that could shadow it.
	if !isErrNotNil(gate.Cond) {
		t.Errorf("the grant gate's verdict (%s:%d) is not the if's whole condition `err != nil`: a conjunct that never holds leaves the gate dead code at its only call site", file, gated)
	}
	if !returnsErrTyped(gate.Body) {
		t.Errorf("the grant gate's body (%s:%d) is not a single return of the verdict, bare or wrapped with %%w: the typed cause the delivery path reads with errors.As does not reach it", file, gated)
	}
	// The gate judges the run's own credentials and workflow: the
	// function's ctx and wf parameters, not a context without the run's
	// bundle nor a workflow other than the one the run executes.
	call := gate.Init.(*ast.AssignStmt).Rhs[0].(*ast.CallExpr)
	for i, param := range []string{"ctx", "wf"} {
		if i >= len(call.Args) || !isParam(call.Args[i], fn, param, before) {
			t.Errorf("the grant gate (%s:%d) does not pass prepareRepoWorkspace's own %s parameter as argument %d: it judges something other than the run", file, gated, param, i)
		}
	}
}

// definesErr reports whether init is `err := <call>`.
func definesErr(init ast.Stmt) bool {
	as, ok := init.(*ast.AssignStmt)
	if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
		return false
	}
	if _, ok := as.Rhs[0].(*ast.CallExpr); !ok {
		return false
	}
	return isErr(as.Lhs[0])
}

// isErr reports whether expr is the identifier `err`.
func isErr(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "err"
}

// isParam reports whether expr is the identifier of fn's parameter called
// name, holding the value fn was called with: fn declares that parameter
// (a top-level `name :=` cannot redeclare it, the parameters share the
// body's scope), and no statement of fn's body before the gate assigns it.
func isParam(expr ast.Expr, fn *ast.FuncDecl, name string, before []ast.Stmt) bool {
	id, ok := expr.(*ast.Ident)
	if !ok || id.Name != name {
		return false
	}
	declared := false
	for _, field := range fn.Type.Params.List {
		for _, p := range field.Names {
			declared = declared || p.Name == name
		}
	}
	if !declared {
		return false
	}
	for _, st := range before {
		if assignsName(st, name) {
			return false
		}
	}
	return true
}

// assignsName reports whether st, at any depth, assigns name with `=`.
func assignsName(st ast.Stmt, name string) bool {
	found := false
	ast.Inspect(st, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.ASSIGN {
			return !found
		}
		for _, lhs := range as.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// callsGitVerb reports whether call passes the string literal verb among
// its arguments — the shape every git subprocess of the runner takes
// (r.runGitEnv(ctx, dir, tok, env, "-c", …, "clone", …)).
func callsGitVerb(call *ast.CallExpr, verb string) bool {
	for _, arg := range call.Args {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if v, err := strconv.Unquote(lit.Value); err == nil && v == verb {
			return true
		}
	}
	return false
}

// isErrNotNil reports whether cond is exactly `err != nil`.
func isErrNotNil(cond ast.Expr) bool {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.NEQ {
		return false
	}
	y, yok := bin.Y.(*ast.Ident)
	return isErr(bin.X) && yok && y.Name == "nil"
}

// funcCalledIn names the function or method of `err := <f>(…)` /
// `err := r.<f>(…)` when init is that shape, "" otherwise.
func funcCalledIn(init ast.Stmt) string {
	as, ok := init.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return ""
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// returnsErrTyped reports whether the block is a single return of the
// verdict with its type reachable: the verdict itself as a result, or
// fmt.Errorf whose %w verb is matched by the verdict. A single statement,
// because a statement before the return can rebind or re-wrap the verdict
// (`err := fmt.Errorf("%v", err)`, `err = errors.New(…)`).
func returnsErrTyped(block *ast.BlockStmt) bool {
	if len(block.List) != 1 {
		return false
	}
	ret, ok := block.List[0].(*ast.ReturnStmt)
	if !ok {
		return false
	}
	for _, res := range ret.Results {
		if isErr(res) || wrapsErrWithW(res) {
			return true
		}
	}
	return false
}

// wrapsErrWithW reports whether expr is fmt.Errorf(format, args…) whose
// format literal has a %w verb whose argument is the verdict.
func wrapsErrWithW(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) < 2 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Errorf" {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
		return false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	format, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	verbs, ok := formatVerbs(format)
	if !ok {
		return false
	}
	args := call.Args[1:]
	for i, v := range verbs {
		if v != 'w' || i >= len(args) {
			continue
		}
		if isErr(args[i]) {
			return true
		}
	}
	return false
}

// formatVerbs lists the verbs of a fmt format string in argument order,
// skipping `%%`. A format using explicit argument indexes (`%[2]w`) or
// `*` widths is reported unreadable (ok false) rather than guessed at.
func formatVerbs(format string) (verbs []rune, ok bool) {
	rs := []rune(format)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '%' {
			continue
		}
		i++
		for i < len(rs) && (rs[i] == '+' || rs[i] == '-' || rs[i] == '#' || rs[i] == ' ' || rs[i] == '0' ||
			(rs[i] >= '1' && rs[i] <= '9') || rs[i] == '.') {
			i++
		}
		if i >= len(rs) {
			return nil, false
		}
		switch rs[i] {
		case '%':
			continue
		case '[', '*':
			return nil, false
		}
		verbs = append(verbs, rs[i])
	}
	return verbs, true
}
