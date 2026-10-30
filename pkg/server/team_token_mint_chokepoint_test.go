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
// three primitives fabricated it. Two guards before this one tried to
// RECOGNISE the shapes a mint hides behind and failed open — once on a name
// filter (any handler not named handle* passed unseen), once on presence
// (a mint held by a variable, or a chokepoint ask parked in a branch the
// mint never runs under, passed unseen). This one RESOLVES symbols instead:
//
//   - it enumerates the users of the mint primitives from the source,
//     resolved exactly through each file's own imports, across whole
//     function declarations AND package-level variables, in every non-vendor
//     directory of the module — a primitive is a user either called or held
//     as a VALUE (`var alias = webhooks.MintToken`), wherever the reference
//     is written: an initializer, a composite-literal field, a return, an
//     argument
//     (TestEveryMintPrimitiveCallerLivesInTheGuardsGraph);
//   - from each caller in the graph it walks UP the enclosing chain — the
//     functions that call it, then theirs, transitively — over a per-package
//     symbol table: top-level functions and methods, package-level variables,
//     and a registrar's own locals initialized with a function value;
//   - it demands the chain cross the gate: a registration wrapped in
//     gateDerivation(auth.MintTeamToken, …), or a chokepoint ask sitting ON
//     THE FLOW before the mint — preceding it in the source, held by no
//     branch the mint is outside of — where the chain stops;
//   - a chain crossing no gate fails, and a form the symbols cannot resolve
//     fails too: every name a registration's handler expression is built
//     from that the symbols cannot answer carries its own named error — one
//     resolvable name (requireAuth sits on every route) no longer eats the
//     judgment for the unread name carrying the handler — never a silence.
//
// The tables below (gatedRouteChokepointCodes, and the
// gatedBeneathTheRegistration table of
// TestTheChokepointTableMatchesTheRegistrations) stay the readable inventory
// a human audits; what makes them true is the enumeration from the
// primitives, not the table's own presence.
//
// The sibling classes keep their own gates and are out of this one: a
// personal access token is MintPAT (pat.MintToken, gated in
// handleCreatePAT), a WebSocket ticket MintWSTicket, a forge installation
// token is minted from the GitHub App's key rather than from the presented
// credential.

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
// guard closes: the handlers' own package, the orchestrator that mints two
// calls away from them, and the three primitives' own packages, so a hop
// THROUGH one of those packages is followed. TestEveryMintPrimitiveCaller-
// LivesInTheGuardsGraph is what keeps this list honest instead of assumed:
// a direct caller of a primitive anywhere else fails there, whatever kind of
// declaration holds it.
var mintGraphDirs = []string{"pkg/server", "pkg/forge", "pkg/webhooks", "pkg/configshare", "pkg/auth"}

// mintChokepointCalls are the calls a function's body makes to ASK the
// derivation chokepoint, each mapped to the mint it asks for — empty meaning
// "the mint is the call's last argument", which is how pkg/server spells its
// own question (checkDerivation(ctx, id, auth.MintTeamToken)).
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

// modulePath is the module's own import path. A handler name qualified by an
// import outside it is code this module does not own — a stdlib type
// conversion (`http.HandlerFunc(…)`) wraps a value, it fabricates nothing —
// while a name qualified by a module package this guard does not read is
// exactly the unread vehicle the fail-closed by name exists for.
const modulePath = "github.com/SocialGouv/iterion"

// teamTokenMintVerb is the mint this class's chains owe a question for. The
// sibling mints (MintPAT, MintWSTicket, …) keep their own gates.
const teamTokenMintVerb = "auth.MintTeamToken"

// mintCall is one use of a mint primitive a declaration makes — the call of
// it, or the reference holding it as a value — with the verb it spells.
type mintCall struct {
	ref  ast.Node
	verb string
}

// declaration is one resolvable symbol of a package: a top-level function or
// method, or a package-level variable, together with the node a call inside
// it reaches (a body, or a variable's initializer), the import aliases the
// file that declares it resolves package names by, and the facts every pass
// of this guard reads from it — the calls it makes, the mint primitives it
// reaches, the chokepoint questions it asks.
type declaration struct {
	name     string
	dir      string
	file     string
	value    ast.Node
	aliases  map[string]string
	isVar    bool
	calls    map[string]string
	mints    []mintCall
	teamAsks []*ast.CallExpr
}

// loadPackageDecls parses every non-test .go file of a module-relative
// directory into the symbol table this guard resolves names against: a
// top-level function or method keyed by its declared name, and every
// package-level variable keyed by its own name with the value it is
// initialized by. A handler held by a package-level var is as declared as
// one held by a func — a table without the vars resolved nothing the moment
// a mint moved into one.
func loadPackageDecls(t *testing.T, dir string) map[string][]*declaration {
	t.Helper()
	full := filepath.Join(moduleRoot, dir)
	entries, err := os.ReadDir(full)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string][]*declaration{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(full, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", dir, e.Name(), err)
		}
		aliases := importAliases(file)
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				out[d.Name.Name] = append(out[d.Name.Name], newDeclaration(d.Name.Name, dir, e.Name(), d.Body, aliases, false))
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
							out[name.Name] = append(out[name.Name], newDeclaration(name.Name, dir, e.Name(), vs.Values[i], aliases, true))
						}
					}
				}
			}
		}
	}
	return out
}

// newDeclaration reads what a declaration calls, which of those uses are
// mint primitives and which are chokepoint questions. A package-level
// variable initialized by a pure reference to another symbol records that
// symbol as an edge like a call: `var k = h` hands every caller of k the
// value h holds, so an alias of an alias carries the mint one hop out — the
// over-approximation erring toward the red.
func newDeclaration(name, dir, file string, value ast.Node, aliases map[string]string, isVar bool) *declaration {
	d := &declaration{name: name, dir: dir, file: file, value: value, aliases: aliases, isVar: isVar, calls: map[string]string{}}
	d.calls = nodeCalls(value)
	if isVar {
		switch v := value.(type) {
		case *ast.Ident:
			d.calls[v.Name] = ""
		case *ast.SelectorExpr:
			d.calls[v.Sel.Name] = ""
			if x, ok := v.X.(*ast.Ident); ok {
				d.calls[x.Name+"."+v.Sel.Name] = ""
			}
		}
	}
	d.mints = mintCallExprs(value, aliases)
	for call, mint := range askCallExprs(value) {
		if mint == teamTokenMintVerb {
			d.teamAsks = append(d.teamAsks, call)
		}
	}
	return d
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

// spellings hands every free spelling a node makes to check: each identifier
// the code reads as a value or a callee, and each selector judged whole by
// its qualifier. A selector's Sel is a field name, not a free spelling — it
// is judged only as part of the selector itself — so `pat.MintToken` (the
// sibling PAT primitive, a selector like any other) is never mistaken for
// the verb's bare spelling.
func spellings(n ast.Node, check func(ast.Expr)) {
	if n == nil {
		return
	}
	ast.Inspect(n, func(child ast.Node) bool {
		switch v := child.(type) {
		case *ast.SelectorExpr:
			check(v)
			spellings(v.X, check)
			return false
		case *ast.Ident:
			check(v)
			return false
		}
		return true
	})
}

// mintCallExprs answers EVERY use of a team-token mint primitive a node
// makes — the calls of it and the references holding it as a VALUE alike
// (`var alias = webhooks.MintToken`, a composite-literal field, a return, an
// argument) — resolved the way the source resolves it: a selector spelled
// through the file's own alias for the primitive's package, a bare spelling
// inside the primitive's home package, the bare spelling anywhere else (a
// dot-import qualifies no name, so the spelling is all there is), and — for
// the method verb, whose receiver no package name qualifies — the selector's
// name whoever carries it, the over-approximation a guard must err in.
func mintCallExprs(body ast.Node, aliases map[string]string) []mintCall {
	var out []mintCall
	spellings(body, func(e ast.Expr) {
		for _, verb := range teamTokenMintVerbs {
			if verbHits(verb, e, aliases) {
				out = append(out, mintCall{ref: e, verb: verb.call})
				break
			}
		}
	})
	return out
}

// verbHits answers whether a spelling is this verb — as the function part of
// a call or held as a value, the alias a mint hides behind being one
// spelling either way.
func verbHits(verb mintVerb, fun ast.Expr, aliases map[string]string) bool {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		if verb.pkgPath == "" {
			return f.Sel.Name == verb.call
		}
		x, ok := f.X.(*ast.Ident)
		if !ok {
			return false
		}
		return f.Sel.Name == verb.call[strings.Index(verb.call, ".")+1:] && aliases[x.Name] == verb.pkgPath
	case *ast.Ident:
		// The bare spelling, wherever it is written: the home package's own
		// reading of its primitive, or a dot-imported file's — which
		// qualifies no name, so the spelling is all it can be judged by.
		return verb.home != "" && f.Name == verb.bare
	}
	return false
}

// askCallExprs answers the chokepoint questions a node asks, each with the
// mint it names: checkDerivation names the mint in its last argument, the
// forge seam and the server's wrapper carry the one mint they ask for.
func askCallExprs(body ast.Node) map[*ast.CallExpr]string {
	out := map[*ast.CallExpr]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		mint := ""
		for _, name := range calleeNames(call.Fun) {
			asks, ok := chokepointCall(name)
			if !ok {
				continue
			}
			if asks != "" {
				mint = asks
				continue
			}
			if len(call.Args) > 0 {
				mint = exprText(call.Args[len(call.Args)-1])
			}
		}
		if mint != "" {
			out[call] = mint
		}
		return true
	})
	return out
}

// parentsIn maps every node of a body to its parent: the chain the flow
// check walks to see what holds an ask.
func parentsIn(body ast.Node) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	return parents
}

// branchHeld answers whether child runs only when a branch of parent is
// taken: the edge an ask must NOT sit behind unless the mint sits behind it
// too. An if-statement's init and condition, a switch's tag, a block's list
// run unconditionally once control reaches the statement; a branch's body
// does not — and neither does a function literal's body, a defer's or a
// go's call, which fire when the closure runs, not where they are written.
func branchHeld(parent, child ast.Node) bool {
	switch p := parent.(type) {
	case *ast.IfStmt:
		return child == p.Body || child == p.Else
	case *ast.SwitchStmt:
		return child == p.Body
	case *ast.TypeSwitchStmt:
		return child == p.Body
	case *ast.SelectStmt:
		return child == p.Body
	case *ast.ForStmt:
		return child == p.Body
	case *ast.RangeStmt:
		return child == p.Body
	case *ast.FuncLit:
		return child == p.Body
	case *ast.DeferStmt:
		return child == p.Call
	case *ast.GoStmt:
		return child == p.Call
	}
	return false
}

// isAncestor answers whether ancestor holds n in the body's parent chain.
func isAncestor(parents map[ast.Node]ast.Node, ancestor, n ast.Node) bool {
	for p := parents[n]; p != nil; p = parents[p] {
		if p == ancestor {
			return true
		}
	}
	return false
}

// asksBeforeItsMint answers whether EVERY mint of the body has an ask on
// ITS flow: one preceding it in the source, with every branch that holds
// the ask holding the mint too. The ask in
// `if err := o.approveTeamTokenMint(ctx); err != nil { … }` followed by the
// mint at the same level qualifies — the question is put before the mint on
// every path that reaches it. The same ask wrapped in `if impossible { … }`
// with the mint after the braces does not. And one covered pair covers
// itself alone: a body of two mints is terminal only when each has its own
// covering ask — read per DECLARATION, the first covered pair once ended
// the walk, and every other mint of the body rode without a question under
// a green guard.
func asksBeforeItsMint(body ast.Node, asks []*ast.CallExpr, mints []ast.Node) bool {
	if len(asks) == 0 || len(mints) == 0 {
		return false
	}
	parents := parentsIn(body)
	covered := map[ast.Node]bool{}
	for _, ask := range asks {
		var held []ast.Node
		for child, parent := ast.Node(ask), parents[ask]; parent != nil && parent != body; child, parent = parent, parents[parent] {
			if branchHeld(parent, child) {
				held = append(held, parent)
			}
		}
		for _, mint := range mints {
			if covered[mint] || mint.Pos() <= ask.Pos() {
				continue
			}
			covers := true
			for _, h := range held {
				if !isAncestor(parents, h, mint) {
					covers = false
					break
				}
			}
			covered[mint] = covers
		}
	}
	for _, mint := range mints {
		if !covered[mint] {
			return false
		}
	}
	return true
}

// mintGraph is what the guard resolves names against: the symbol tables of
// the directories whose chains it closes, and the caller edges between
// their declarations, keyed by every name a callee is known by — the
// over-approximation that walks a chain onto more callers, never fewer.
type mintGraph struct {
	decls   map[string]map[string][]*declaration
	callers map[string][]*declaration
}

// loadMintGraph loads the symbol tables and indexes who calls whom.
func loadMintGraph(t *testing.T) *mintGraph {
	t.Helper()
	g := &mintGraph{decls: map[string]map[string][]*declaration{}, callers: map[string][]*declaration{}}
	for _, dir := range mintGraphDirs {
		g.decls[dir] = loadPackageDecls(t, dir)
	}
	for _, dir := range mintGraphDirs {
		for _, list := range g.decls[dir] {
			for _, d := range list {
				for name := range d.calls {
					g.callers[name] = append(g.callers[name], d)
				}
			}
		}
	}
	if len(g.callers) == 0 {
		t.Fatal("the symbol table resolved no calls at all: the guard is reading nothing")
	}
	return g
}

// seed is a direct caller of a mint primitive found in the graph, with
// whether it asks the chokepoint on the flow that reaches its mint — where
// the chain stops and everything above it owes nothing.
type seed struct {
	decl     *declaration
	verb     string
	terminal bool
}

// seeds enumerates the graph's direct callers of the primitives.
func (g *mintGraph) seeds(t *testing.T) []seed {
	t.Helper()
	var out []seed
	for _, dir := range mintGraphDirs {
		for _, list := range g.decls[dir] {
			for _, d := range list {
				if len(d.mints) == 0 {
					continue
				}
				mintNodes := make([]ast.Node, 0, len(d.mints))
				for _, mc := range d.mints {
					mintNodes = append(mintNodes, mc.ref)
				}
				out = append(out, seed{decl: d, verb: d.mints[0].verb, terminal: asksBeforeItsMint(d.value, d.teamAsks, mintNodes)})
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no caller of a mint primitive found in the graph directories: the guard is reading nothing")
	}
	return out
}

// reachingNames closes the seeds upward: a declaration joins when its body
// calls a name already reaching — the functions that reach a mint through
// another function reach it themselves. The terminals are not in the set:
// the chain stops at them, which is what lets a re-provision that reuses
// its sealed secret through, and nothing above a terminal owes a gate.
func (g *mintGraph) reachingNames(seeds []seed) map[string]bool {
	reaching := map[string]bool{}
	for _, s := range seeds {
		if !s.terminal {
			reaching[s.decl.name] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for _, dir := range mintGraphDirs {
			for _, list := range g.decls[dir] {
				for _, d := range list {
					if reaching[d.name] {
						continue
					}
					for name := range d.calls {
						if reaching[name] {
							reaching[d.name] = true
							grew = true
							break
						}
					}
				}
			}
		}
	}
	return reaching
}

// resolveName answers the declarations a name in a registration's handler
// expression denotes: a symbol of the server package (a function, a method,
// a package-level var), or a local of the registrar followed to what
// initializes it — h := http.HandlerFunc(s.serveX) resolves to serveX. The
// walk is bounded by a seen set: a cycle is a form the guard refuses to
// guess through, and the names it cannot resolve are what the
// registration's fail-closed branch is for.
func (g *mintGraph) resolveName(dir, name string, locals map[string]ast.Expr) []*declaration {
	seen := map[string]bool{}
	var out []*declaration
	var walk func(name string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if list, ok := g.decls[dir][name]; ok {
			out = append(out, list...)
		}
		if expr, ok := locals[name]; ok && expr != nil {
			ast.Inspect(expr, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.Ident:
					walk(v.Name)
				case *ast.SelectorExpr:
					walk(v.Sel.Name)
				}
				return true
			})
		}
	}
	walk(name)
	return out
}

// resolveRegistration answers every declaration a registration's handler
// value resolves to: each spine name it is built from, through the server
// package's symbols and the registrar's own locals.
func (g *mintGraph) resolveRegistration(reg *routeRegistration) []*declaration {
	var out []*declaration
	seen := map[string]bool{}
	for _, rn := range reg.names {
		for _, d := range g.resolveName("pkg/server", rn.name, reg.locals) {
			if seen[d.dir+"/"+d.name] {
				continue
			}
			seen[d.dir+"/"+d.name] = true
			out = append(out, d)
		}
	}
	return out
}

// regName is one name a registration's handler value is built from, with
// what the fail-closed needs to judge it: the import path qualifying it (a
// module package is a vehicle that can carry the mint out of what this guard
// read; a stdlib package is a type conversion wrapping a value, which
// fabricates nothing), where on the spine the name sits (the handler itself,
// the wrapper producing or transforming it, or an argument handed to a
// wrapper), how many selector hops carry it (a field read is construction
// input, not code on the flow), and the wrapper it is handed to (a parameter
// handed to a wrapper this guard read is that wrapper's judgment, not the
// parameter's).
type regName struct {
	name    string
	pkgPath string
	pos     string
	chain   int
	carrier string
}

// spine positions.
const (
	atTop = "top"
	atFun = "fun"
	atArg = "arg"
)

// predeclaredNames are the identifiers no declaration ever resolves to and
// no value ever rides: the boolean and nil literals and the builtin
// functions. They name no code, so they carry no mint.
var predeclaredNames = map[string]bool{
	"true": true, "false": true, "nil": true, "iota": true,
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

// routeRegistration is one s.mux.Handle("METHOD /pattern", …) of pkg/server:
// every name its handler VALUE is built from — the spine the value flows
// through, never the statements of an inline body (judged by the inline
// pass over its calls) and never the gate's mint argument (the gate's own
// currency, not handler code) — the function literals it holds inline, the
// mint its gate names (empty when no gateDerivation wraps it), the file's
// own import aliases, and the registrar's own resolution context — the
// locals it assigns, the parameters it takes, whether it is itself a mux
// forwarder.
type routeRegistration struct {
	key       string
	names     []regName
	literals  []ast.Node
	gateMint  string
	file      string
	enclosing string
	aliases   map[string]string
	locals    map[string]ast.Expr
	params    map[string]bool
	forwarder bool
}

// spineNames walks the handler expression the way the handler VALUE flows:
// the expression itself, the function of each wrapper call, every argument
// the value passes through. It never walks into a function literal — the
// body is judged by the inline pass — and never into the gate's mint
// argument, which names the gate's currency, not handler code, and never
// past a selector's own qualifier: the hops of a selector chain are fields
// and receivers, not free code the guard must answer. Each name carries the
// import path qualifying it, where it sits, and the wrapper it is handed
// to, so the fail-closed can tell a stdlib conversion from a module
// vehicle, a field read from a called function, a parameter flowing into
// unread code from one a read wrapper judges.
func spineNames(e ast.Expr, aliases map[string]string) ([]regName, string) {
	var out []regName
	seen := map[regName]bool{}
	add := func(n regName) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	// chainDepth counts the selector hops a name rides: one for
	// `s.serveX`, two for `s.cfg.PublicURL` — the second hop and beyond are
	// fields of fields, construction input rather than code.
	chainDepth := func(x ast.Expr) int {
		n := 1
		for {
			sel, ok := x.(*ast.SelectorExpr)
			if !ok {
				return n
			}
			n++
			x = sel.X
		}
	}
	gate := ""
	var walk func(e ast.Expr, pos, carrier string)
	walk = func(e ast.Expr, pos, carrier string) {
		switch v := e.(type) {
		case *ast.FuncLit:
			return
		case *ast.Ident:
			add(regName{name: v.Name, pos: pos, carrier: carrier})
		case *ast.SelectorExpr:
			pkg := ""
			if x, ok := v.X.(*ast.Ident); ok {
				pkg = aliases[x.Name]
			}
			add(regName{name: v.Sel.Name, pkgPath: pkg, pos: pos, chain: chainDepth(v.X), carrier: carrier})
		case *ast.CallExpr:
			carrier := carrier
			switch f := v.Fun.(type) {
			case *ast.Ident:
				carrier = f.Name
			case *ast.SelectorExpr:
				carrier = f.Sel.Name
			}
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "gateDerivation" && len(v.Args) == 2 {
				gate = exprText(v.Args[0])
				walk(v.Fun, atFun, carrier)
				walk(v.Args[1], atArg, carrier)
				return
			}
			walk(v.Fun, atFun, carrier)
			for _, a := range v.Args {
				walk(a, atArg, carrier)
			}
		}
	}
	walk(e, atTop, "")
	return out, gate
}

// parsedRouteRegistrations reads every Handle / HandleFunc call of
// pkg/server taking two arguments. It does NOT require the pattern to be a
// string literal (studio_legacy_redirect.go builds its patterns) and it does
// NOT filter the handler's names: TestEveryTeamTokenMintSitsBehindThe-
// Chokepoint judges whatever resolves, and fails on a registration where
// nothing does.
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
		aliases := importAliases(file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			params := map[string]bool{}
			if fn.Type != nil && fn.Type.Params != nil {
				for _, f := range fn.Type.Params.List {
					for _, n := range f.Names {
						params[n.Name] = true
					}
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
					return true
				}
				reg := routeRegistration{
					key:       patternText(call.Args[0]),
					file:      e.Name(),
					enclosing: fn.Name.Name,
					aliases:   aliases,
					locals:    registrarLocals(fn),
					params:    params,
					// A registrar named Handle/HandleFunc is the mux
					// forwarder itself (recordingMux): its handlers are
					// read at ITS call sites, which are registrations this
					// reader sees.
					forwarder: fn.Name.Name == "Handle" || fn.Name.Name == "HandleFunc",
				}
				reg.names, reg.gateMint = spineNames(call.Args[1], aliases)
				ast.Inspect(call.Args[1], func(inner ast.Node) bool {
					if lit, ok := inner.(*ast.FuncLit); ok {
						reg.literals = append(reg.literals, lit.Body)
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

// registrarLocals answers the names a registrar ASSIGNS, with the value each
// is initialized by: a local holding a function value
// (h := http.HandlerFunc(s.serveX), h := studioLegacyRedirect()) is a
// declaration the registration can be resolved through. A PARAMETER is
// deliberately absent: a handler that reaches its registration only through
// a registrar's parameter is read by nobody — the registrar's call site is
// not a registration this reader sees — and a parameter-carried handler
// fails closed unless the registrar is the forwarder itself.
func registrarLocals(fn *ast.FuncDecl) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	if fn == nil || fn.Body == nil {
		return out
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if v.Tok != token.DEFINE && v.Tok != token.ASSIGN {
				return true
			}
			for i, lhs := range v.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name == "_" {
					continue
				}
				if v.Tok == token.DEFINE && i < len(v.Rhs) {
					out[id.Name] = v.Rhs[i]
				} else {
					// Reassigned or valueless: the name resolves, nothing
					// about the value it carries.
					out[id.Name] = nil
				}
			}
		case *ast.ValueSpec:
			for i, id := range v.Names {
				if i < len(v.Values) {
					out[id.Name] = v.Values[i]
				} else {
					out[id.Name] = nil
				}
			}
		case *ast.RangeStmt:
			if id, ok := v.Key.(*ast.Ident); ok && id.Name != "_" {
				out[id.Name] = nil
			}
			if id, ok := v.Value.(*ast.Ident); ok && id.Name != "_" {
				out[id.Name] = nil
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

// TestEveryTeamTokenMintSitsBehindTheChokepoint is the class guard of
// #2687. It enumerates the users of the mint primitives — a call of one or a
// reference holding it as a value — walks UP every chain from a mint,
// resolving each identifier to the declaration that defines it, and demands
// the chain cross the gate: a gateDerivation(auth.MintTeamToken, …)
// registration, or a chokepoint ask on the flow before the mint, where the
// chain stops. A chain that crosses no gate fails, and every name a
// registration's handler is built from that the symbols cannot answer fails
// on its own — "this registration cannot be judged" is a named error per
// unread name, not a silence one resolvable name can eat.
func TestEveryTeamTokenMintSitsBehindTheChokepoint(t *testing.T) {
	g := loadMintGraph(t)
	seeds := g.seeds(t)
	reaching := g.reachingNames(seeds)
	registrations := parsedRouteRegistrations(t)

	// The chains that stop at the mint itself: the question is put where the
	// mint is decided, so everything above them owes nothing.
	terminals := map[string]string{}
	terminalNames := []string{}
	for _, s := range seeds {
		if s.terminal {
			terminals[s.decl.dir+"/"+s.decl.name] = s.verb
			terminalNames = append(terminalNames, s.decl.dir+"/"+s.decl.name)
		}
	}
	if len(terminals) == 0 {
		t.Error("no declaration asks the chokepoint on the flow that reaches its own mint: the exemption that lets a re-provision reusing its sealed secret through is gone")
	}

	// Walk the registrations: every one that resolves to a mint-reaching
	// declaration must carry the gate at its registration, hold the ask in
	// an inline body, or wrap a chain a terminal stops.
	gatedBy := map[string]bool{}
	var judged []string
	for i := range registrations {
		reg := &registrations[i]
		for _, d := range g.resolveRegistration(reg) {
			if !reaching[d.name] {
				continue
			}
			judged = append(judged, d.dir+"/"+d.name+" ← "+reg.key)
			if reg.gateMint == teamTokenMintVerb {
				gatedBy[d.name] = true
				continue
			}
			t.Errorf("%q (%s) resolves to %s declared in %s, which reaches a team-token mint, through gate %q: register it through gateDerivation(%s, …) or ask checkDerivation on the flow before the mint",
				reg.key, reg.file, d.dir+"/"+d.name, d.dir, reg.gateMint, teamTokenMintVerb)
		}
		for _, body := range reg.literals {
			calls := nodeCalls(body)
			// A body binding the primitive to a local name mints with no
			// call of the verb's own spelling in its calls: the value
			// reference is the mint the same way a call is.
			spelled := ""
			var mintRefs []ast.Node
			for _, mc := range mintCallExprs(body, reg.aliases) {
				spelled = mc.verb
				mintRefs = append(mintRefs, mc.ref)
			}
			// The ask must sit ON THE FLOW that reaches the body's mints —
			// the very test the seeds answer to. Read by PRESENCE over the
			// body's calls, one question anywhere — written after the mint,
			// in a branch the mint does not ride — whitewashed the whole
			// body, and a mint ran with no question asked under a green
			// guard.
			teamAsks := []*ast.CallExpr{}
			for call, mint := range askCallExprs(body) {
				if mint == teamTokenMintVerb {
					teamAsks = append(teamAsks, call)
				}
			}
			askedOnTheFlow := asksBeforeItsMint(body, teamAsks, mintRefs)
			for call := range calls {
				reaches := reaching[call]
				if !reaches && verbNamed(call) != "" {
					reaches = true
				}
				if !reaches {
					continue
				}
				judged = append(judged, reg.key+" (inline)")
				if reg.gateMint == teamTokenMintVerb || askedOnTheFlow {
					continue
				}
				t.Errorf("%q (%s) registers an inline handler whose body reaches a team-token mint through %q with gate %q",
					reg.key, reg.file, call, reg.gateMint)
			}
			if spelled != "" {
				judged = append(judged, reg.key+" (inline)")
				if reg.gateMint == teamTokenMintVerb || askedOnTheFlow {
					continue
				}
				t.Errorf("%q (%s) registers an inline handler whose body holds a team-token mint primitive as a value (%s) with gate %q",
					reg.key, reg.file, spelled, reg.gateMint)
			}
		}
		// Fail closed BY NAME: every name the handler value flows through
		// that the symbols cannot answer carries its own named error. One
		// resolvable name (requireAuth sits on every route) no longer eats
		// the judgment for the unread name carrying the handler — that
		// silence is how a mint passed unseen. What is not a vehicle for
		// code is not judged: a predeclared literal names nothing, a stdlib
		// conversion fabricates nothing, a field two hops from its receiver
		// is construction input to the wrapper it is handed to, and a
		// parameter handed to a wrapper this guard READ is that wrapper's
		// judgment. A handler carried by the registrar's PARAMETER into no
		// wrapper, or into an unread one, is its own failure — the real
		// registration is not one this reader sees unless the registrar is
		// the forwarder itself.
		for _, rn := range reg.names {
			if predeclaredNames[rn.name] {
				continue
			}
			if rn.pkgPath != "" && !strings.HasPrefix(rn.pkgPath, modulePath) {
				continue
			}
			if reg.params[rn.name] {
				// Carried into no wrapper (the registration's value IS the
				// parameter), or into a wrapper this guard cannot read: the
				// real registration is not one this reader saw.
				carriedUnread := rn.pos == atTop || len(g.resolveName("pkg/server", rn.carrier, reg.locals)) == 0
				if !reg.forwarder && carriedUnread {
					t.Errorf("%q (%s): its handler is carried by the parameter %q of registrar %s, which is no Handle/HandleFunc forwarder — the real registration is not one this guard read, which is how a mint passes unseen",
						reg.key, reg.file, rn.name, reg.enclosing)
				}
				continue
			}
			if len(g.resolveName("pkg/server", rn.name, reg.locals)) > 0 {
				continue
			}
			// An ARGUMENT handed to a wrapper is construction input when it
			// is a package constant or a field read (`webhooks.ProviderFor-
			// gejo`, `s.cfg.PublicURL`): a constant and a field fabricate
			// nothing, and the package they come from is the exact pass's
			// business. A bare identifier argument is a value the handler
			// may BE — it is judged.
			if rn.pos == atArg && (rn.chain > 1 || rn.pkgPath != "") {
				continue
			}
			qualified := ""
			if rn.pkgPath != "" {
				qualified = ", qualified by " + rn.pkgPath
			}
			t.Errorf("%q (%s): its handler expression names %q%s, which resolves to no declaration this guard read and is not built by its registrar %s — this registration cannot be judged, which is how a mint passes unseen",
				reg.key, reg.file, rn.name, qualified, reg.enclosing)
		}
	}

	// Every mint-reaching declaration must be answered by the gate: carry a
	// gateDerivation registration itself, or sit below one — below a
	// terminal, whose own ask stops the chain. A reaching declaration
	// neither gated nor carried answers to nobody: the mint it reaches
	// would run for whatever calls it.
	answered := map[string]bool{}
	for name := range gatedBy {
		answered[name] = true
	}
	for grew := true; grew; {
		grew = false
		for name := range reaching {
			if answered[name] {
				continue
			}
			for _, caller := range g.callers[name] {
				if answered[caller.name] {
					answered[name] = true
					grew = true
					break
				}
			}
		}
	}
	var orphans []string
	for name := range reaching {
		if !answered[name] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		t.Errorf("%s reaches a team-token mint yet no gateDerivation(%s, …) registration resolves to it and no chokepoint ask stops its chain first: this chain from the mint crosses no gate", name, teamTokenMintVerb)
	}

	if len(judged) == 0 {
		t.Fatal("no registered mint-reaching handler found: the guard is reading nothing")
	}
	sort.Strings(judged)
	sort.Strings(terminalNames)
	t.Logf("mint-reaching chains answered by a gate: %d registrations judged over %d reaching declarations", len(judged), len(reaching))
	t.Logf("chains stopped at the mint itself: %v", terminalNames)
}

// TestEveryMintPrimitiveCallerLivesInTheGuardsGraph is what makes
// mintGraphDirs a fact rather than an assumption: an EXACT pass over every
// non-vendor directory of the module looks for direct users of the mint
// primitives — resolved through each file's own imports, so `pat.MintToken`
// is not mistaken for `webhooks.MintToken` — across whole function
// declarations AND package-level variables, a primitive either called or
// HELD AS A VALUE (an initializer, a composite-literal field, an argument),
// and fails when one lives outside the directories the call graph closes. A
// mint reached through a third package is then either followed or reported,
// never unseen — held by a function or held by a var alike.
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
		for _, decl := range file.Decls {
			var bodies []ast.Node
			var holders []string
			switch dd := decl.(type) {
			case *ast.FuncDecl:
				if dd.Body == nil {
					continue
				}
				bodies = append(bodies, dd.Body)
				holders = append(holders, dd.Name.Name)
			case *ast.GenDecl:
				if dd.Tok != token.VAR {
					continue
				}
				for _, spec := range dd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, val := range vs.Values {
						holder := vs.Names[0].Name
						if i < len(vs.Names) {
							holder = vs.Names[i].Name
						}
						bodies = append(bodies, val)
						holders = append(holders, holder)
					}
				}
			}
			for i, body := range bodies {
				for _, mc := range mintCallExprs(body, aliases) {
					where := rel + "/" + filepath.Base(p) + ":" + holders[i] + " → " + mc.verb
					if covered[rel] {
						inside = append(inside, where)
					} else {
						outside = append(outside, where)
					}
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
