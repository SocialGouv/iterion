package main

import (
	"bytes"
	"go/ast"
	"go/constant"
	"go/format"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// binding is intentionally parser-local: loading packages would select one
// platform/build-tag set and lose the other source sites. Resolution is limited
// to the syntactic cases documented in the ledger; unknowns stay unresolved.
type binding = ast.Object //nolint:staticcheck // SA1019: syntax-only, not a replacement for go/types.

func scan(root string, paths []string) (census, error) {
	c := census{SchemaVersion: 1, Mode: "static", Exclusions: []string{"git-ignored untracked files", "vendor/", "testdata/ (synthetic source)"}, Counts: map[string]int{"skip/reason/named": 0, "skip/reason/missing": 0, "skip/reason/dynamic": 0, "sleep/constant": 0, "sleep/dynamic": 0}, Skips: []skip{}, Sleeps: []sleep{}}
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	for _, path := range paths {
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, filepath.Join(root, filepath.FromSlash(path)), nil, parser.ParseComments)
		if err != nil {
			return c, err
		}
		c.Files++
		imports := map[string]string{}
		for _, i := range f.Imports {
			p, _ := strconv.Unquote(i.Path.Value)
			name := filepath.Base(p)
			if i.Name != nil {
				name = i.Name.Name
			}
			imports[name] = p
			if name == "." {
				imports["."+p] = p
			}
		}
		constraint := ""
		for _, group := range f.Comments {
			for _, comment := range group.List {
				if strings.HasPrefix(comment.Text, "//go:build ") {
					constraint = strings.TrimPrefix(comment.Text, "//go:build ")
				}
			}
		}
		// A function owns its nested closures. The enclosing test/helper remains
		// stable even when a runtime subtest name is computed from a table.
		for _, decl := range f.Decls {
			name := "<package>"
			node := ast.Node(decl)
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if fn.Body == nil {
					continue
				}
				name, node = fn.Name.Name, fn.Body
			}
			probes := functionProbes(node, imports)
			var bubbles []*ast.CallExpr
			ast.Inspect(node, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && (isPackageCall(call.Fun, "testing/synctest", "Test", imports) || isPackageCall(call.Fun, "testing/synctest", "Run", imports)) {
					bubbles = append(bubbles, call)
				}
				return true
			})
			ast.Inspect(node, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pos := fs.PositionFor(call.Pos(), false)
				where := site{File: path, Line: pos.Line, Column: pos.Column, Function: name, Scope: scope(path), BuildConstraint: constraint}
				if isPackageCall(call.Fun, "time", "Sleep", imports) && len(call.Args) == 1 {
					clock := "caller-dependent"
					for _, bubble := range bubbles {
						if call.Pos() > bubble.Pos() && call.End() < bubble.End() {
							clock = "synctest"
						}
					}
					s := sleep{site: where, Clock: clock, Expression: expression(fs, call.Args[0]), Status: "dynamic"}
					value := evaluate(call.Args[0], imports, map[*binding]bool{})
					if value != nil && value.Kind() != constant.Unknown {
						if ns, exact := constant.Int64Val(constant.ToInt(value)); exact {
							minimum := max(int64(0), ns)
							s.RequestedNS, s.MinimumNS, s.Status = &ns, &minimum, "constant"
						}
					}
					c.Sleeps = append(c.Sleeps, s)
					c.Counts["sleep/"+s.Status]++
					c.Counts["sleep/scope/"+s.Scope]++
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Skip" && sel.Sel.Name != "Skipf" && sel.Sel.Name != "SkipNow") || !testingReceiver(sel.X, imports, map[*binding]bool{}) {
					return true
				}
				s := skip{site: where, Method: sel.Sel.Name, Expression: expression(fs, call), ReasonStatus: "missing", FunctionProbes: probes}
				s.Message, s.ReasonStatus = skipMessage(call, sel.Sel.Name, imports)
				s.Class = classify(s.Message, s.ReasonStatus)
				c.Skips = append(c.Skips, s)
				c.Counts["skip/reason/"+s.ReasonStatus]++
				c.Counts["skip/class/"+s.Class]++
				c.Counts["skip/scope/"+s.Scope]++
				return true
			})
		}
	}
	return c, nil
}
func skipMessage(call *ast.CallExpr, method string, imports map[string]string) (string, string) {
	args := call.Args
	if method == "Skipf" && len(args) > 1 {
		args = args[:1]
	}
	var parts []string
	dynamic := call.Ellipsis.IsValid()
	for _, arg := range args {
		v := evaluate(arg, imports, map[*binding]bool{})
		if v == nil || v.Kind() != constant.String {
			dynamic = true
			continue
		}
		parts = append(parts, constant.StringVal(v))
	}
	message := strings.Join(parts, "")
	if strings.TrimSpace(message) != "" {
		return message, "named"
	}
	if dynamic {
		return message, "dynamic"
	}
	return message, "missing"
}

func scope(path string) string {
	if strings.HasPrefix(path, "third_party/") {
		return "third_party"
	}
	if strings.HasSuffix(path, "_test.go") {
		return "test"
	}
	return "support-or-production"
}
func expression(fs *token.FileSet, e ast.Node) string {
	var b bytes.Buffer
	_ = format.Node(&b, fs, e)
	return b.String()
}
func isPackageCall(e ast.Expr, pkg, name string, imports map[string]string) bool {
	if sel, ok := e.(*ast.SelectorExpr); ok {
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Obj == nil && imports[id.Name] == pkg && sel.Sel.Name == name
	}
	id, ok := e.(*ast.Ident)
	return ok && id.Obj == nil && (imports["."+pkg] == pkg || imports["."] == pkg) && id.Name == name
}
func testingType(e ast.Expr, imports map[string]string, seen map[*binding]bool) bool {
	if p, ok := e.(*ast.StarExpr); ok {
		return testingType(p.X, imports, seen)
	}
	for _, name := range []string{"T", "B", "F", "TB"} {
		if isPackageCall(e, "testing", name, imports) {
			return true
		}
	}
	if id, ok := e.(*ast.Ident); ok && id.Obj != nil && !seen[id.Obj] {
		seen[id.Obj] = true
		if d, ok := id.Obj.Decl.(*ast.TypeSpec); ok && d.Assign.IsValid() {
			return testingType(d.Type, imports, seen)
		}
	}
	return false
}
func testingReceiver(e ast.Expr, imports map[string]string, seen map[*binding]bool) bool {
	return testingType(expressionType(e, seen), imports, map[*binding]bool{})
}

// Resolve local aliases and fixture fields without loading platform-dependent
// packages. Cross-file/user-defined wrappers are not guessed to be testing.T.
func expressionType(e ast.Expr, seen map[*binding]bool) ast.Expr {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return expressionType(e.X, seen)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return expressionType(e.X, seen)
		}
	case *ast.CompositeLit:
		return e.Type
	case *ast.SelectorExpr:
		typ := expressionType(e.X, seen)
		for typ != nil {
			switch t := typ.(type) {
			case *ast.StarExpr:
				typ = t.X
			case *ast.Ident:
				if t.Obj == nil || seen[t.Obj] {
					return nil
				}
				seen[t.Obj] = true
				if d, ok := t.Obj.Decl.(*ast.TypeSpec); ok {
					typ = d.Type
				} else {
					return nil
				}
			case *ast.StructType:
				for _, f := range t.Fields.List {
					for _, name := range f.Names {
						if name.Name == e.Sel.Name {
							return f.Type
						}
					}
				}
				return nil
			default:
				return nil
			}
		}
	case *ast.Ident:
		if e.Obj == nil || seen[e.Obj] {
			return nil
		}
		seen[e.Obj] = true
		switch d := e.Obj.Decl.(type) {
		case *ast.Field:
			return d.Type
		case *ast.ValueSpec:
			if d.Type != nil {
				return d.Type
			}
			for i, name := range d.Names {
				if name.Name == e.Name && i < len(d.Values) {
					return expressionType(d.Values[i], seen)
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range d.Lhs {
				if name, ok := lhs.(*ast.Ident); ok && name.Name == e.Name && i < len(d.Rhs) {
					return expressionType(d.Rhs[i], seen)
				}
			}
		}
	}
	return nil
}

// evaluate deliberately resolves constants only. A variable initialized to 1s
// can change before Sleep; reporting its initializer as a bound would lie.
// Unsupported expressions, cross-file constants and iota remain explicit nulls.
func evaluate(e ast.Expr, imports map[string]string, seen map[*binding]bool) (result constant.Value) {
	defer func() {
		if recover() != nil {
			result = nil
		}
	}()
	switch e := e.(type) {
	case *ast.BasicLit:
		// Untyped float/complex values coerce beside a typed Duration;
		// folding them as rationals can change division and invent a bound.
		if e.Kind != token.INT && e.Kind != token.CHAR && e.Kind != token.STRING {
			return nil
		}
		return constant.MakeFromLiteral(e.Value, e.Kind, 0)
	case *ast.ParenExpr:
		return evaluate(e.X, imports, seen)
	case *ast.UnaryExpr:
		if v := evaluate(e.X, imports, seen); v != nil {
			return constant.UnaryOp(e.Op, v, 0)
		}
	case *ast.BinaryExpr:
		a, b := evaluate(e.X, imports, seen), evaluate(e.Y, imports, seen)
		if a == nil || b == nil {
			return nil
		}
		if e.Op == token.SHL || e.Op == token.SHR {
			n, ok := constant.Uint64Val(b)
			if !ok || n > 63 {
				return nil
			}
			return constant.Shift(a, e.Op, uint(n))
		}
		op := e.Op
		if op == token.QUO && a.Kind() == constant.Int && b.Kind() == constant.Int {
			op = token.QUO_ASSIGN
		}
		return constant.BinaryOp(a, op, b)
	case *ast.CallExpr:
		if isPackageCall(e.Fun, "time", "Duration", imports) && len(e.Args) == 1 {
			return evaluate(e.Args[0], imports, seen)
		}
	case *ast.Ident:
		if e.Obj != nil && e.Obj.Kind == ast.Con && !seen[e.Obj] {
			seen[e.Obj] = true
			defer delete(seen, e.Obj)
			if d, ok := e.Obj.Decl.(*ast.ValueSpec); ok {
				// Width-sensitive typed constants need type checking (for
				// example ^uint8(1) is 254, not -2). Keep them unresolved.
				if d.Type != nil && !isPackageCall(d.Type, "time", "Duration", imports) {
					return nil
				}
				for i, name := range d.Names {
					if name.Name == e.Name && i < len(d.Values) {
						return evaluate(d.Values[i], imports, seen)
					}
				}
			}
		}
	}
	units := map[string]time.Duration{"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond, "Second": time.Second, "Minute": time.Minute, "Hour": time.Hour}
	for name, ns := range units {
		if isPackageCall(e, "time", name, imports) {
			return constant.MakeInt64(int64(ns))
		}
	}
	return nil
}
func functionProbes(node ast.Node, imports map[string]string) []prerequisite {
	var out []prerequisite
	seen := map[prerequisite]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		kind := ""
		if isPackageCall(call.Fun, "os", "Getenv", imports) || isPackageCall(call.Fun, "os", "LookupEnv", imports) {
			kind = "environment"
		}
		if isPackageCall(call.Fun, "os/exec", "LookPath", imports) {
			kind = "executable"
		}
		if kind == "" {
			return true
		}
		v := evaluate(call.Args[0], imports, map[*binding]bool{})
		if v == nil || v.Kind() != constant.String {
			return true
		}
		p := prerequisite{kind, constant.StringVal(v)}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
		return true
	})
	return out
}

// Classes are generated hints, never a claim that a requirement is available.
// An explicit [class] prefix wins. Unknown prose stays visibly "condition".
func classify(message, status string) string {
	if status != "named" {
		return status
	}
	lower := strings.ToLower(message)
	rules := []struct {
		class string
		words []string
	}{
		{"mode", []string{"short mode", "-short", "under -race", "race detector", "corpus bench", "refresh deadline", "opt into"}},
		{"credentials", []string{"credential", "api_key", "token not set", "token_file", "gitlab_live_", "re-authenticate", "forge_github_token"}},
		{"service", []string{"mongo", "nats", "s3_endpoint", "gateway", "127.0.0.1", "registry unavailable"}},
		{"tool", []string{"path", "cli", "python3", "git not", "git --exec-path", "git http-backend", "no shell", "no gcc", "no dash", "mkfifo", "ssh-keygen", "release-it", "not installed", "ultra11y_bin", "setsid is missing", "bash is missing", "docker"}},
		{"platform", []string{"windows", "linux", "unix", "posix", "platform", "kernel", "procfs", "/proc/", "rlimit", "process namespace", "no pty"}},
		{"filesystem", []string{"permission", "symlink", "hardlink", "hard link", "filesystem", "mode-0", "root reads", "root removes", "root traverses", "root unlinks", "root ignores", "chmod", "no fifo", "sealed dir", "mode bits"}},
		{"fixture", []string{"fixture", "example", "catalog", ".botz", "presets/", "scenario", "helper process", "helper server", "helper runs"}},
		{"capability", []string{"does not implement", "backend has no", "unavailable in this build", "builtin in this build", "cross-process lock"}},
		{"environment", []string{"environment", "ambient", "tz ", "already enabled", "in-cluster", "display", "host refused", "fsnotify", "not set", "unset"}},
	}
	for _, r := range rules {
		if strings.HasPrefix(lower, "["+r.class+"] ") {
			return r.class
		}
	}
	for _, r := range rules {
		for _, word := range r.words {
			if strings.Contains(lower, word) {
				return r.class
			}
		}
	}
	return "condition"
}
