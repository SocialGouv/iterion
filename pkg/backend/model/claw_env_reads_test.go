package model

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"testing"
)

// envReadForms are the calls that read an environment variable named by
// their first argument — the forms internal/envtrust's routing census knows,
// and syscall's and x/sys/unix's — keyed by import path. claw is first-party
// code since the fold and may read through envtrust as well as os.
var envReadForms = map[string][]string{
	"os":                    {"Getenv", "LookupEnv"},
	"syscall":               {"Getenv"},
	"golang.org/x/sys/unix": {"Getenv"},
	"github.com/SocialGouv/iterion/internal/envtrust":     {"Inherited", "Planted"},
	"github.com/SocialGouv/iterion/internal/isolationenv": {"Value"},
}

// envExpandForms read every variable their template argument names
// (`os.ExpandEnv("$HOME/.npmrc")`).
var envExpandForms = map[string][]string{"os": {"ExpandEnv"}}

// envImplicitReads are the calls that read variables they are not handed:
// the standard library's directory lookups, the PATH search every command
// spawn and lookup makes, and the working-directory hint. The names are the
// Unix ones, which a sandboxed runner's Linux container uses.
var envImplicitReads = map[string]map[string][]string{
	"os": {
		"UserHomeDir":   {"HOME"},
		"UserCacheDir":  {"XDG_CACHE_HOME", "HOME"},
		"UserConfigDir": {"XDG_CONFIG_HOME", "HOME"},
		"TempDir":       {"TMPDIR"},
		"MkdirTemp":     {"TMPDIR"},
		"CreateTemp":    {"TMPDIR"},
		"Getwd":         {"PWD"},
	},
	"path/filepath": {"Abs": {"PWD"}},
	"os/exec": {
		"Command":        {"PATH"},
		"CommandContext": {"PATH"},
		"LookPath":       {"PATH"},
	},
	"github.com/SocialGouv/iterion/internal/envtrust": {"LookPath": {"PATH"}},
}

// envName is what a shell would expand: os.ExpandEnv also hands `$5` and `$$`
// to its mapping, which are not variables.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envNamesReadIn returns, sorted and deduplicated, every env name the given
// files and directories read, and fails the test on each read it cannot
// decide (envReads).
func envNamesReadIn(t *testing.T, paths []string) []string {
	t.Helper()
	var files []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.IsDir() {
			files = append(files, nonTestGoFilesIn(t, path)...)
			continue
		}
		files = append(files, path)
	}
	names, problems, err := envReads(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	return names
}

// envReads derives the env names the files read, and lists every read whose
// name the derivation cannot see: a name that is not a string literal, a read
// form handed on as a value (`env = os.Getenv` is tracked; `f(os.Getenv)`,
// a struct field or a return is not), a tracked reader handed on in turn —
// returned, held in a result or an exported package variable — and a dot
// import of a read form's package, which hides the form's name.
//
// A reader held in a variable or a parameter (`get := os.Getenv`,
// `var getenv = os.Getenv`, the `if env == nil { env = os.Getenv }` default)
// is tracked by its go/types object. The files of each package are
// type-checked together, without an importer, so a package variable read
// from another of its files resolves too. A call through a reader counts as a
// read of its literal argument; a parameter that only ever receives its
// reader from a caller outside the walked files is not seen.
func envReads(files []string) (names, problems []string, err error) {
	fset := token.NewFileSet()
	type pkgKey struct{ dir, name string }
	packages := map[pkgKey][]*ast.File{}
	var order []pkgKey
	sorted := slices.Clone(files)
	sort.Strings(sorted)
	for _, file := range sorted {
		f, perr := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", file, perr)
		}
		key := pkgKey{filepath.Dir(file), f.Name.Name}
		if _, seen := packages[key]; !seen {
			order = append(order, key)
		}
		packages[key] = append(packages[key], f)
	}
	found := map[string]bool{}
	for _, key := range order {
		problems = append(problems, envReadsInPackage(fset, key.name, packages[key], found)...)
	}
	names = make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, problems, nil
}

func envReadsInPackage(fset *token.FileSet, name string, files []*ast.File, found map[string]bool) []string {
	var problems []string
	report := func(n ast.Node, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: ", fset.Position(n.Pos()))+fmt.Sprintf(format, args...))
	}

	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Error: func(error) {}, FakeImportC: true}
	pkg, _ := conf.Check(name, fset, files, info)

	// The name each read-form package is imported by, per file.
	imports := map[*ast.File]map[string]string{} // file → local name → import path
	for _, f := range files {
		imported := map[string]string{}
		for _, spec := range f.Imports {
			importPath, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				continue
			}
			_, named := envReadForms[importPath]
			_, expands := envExpandForms[importPath]
			_, implicit := envImplicitReads[importPath]
			if !named && !expands && !implicit {
				continue
			}
			local := path.Base(importPath)
			if spec.Name != nil {
				local = spec.Name.Name
			}
			if local == "." {
				report(spec, "a dot import of %s hides which of its calls read the environment: import it by name, so this derivation can decide what it reads", importPath)
				continue
			}
			imported[local] = importPath
		}
		imports[f] = imported
	}
	formOf := func(f *ast.File, e ast.Expr) (importPath, fn string, ok bool) {
		sel, isSel := ast.Unparen(e).(*ast.SelectorExpr)
		if !isSel {
			return "", "", false
		}
		x, isIdent := sel.X.(*ast.Ident)
		if !isIdent {
			return "", "", false
		}
		importPath, ok = imports[f][x.Name]
		if !ok {
			return "", "", false
		}
		if o := info.Uses[x]; o != nil {
			if _, isPkg := o.(*types.PkgName); !isPkg {
				return "", "", false // a local named like the import shadows it
			}
		}
		fn = sel.Sel.Name
		if slices.Contains(envReadForms[importPath], fn) || slices.Contains(envExpandForms[importPath], fn) {
			return importPath, fn, true
		}
		if _, implicit := envImplicitReads[importPath][fn]; implicit {
			return importPath, fn, true
		}
		return "", "", false
	}
	namedForm := func(importPath, fn string) bool { return slices.Contains(envReadForms[importPath], fn) }

	objectOf := func(id *ast.Ident) types.Object {
		if o := info.Defs[id]; o != nil {
			return o
		}
		return info.Uses[id]
	}

	// Readers: variables and parameters holding a named read form, directly
	// or through another reader. accounted holds the expressions whose value
	// use is explained: the right-hand side that made a reader.
	readers := map[types.Object]bool{}
	accounted := map[ast.Expr]bool{}
	isReaderValue := func(f *ast.File, e ast.Expr) bool {
		if importPath, fn, ok := formOf(f, e); ok {
			return namedForm(importPath, fn)
		}
		if id, ok := ast.Unparen(e).(*ast.Ident); ok {
			o := objectOf(id)
			return o != nil && readers[o]
		}
		return false
	}
	markFrom := func(f *ast.File, lhs, rhs []ast.Expr) bool {
		changed := false
		if len(lhs) != len(rhs) {
			return false
		}
		for i := range rhs {
			if !isReaderValue(f, rhs[i]) {
				continue
			}
			id, ok := lhs[i].(*ast.Ident)
			if !ok {
				continue // a field or an index: reported below as a value use
			}
			o := objectOf(id)
			if o == nil && pkg != nil && id.Name != "_" {
				// A build-tag twin redeclares the name: go/types records the
				// first declaration only, and every use resolves to it — so
				// the reader is that object, whichever twin sorted first.
				o = pkg.Scope().Lookup(id.Name)
			}
			if o == nil {
				continue
			}
			accounted[rhs[i]] = true
			if !readers[o] {
				readers[o] = true
				changed = true
			}
		}
		return changed
	}
	for changed := true; changed; {
		changed = false
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.AssignStmt:
					changed = markFrom(f, v.Lhs, v.Rhs) || changed
				case *ast.ValueSpec:
					lhs := make([]ast.Expr, len(v.Names))
					for i, n := range v.Names {
						lhs[i] = n
					}
					changed = markFrom(f, lhs, v.Values) || changed
				}
				return true
			})
		}
	}

	literalName := func(call *ast.CallExpr, what string) (string, bool) {
		if len(call.Args) == 0 {
			report(call, "%s is called without a name", what)
			return "", false
		}
		lit, ok := ast.Unparen(call.Args[0]).(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			report(call, "%s reads an environment variable whose name is not a string literal: spell the name at the read, so this derivation can decide it", what)
			return "", false
		}
		s, uerr := strconv.Unquote(lit.Value)
		if uerr != nil {
			report(call, "%s: unreadable literal %s", what, lit.Value)
			return "", false
		}
		return s, true
	}
	// Reader identifiers whose use is a call, an assignment (to it, or of it
	// to another reader) or a nil test; any other use hands the reader on.
	allowed := map[*ast.Ident]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if importPath, fn, ok := formOf(f, v.Fun); ok {
					accounted[ast.Unparen(v.Fun)] = true
					what := importPath + "." + fn
					switch {
					case namedForm(importPath, fn):
						if name, ok := literalName(v, what); ok && name != "" {
							found[name] = true
						}
					case slices.Contains(envExpandForms[importPath], fn):
						if tmpl, ok := literalName(v, what); ok {
							os.Expand(tmpl, func(name string) string {
								if envName.MatchString(name) {
									found[name] = true
								}
								return ""
							})
						}
					default:
						for _, name := range envImplicitReads[importPath][fn] {
							found[name] = true
						}
					}
					return true
				}
				if id, ok := ast.Unparen(v.Fun).(*ast.Ident); ok {
					if o := objectOf(id); o != nil && readers[o] {
						allowed[id] = true
						if name, ok := literalName(v, "the reader "+id.Name); ok && name != "" {
							found[name] = true
						}
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range v.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						allowed[id] = true
					}
					if i < len(v.Rhs) && accounted[v.Rhs[i]] {
						if rid, ok := ast.Unparen(v.Rhs[i]).(*ast.Ident); ok {
							allowed[rid] = true
						}
					}
				}
			case *ast.ValueSpec:
				for _, value := range v.Values {
					if rid, ok := ast.Unparen(value).(*ast.Ident); ok && accounted[value] {
						allowed[rid] = true
					}
				}
			case *ast.BinaryExpr:
				if v.Op == token.EQL || v.Op == token.NEQ {
					for _, side := range [2][2]ast.Expr{{v.X, v.Y}, {v.Y, v.X}} {
						if id, ok := ast.Unparen(side[0]).(*ast.Ident); ok {
							if other, ok := ast.Unparen(side[1]).(*ast.Ident); ok && other.Name == "nil" {
								allowed[id] = true
							}
						}
					}
				}
			}
			return true
		})
	}

	// Every other use of a read form, or of a reader, hands it on as a value
	// whose reads this derivation cannot see.
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				if importPath, fn, ok := formOf(f, v); ok && !accounted[v] {
					if namedForm(importPath, fn) {
						report(v, "%s.%s is handed on as a value: the names it reads there are invisible to this derivation — call it at the read with a literal name, or hold it in a local reader", importPath, fn)
					} else {
						report(v, "%s.%s is handed on as a value: the variables it reads there are invisible to this derivation — call it directly", importPath, fn)
					}
				}
			case *ast.Ident:
				if o := info.Uses[v]; o != nil && readers[o] && !allowed[v] {
					report(v, "the env reader %s is handed on as a value: the names it reads there are invisible to this derivation — call it here with a literal name", v.Name)
				}
			}
			return true
		})
	}
	// A reader held where code outside these files can reach it — a result,
	// an exported package variable — reads names this walk never sees.
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			v, ok := info.Defs[id].(*types.Var)
			if !ok || !readers[v] {
				return true
			}
			switch {
			case v.Kind() == types.ResultVar:
				report(id, "the env reader %s is a result: its caller reads names this derivation cannot see — call the read form here with a literal name", id.Name)
			case v.Kind() == types.PackageVar && v.Exported():
				report(id, "the env reader %s is an exported package variable: another package can read names through it that this derivation cannot see — unexport it", id.Name)
			}
			return true
		})
	}
	return problems
}
