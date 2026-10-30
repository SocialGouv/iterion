package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The class the derivation chokepoint closes is defined by the VERB, not by a
// route's name: a team-scoped self-authenticating token exists because one of
// three primitives fabricated it. These tests enumerate the callers of those
// primitives from the SOURCE and confront that set with what the mux really
// registers and with the table the OpenAPI spec is generated from. A new mint
// site, or a gate removed from an old one, fails here rather than in a review.
//
// Nothing here filters on a NAME: not on a `handle` prefix (the package
// registers s.hub.HandleWebSocket and SPAHandler through the same mux), not on
// a directory the chain is assumed to stay inside. A mint-reaching handler
// fails whatever it is called, a registration whose handler expression
// resolves to no function fails as unreadable, and a caller of a mint
// primitive living outside the directories the call graph covers fails too —
// the three shapes a guard keyed on a naming convention lets through.
//
// The sibling classes keep their own gates and are out of this one: a personal
// access token is MintPAT (pat.MintToken, gated in handleCreatePAT), a
// WebSocket ticket MintWSTicket, a forge installation token is minted from the
// GitHub App's key rather than from the presented credential.

// mintVerb is one primitive that fabricates a team token, in the two
// spellings source can reach it by.
type mintVerb struct {
	// call is how a file outside home spells it ("webhooks.MintToken"), and
	// for a method verb the selector's own name, whoever the receiver is.
	call string
	// home is the module-relative directory of the package that DEFINES the
	// primitive, where the call is spelled bare; empty for a method verb,
	// whose receiver no package name qualifies.
	home string
	// bare is that spelling inside home.
	bare string
	// pkgPath is the import path `call` must be qualified by for the exact
	// module-wide pass. Empty = matched on the selector name alone, which
	// over-approximates, the direction a guard must err in.
	pkgPath string
}

var teamTokenMintVerbs = []mintVerb{
	{call: "webhooks.MintToken", home: "pkg/webhooks", bare: "MintToken", pkgPath: "github.com/SocialGouv/iterion/pkg/webhooks"},
	{call: "configshare.MintToken", home: "pkg/configshare", bare: "MintToken", pkgPath: "github.com/SocialGouv/iterion/pkg/configshare"},
	{call: "CreateInvitation"},
}

// mintGraphDirs are the module-relative directories whose call graph this
// guard closes transitively: the handlers' own package, the orchestrator that
// mints two calls away from them, and the three primitives' own packages, so
// a hop THROUGH one of those packages is seen. A caller of a primitive
// anywhere else fails TestEveryMintPrimitiveCallerLivesInTheGuardsGraph,
// which is what keeps this list honest instead of assumed.
var mintGraphDirs = []string{"pkg/server", "pkg/forge", "pkg/webhooks", "pkg/configshare", "pkg/auth"}

// mintChokepointCalls are the calls a function's body makes to ASK the
// derivation chokepoint, each mapped to the mint it asks for — empty meaning
// "the mint is the call's last argument", which is how pkg/server spells its
// own question (checkDerivation(ctx, id, auth.MintTeamToken)). A function
// that calls a mint primitive and one of these IS the chokepoint for that
// mint: the chain stops there and its callers owe nothing.
//
// ApproveTeamTokenMint is the seam pkg/forge asks at the one line that
// reserves a hook identity; it carries no mint argument because it asks for
// exactly one, so it is named here with the mint it stands for.
var mintChokepointCalls = map[string]string{
	"checkDerivation":      "",
	"ApproveTeamTokenMint": "auth.MintTeamToken",
	"approveTeamTokenMint": "auth.MintTeamToken",
}

// moduleRoot is the module's directory as seen from pkg/server, where these
// tests run.
const moduleRoot = "../.."

// funcFacts is what this guard reads about one function: the names it calls,
// the mints it asks the chokepoint for, and where it is declared.
type funcFacts struct {
	calls    map[string]bool
	mintsAsk map[string]bool
	dir      string
	file     string
}

// parseDirFuncs parses every non-test .go file of a module-relative directory
// and answers its top-level functions AND package-level variables, keyed by
// name: a handler held by a package-level var is as registered as one held by
// a func, and a guard that only reads FuncDecls would not see its mint.
func parseDirFuncs(t *testing.T, dir string) map[string]*funcFacts {
	t.Helper()
	full := filepath.Join(moduleRoot, dir)
	entries, err := os.ReadDir(full)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]*funcFacts{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(full, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", dir, e.Name(), err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				record(out, d.Name.Name, dir, e.Name(), d.Body)
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i < len(vs.Values) {
							record(out, name.Name, dir, e.Name(), vs.Values[i])
						}
					}
				}
			}
		}
	}
	return out
}

// record folds the calls a node makes into name's facts.
func record(out map[string]*funcFacts, name, dir, file string, body ast.Node) {
	f := out[name]
	if f == nil {
		f = &funcFacts{calls: map[string]bool{}, mintsAsk: map[string]bool{}, dir: dir, file: file}
		out[name] = f
	}
	for call, mint := range nodeCalls(body) {
		f.calls[call] = true
		if mint != "" {
			f.mintsAsk[mint] = true
		}
	}
}

// nodeCalls answers every call a node makes, by the names the call is known
// by, with the mint the call names when it is a chokepoint question.
func nodeCalls(n ast.Node) map[string]string {
	out := map[string]string{}
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		names := calleeNames(call.Fun)
		mint := ""
		for _, name := range names {
			asks, ok := chokepointCall(name)
			if !ok {
				continue
			}
			if asks != "" {
				mint = asks
				continue
			}
			// The mint this question names is its LAST argument.
			if len(call.Args) > 0 {
				mint = exprText(call.Args[len(call.Args)-1])
			}
		}
		for _, name := range names {
			out[name] = mint
		}
		return true
	})
	return out
}

// chokepointCall reports whether name is a chokepoint question, and the mint
// it asks for when the call itself does not carry one.
func chokepointCall(name string) (string, bool) {
	if mint, ok := mintChokepointCalls[name]; ok {
		return mint, true
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		if mint, ok := mintChokepointCalls[name[i+1:]]; ok {
			return mint, true
		}
	}
	return "", false
}

// calleeNames answers the names a call expression's function part is known
// by: "Name" for an identifier, "Sel" and "x.Sel" for a selector.
func calleeNames(fun ast.Expr) []string {
	switch f := fun.(type) {
	case *ast.Ident:
		return []string{f.Name}
	case *ast.SelectorExpr:
		names := []string{f.Sel.Name}
		if x, ok := f.X.(*ast.Ident); ok {
			names = append(names, x.Name+"."+f.Sel.Name)
		}
		return names
	}
	return nil
}

// exprText renders an expression the way the source spells it, for the few
// shapes this guard compares ("auth.MintTeamToken", "s.mux").
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	}
	return ""
}

// verbNamed answers the mint verb a call name IS, for the inline handlers
// whose body reaches a primitive with no named function in between.
func verbNamed(call string) string {
	for _, verb := range teamTokenMintVerbs {
		if call == verb.call {
			return verb.call
		}
	}
	return ""
}

// mintReaching answers, keyed by function name, every function of
// mintGraphDirs whose body reaches a team-token mint, with the chain that
// proves it — and separately the functions that ARE the chokepoint for the
// mint they make (they ask it themselves, so their callers owe nothing).
//
// The graph is name-keyed and therefore an over-approximation: it demands a
// gate on more than the minimum, never on less.
func mintReaching(t *testing.T) (reaching map[string]string, atTheMint map[string]string, facts map[string]*funcFacts) {
	t.Helper()
	facts = map[string]*funcFacts{}
	perDir := map[string]map[string]*funcFacts{}
	for _, dir := range mintGraphDirs {
		fs := parseDirFuncs(t, dir)
		perDir[dir] = fs
		for name, f := range fs {
			if cur := facts[name]; cur != nil {
				for c := range f.calls {
					cur.calls[c] = true
				}
				for m := range f.mintsAsk {
					cur.mintsAsk[m] = true
				}
				continue
			}
			facts[name] = f
		}
	}
	reaching, atTheMint = map[string]string{}, map[string]string{}
	for _, verb := range teamTokenMintVerbs {
		for dir, fs := range perDir {
			for name, f := range fs {
				direct := f.calls[verb.call]
				if verb.home != "" && dir == verb.home && f.calls[verb.bare] {
					direct = true
				}
				if !direct {
					continue
				}
				if f.mintsAsk["auth.MintTeamToken"] {
					// It asks the chokepoint for the mint it makes: it IS
					// the chokepoint, and the chain stops here.
					atTheMint[name] = verb.call
					continue
				}
				reaching[name] = verb.call
			}
		}
	}
	// A function recorded as the chokepoint is never also propagated.
	for name := range atTheMint {
		delete(reaching, name)
	}
	for grew := true; grew; {
		grew = false
		for name, f := range facts {
			if _, done := reaching[name]; done {
				continue
			}
			if _, gated := atTheMint[name]; gated {
				continue
			}
			for callee := range f.calls {
				if via, ok := reaching[callee]; ok {
					reaching[name] = callee + " → " + via
					grew = true
					break
				}
			}
		}
	}
	if len(reaching) == 0 && len(atTheMint) == 0 {
		t.Fatalf("no caller of %v found: the guard is reading nothing", teamTokenMintVerbs)
	}
	return reaching, atTheMint, facts
}

// routeRegistration is one s.mux.Handle("METHOD /pattern", …) of pkg/server:
// every name its handler expression mentions, the function literals it holds
// inline, and the mint its gate names (empty when no gateDerivation wraps it).
type routeRegistration struct {
	key      string
	names    []string
	literals []ast.Node
	gateMint string
	file     string
	// enclosing is the function performing the registration, and locals
	// the names it OWNS (its receiver and everything it assigns): a handler
	// expression that resolves to no function is judged through the
	// registrar only when the registrar itself builds the value, which
	// those names are what proves.
	enclosing string
	locals    map[string]bool
}

// parsedRouteRegistrations reads every Handle / HandleFunc call of pkg/server
// taking two arguments. It does NOT require the pattern to be a string
// literal (studio_legacy_redirect.go builds its patterns) and it does NOT
// filter the handler's names: TestEveryTeamTokenMintSitsBehindTheChokepoint
// judges whatever resolves, and fails on a registration where nothing does.
func parsedRouteRegistrations(t *testing.T) []routeRegistration {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package dir: %v", err)
	}
	fset := token.NewFileSet()
	var out []routeRegistration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			locals := ownedNames(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
					return true
				}
				reg := routeRegistration{key: patternText(call.Args[0]), file: e.Name(), enclosing: fn.Name.Name, locals: locals}
				seen := map[string]bool{}
				ast.Inspect(call.Args[1], func(inner ast.Node) bool {
					switch v := inner.(type) {
					case *ast.FuncLit:
						reg.literals = append(reg.literals, v.Body)
					case *ast.Ident:
						if !seen[v.Name] {
							seen[v.Name] = true
							reg.names = append(reg.names, v.Name)
						}
					case *ast.SelectorExpr:
						if !seen[v.Sel.Name] {
							seen[v.Sel.Name] = true
							reg.names = append(reg.names, v.Sel.Name)
						}
					case *ast.CallExpr:
						gsel, ok := v.Fun.(*ast.SelectorExpr)
						if ok && gsel.Sel.Name == "gateDerivation" && len(v.Args) == 2 {
							reg.gateMint = exprText(v.Args[0])
						}
					}
					return true
				})
				out = append(out, reg)
				return true
			})
		}
	}
	if len(out) < 100 {
		t.Fatalf("only %d route registrations read: the guard is reading nothing", len(out))
	}
	return out
}

// ownedNames answers the names a function owns: its receiver, its parameters
// and every identifier it assigns. A handler value held by one of them is
// built inside the function or handed to it by a caller this guard reads
// separately (recordingMux.Handle forwards its own parameter, and every real
// registration through it is read at ITS call site). A name the function
// does not own — a struct field, a package-level variable this guard could
// not parse — names a value from somewhere nothing read, which is how a mint
// passes unseen.
func ownedNames(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	if fn.Recv != nil {
		for _, f := range fn.Recv.List {
			for _, n := range f.Names {
				out[n.Name] = true
			}
		}
	}
	if fn.Type != nil && fn.Type.Params != nil {
		for _, f := range fn.Type.Params.List {
			for _, n := range f.Names {
				out[n.Name] = true
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					out[id.Name] = true
				}
			}
		case *ast.ValueSpec:
			for _, id := range v.Names {
				out[id.Name] = true
			}
		case *ast.RangeStmt:
			if id, ok := v.Key.(*ast.Ident); ok {
				out[id.Name] = true
			}
			if id, ok := v.Value.(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

// patternText renders the route pattern for the message: the literal when it
// is one, the source's own spelling otherwise ("GET /"+seg), so a
// registration whose pattern is computed is still judged and still named.
func patternText(e ast.Expr) string {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if s, err := strconv.Unquote(lit.Value); err == nil {
			return s
		}
	}
	if b, ok := e.(*ast.BinaryExpr); ok {
		return patternText(b.X) + "+" + patternText(b.Y)
	}
	if t := exprText(e); t != "" {
		return t
	}
	return "«a pattern this guard cannot render»"
}

// TestEveryTeamTokenMintSitsBehindTheChokepoint is the class guard of #2687:
// every handler the mux registers that reaches a team-token mint asks the
// derivation chokepoint — at its registration (gateDerivation), in its own
// body (checkDerivation), or at the mint itself, where the chain stops
// because the minting function asks for it.
//
// The three ways a guard keyed on a naming convention fails OPEN are failures
// here: a handler not named handle* is judged like any other, a chain leaving
// pkg/server for another package is followed (and
// TestEveryMintPrimitiveCallerLivesInTheGuardsGraph proves the covered set
// is the whole truth), and a registration whose handler resolves to no
// function at all is a registration this guard cannot judge, so it fails.
func TestEveryTeamTokenMintSitsBehindTheChokepoint(t *testing.T) {
	reaching, atTheMint, facts := mintReaching(t)
	registrations := parsedRouteRegistrations(t)
	if len(atTheMint) == 0 {
		t.Error("no function was found asking the chokepoint at its own mint: the gate that lets a non-minting re-provision through is gone")
	}

	var judged []string
	for _, reg := range registrations {
		resolved := false
		for _, name := range reg.names {
			f, ok := facts[name]
			if !ok {
				continue
			}
			resolved = true
			via, reaches := reaching[name]
			if !reaches {
				continue
			}
			judged = append(judged, name)
			if reg.gateMint == "auth.MintTeamToken" || f.mintsAsk["auth.MintTeamToken"] {
				continue
			}
			t.Errorf("%q (%s, handler %s declared in %s) reaches a team-token mint (%s) with gate %q: register it through gateDerivation(auth.MintTeamToken, …) or ask checkDerivation in its body",
				reg.key, reg.file, name, f.dir, via, reg.gateMint)
		}
		for _, body := range reg.literals {
			resolved = true
			calls := nodeCalls(body)
			asks := false
			for name, mint := range calls {
				if _, ok := chokepointCall(name); ok && mint == "auth.MintTeamToken" {
					asks = true
				}
			}
			for call := range calls {
				// An inline handler reaches a mint either by calling a
				// primitive itself or by calling something that does.
				via, reaches := reaching[call]
				if !reaches {
					if v := verbNamed(call); v != "" {
						via, reaches = v, true
					}
				}
				if !reaches {
					continue
				}
				judged = append(judged, reg.key+" (inline)")
				if reg.gateMint == "auth.MintTeamToken" || asks {
					continue
				}
				t.Errorf("%q (%s) registers an inline handler that reaches a team-token mint (%s → %s) with gate %q",
					reg.key, reg.file, call, via, reg.gateMint)
			}
		}
		if resolved {
			continue
		}
		// Nothing in the handler expression names a function or a variable
		// this guard read, and it holds no inline body. It can still be
		// judged when the registrar BUILDS the value it hands over
		// (h := studioLegacyRedirect()): the registrar's own calls are then
		// the handler's. A name the registrar does not own comes from
		// somewhere this guard never read — a struct field, a parameter —
		// and a mint behind it would pass unseen, so it fails here.
		for _, name := range reg.names {
			if !reg.locals[name] {
				t.Errorf("%q (%s): its handler expression names %q, which resolves to no function this guard read and is not built by its registrar %s — this registration cannot be judged, which is how a mint passes unseen",
					reg.key, reg.file, name, reg.enclosing)
			}
		}
		if _, ok := facts[reg.enclosing]; !ok {
			t.Errorf("%q (%s): this guard resolves neither its handler %v nor its registrar %s",
				reg.key, reg.file, reg.names, reg.enclosing)
			continue
		}
		if via, reaches := reaching[reg.enclosing]; reaches {
			judged = append(judged, reg.enclosing)
			if reg.gateMint != "auth.MintTeamToken" {
				t.Errorf("%q (%s): its handler %v resolves to nothing this guard read, and its registrar %s reaches a team-token mint (%s) with gate %q",
					reg.key, reg.file, reg.names, reg.enclosing, via, reg.gateMint)
			}
		}
	}
	if len(judged) == 0 {
		t.Fatal("no registered mint-reaching handler found: the guard is reading nothing")
	}
	sort.Strings(judged)
	gatedAtMint := make([]string, 0, len(atTheMint))
	for name := range atTheMint {
		gatedAtMint = append(gatedAtMint, name)
	}
	sort.Strings(gatedAtMint)
	t.Logf("registered mint-reaching handlers judged: %v", judged)
	t.Logf("functions that ask the chokepoint AT their own mint: %v", gatedAtMint)
}

// TestEveryMintPrimitiveCallerLivesInTheGuardsGraph is what makes
// mintGraphDirs a fact rather than an assumption: an EXACT pass over every
// non-vendor directory of the module looks for direct callers of the mint
// primitives — resolved through each file's own imports, so `pat.MintToken`
// is not mistaken for `webhooks.MintToken` — and fails when one lives
// outside the directories the call graph closes. A mint reached through a
// third package is then either followed or reported, never unseen.
func TestEveryMintPrimitiveCallerLivesInTheGuardsGraph(t *testing.T) {
	covered := map[string]bool{}
	for _, d := range mintGraphDirs {
		covered[d] = true
	}
	var outside, inside []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(moduleRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != moduleRoot && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			// A file the Go parser refuses is not a file the compiler
			// builds; say so rather than skipping in silence.
			t.Errorf("parse %s: %v", p, perr)
			return nil
		}
		rel, rerr := filepath.Rel(moduleRoot, filepath.Dir(p))
		if rerr != nil {
			return rerr
		}
		rel = path.Clean(filepath.ToSlash(rel))
		aliases := importAliases(file)
		pkgDir := rel
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if verb := directMintCall(fn.Body, aliases, pkgDir); verb != "" {
				where := rel + "/" + filepath.Base(p) + ":" + fn.Name.Name + " → " + verb
				if covered[rel] {
					inside = append(inside, where)
				} else {
					outside = append(outside, where)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	if len(inside) == 0 {
		t.Fatal("this pass found no caller of any mint primitive in the whole module: it is reading nothing")
	}
	sort.Strings(outside)
	for _, o := range outside {
		t.Errorf("%s lives outside the directories TestEveryTeamTokenMintSitsBehindTheChokepoint closes (%v): add its directory to mintGraphDirs so the chain is followed, or the mint passes unseen", o, mintGraphDirs)
	}
	sort.Strings(inside)
	t.Logf("direct callers of a team-token mint primitive, all inside the graph: %v", inside)
}

// importAliases answers the local name each import is reachable by.
func importAliases(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		alias := path.Base(p)
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		out[alias] = p
	}
	return out
}

// directMintCall answers the verb a body calls DIRECTLY, resolved exactly:
// a selector whose receiver is the file's own alias for the primitive's
// package, a bare call inside that package, or — for the method verb — the
// selector's name whoever the receiver is.
func directMintCall(body ast.Node, aliases map[string]string, pkgDir string) string {
	found := ""
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found != "" {
			return found == ""
		}
		for _, verb := range teamTokenMintVerbs {
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				if verb.pkgPath == "" {
					if f.Sel.Name == verb.call {
						found = verb.call
					}
					continue
				}
				x, ok := f.X.(*ast.Ident)
				if !ok {
					continue
				}
				wantSel := verb.call[strings.Index(verb.call, ".")+1:]
				if f.Sel.Name == wantSel && aliases[x.Name] == verb.pkgPath {
					found = verb.call
				}
			case *ast.Ident:
				if verb.home != "" && pkgDir == verb.home && f.Name == verb.bare {
					found = verb.call
				}
			}
		}
		return found == ""
	})
	return found
}

// TestTheChokepointTableMatchesTheRegistrations confronts the hand-written
// table the OpenAPI spec is generated from (gatedRouteChokepointCodes) with
// the registrations themselves: a route gated by gateDerivation states its
// 401 and 403 in the published spec, and the table claims no gate the mux
// does not have. The table is the only place the refusals are declared, so
// nothing but this comparison keeps it true.
func TestTheChokepointTableMatchesTheRegistrations(t *testing.T) {
	// The routes whose refusal comes from BENEATH the registration: the
	// handler asks checkDerivation itself, or the mint it reaches asks at
	// the mint (the repo-bot provisioning, whose question is only put when
	// a hook identity is really reserved). Each names the mint it asks for.
	gatedBeneathTheRegistration := map[string]string{
		"POST /api/auth/me/team/{team_id}":          "MintAccessToken",
		"POST /api/auth/me/org/{org_id}":            "MintAccessToken",
		"POST /api/me/tokens":                       "MintPAT",
		"POST /api/ws/ticket":                       "MintWSTicket",
		"GET /api/me/sso/{provider}/link/start":     "MintSSOLink",
		"POST /api/admin/users/{id}/reset-password": "MintPassword",
		// forge.Orchestrator.ApproveTeamTokenMint, asked at the
		// reservation of a NEW hook identity: a re-provision that reuses
		// the integration's sealed secret mints nothing and answers 200.
		"POST /api/teams/{id}/forge/repo-bots":                          "MintTeamToken",
		"PATCH /api/teams/{id}/forge/repo-bots/{integration_id}":        "MintTeamToken",
		"POST /api/orgs/{id}/provision-approvals/{approval_id}/approve": "MintTeamToken",
	}
	byMiddleware := map[string]bool{}
	for _, reg := range parsedRouteRegistrations(t) {
		if reg.gateMint == "" {
			continue
		}
		byMiddleware[reg.key] = true
		codes, ok := gatedRouteChokepointCodes[reg.key]
		if !ok {
			t.Errorf("%q is registered through gateDerivation(%s, …) but states no refusal in gatedRouteChokepointCodes: the OpenAPI spec hides it", reg.key, reg.gateMint)
			continue
		}
		var has401, has403 bool
		for _, c := range codes {
			has401 = has401 || c == http.StatusUnauthorized
			has403 = has403 || c == http.StatusForbidden
		}
		if !has401 || !has403 {
			t.Errorf("%q is gated but its table entry %v misses 401 or 403", reg.key, codes)
		}
	}
	for key := range gatedRouteChokepointCodes {
		if byMiddleware[key] {
			continue
		}
		if _, ok := gatedBeneathTheRegistration[key]; !ok {
			t.Errorf("%q claims the chokepoint's refusals but no gateDerivation registration carries it, and it is not declared as gated beneath its registration", key)
		}
	}
	for key := range gatedBeneathTheRegistration {
		if byMiddleware[key] {
			t.Errorf("%q is declared as gated beneath its registration AND registered through gateDerivation: one of the two is stale", key)
		}
		if _, ok := gatedRouteChokepointCodes[key]; !ok {
			t.Errorf("%q is declared as gated beneath its registration but states no refusal in gatedRouteChokepointCodes: the OpenAPI spec hides a 401/403 the route really answers", key)
		}
	}
}

// TestTheForgeOrchestratorIsBuiltWithTheMintChokepoint closes the one hole
// an AST guard on pkg/forge cannot see: the question asked at the mint is
// asked through an injected seam, and a seam left nil asks nobody. Wherever
// this package builds a forge.Orchestrator, ApproveTeamTokenMint is wired —
// a server that provisions without it would mint the repo's hook token for
// any credential that reaches the route.
func TestTheForgeOrchestratorIsBuiltWithTheMintChokepoint(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package dir: %v", err)
	}
	fset := token.NewFileSet()
	built := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, e.Name(), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", e.Name(), perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || exprText(lit.Type) != "forge.Orchestrator" {
				return true
			}
			built++
			wired := ""
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if exprText(kv.Key) == "ApproveTeamTokenMint" {
					wired = exprText(kv.Value)
				}
			}
			if wired == "" || wired == "nil" {
				t.Errorf("%s builds a forge.Orchestrator with ApproveTeamTokenMint %q: the chokepoint asked at the mint is a seam, and an unwired seam asks nobody",
					e.Name(), wired)
			}
			return true
		})
	}
	if built == 0 {
		t.Fatal("no forge.Orchestrator is built in this package: the guard is reading nothing")
	}
}
