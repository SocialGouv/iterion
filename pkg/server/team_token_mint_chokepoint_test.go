package server

import (
	"context"
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

	"github.com/SocialGouv/iterion/pkg/auth"
)

// The class the derivation chokepoint closes is no longer defined by a shape
// a handler may hide a mint behind: the verdict travels in the request's
// context, and the MINT PRIMITIVES demand it. A team-scoped self-
// authenticating token exists because one of three primitives fabricated it —
// webhooks.MintToken, configshare.MintToken, auth.Service.CreateInvitation —
// and the forge's hook-identity reservation reaches the first of them. Each
// primitive refuses, fail closed, a call whose context carries no derivation
// verdict for auth.MintTeamToken (auth.RequireMintVerdict in its own body):
// whatever calls it — a handler, a branch arm, a value alias, a local
// binding, a helper — the mint answers to the chokepoint, because the gate
// stamped the verdict (gateDerivation, and the forge orchestrator's
// ApproveTeamTokenMint seam) or the call never mints.
//
// Five guards before this one tried to RECOGNISE the shapes a mint hides
// behind and were hunted past, one form at a time — a name filter, a
// presence check, a flow check, a binding the walker did not resolve, a
// branch arm it held by the wrong node. What remains here is the residue
// the construction cannot carry:
//
//   - the demand itself, read in each primitive's own body, per the table
//     below — a context parameter and auth.RequireMintVerdict, nothing else
//     (TestEveryTeamTokenMintPrimitiveDemandsTheVerdict);
//   - the stamp held to the gate: the verdict's whole currency — its two
//     constructors, the read that takes it out of a context, a MintVerdict
//     literal, the private context key by hand — is inventoried by SYMBOL,
//     wherever the module's source reaches it (a dot-import of pkg/auth
//     spells the constructors bare, and is its own refusal); in production
//     code that currency lives only in the verdict's printer
//     (pkg/auth/mint_context.go) and the gate (derivation_gate.go) —
//     anything else is a trapdoor named by symbol
//     (TestEveryTeamTokenMintVerdictStamperIsTheGateOrATest);
//   - the forge's ask where the mint is decided, at the one reservation
//     line that mints (TestTheForgeReservesItsHookIdentityBehindTheApproval),
//     and the seam wired wherever an orchestrator is built
//     (TestTheForgeOrchestratorIsBuiltWithTheMintChokepoint);
//   - the caller inventory, the belt behind all of it: every user of a mint
//     primitive lives in the directories this file reads
//     (TestEveryMintPrimitiveCallerLivesInTheGuardsGraph);
//   - the OpenAPI table confronted with the registrations
//     (TestTheChokepointTableMatchesTheRegistrations).
//
// The behaviour itself is witnessed at the routes
// (team_token_mint_gate_test.go: the six gated routes answer 401/403 and a
// standing session still mints) and at the primitives (each package's own
// probe: a bare context refuses, a verdict for another mint approves
// nothing, the forge refuses with no seam wired).
//
// The sibling classes keep their own gates and are out of this one: a
// personal access token is MintPAT (pat.MintToken, gated in
// handleCreatePAT), a WebSocket ticket MintWSTicket, a forge installation
// token is minted from the GitHub App's key rather than from the presented
// credential.

// mintPrimitive is one primitive that fabricates a team token, with where it
// is defined: the readable inventory of the DEMANDING side of the class.
type mintPrimitive struct {
	// verb is how a caller spells it.
	verb string
	// dir is the module-relative directory of the defining package.
	dir string
	// recv is the receiver's type for a method primitive, empty for a free
	// function.
	recv string
	// name is the declared function name.
	name string
}

var teamTokenMintPrimitives = []mintPrimitive{
	{verb: "webhooks.MintToken", dir: "pkg/webhooks", name: "MintToken"},
	{verb: "configshare.MintToken", dir: "pkg/configshare", name: "MintToken"},
	{verb: "auth.Service.CreateInvitation", dir: "pkg/auth", recv: "*Service", name: "CreateInvitation"},
}

// requireMintVerdict is the call whose presence in a primitive's body IS the
// demand: the context must carry the chokepoint's verdict, or the mint is
// refused.
const requireMintVerdict = "RequireMintVerdict"

// mintGraphDirs are the module-relative directories whose callers of the
// primitives the belt reads: the handlers' own package, the orchestrator
// that mints two calls away from them, and the three primitives' own
// packages. TestEveryMintPrimitiveCallerLivesInTheGuardsGraph is what keeps
// this list honest instead of assumed: a direct caller of a primitive
// anywhere else fails there.
var mintGraphDirs = []string{"pkg/server", "pkg/forge", "pkg/webhooks", "pkg/configshare", "pkg/auth"}

// derivationGateFile is the one production file a verdict may be stamped
// from: the gate that asks the chokepoint, and the forge seam's server side,
// live there.
const derivationGateFile = "pkg/server/derivation_gate.go"

// mintContextFile is the verdict's printer: the file that defines
// MintVerdict, its private key and its constructors. The currency exists
// there by construction — NewMintVerdict is its only issuer,
// ContextWithMintVerdict its only stamp — and the demand the primitives make
// (RequireMintVerdict) reads there. The inventory exempts it beside the
// gate, and nowhere else.
const mintContextFile = "pkg/auth/mint_context.go"

// mintVerdictUses is the currency a function name spells, with the named
// refusal each reference to it carries. A composite literal of MintVerdict
// and the private key mintVerdictKey are inventoried beside them: the
// fields and the key are unexported, so every site of both lives in
// pkg/auth, where the bare names are all there is to spell.
var mintVerdictUses = map[string]string{
	"NewMintVerdict":         "stamps a verdict by auth.NewMintVerdict",
	"ContextWithMintVerdict": "stamps a verdict by auth.ContextWithMintVerdict",
	"MintVerdictFromContext": "reads a verdict from a context by auth.MintVerdictFromContext",
}

// moduleRoot is the module's directory as seen from pkg/server, where these
// tests run.
const moduleRoot = "../.."

// modulePath is the module's own import path, which an alias must resolve a
// verdict stamper's package by.
const modulePath = "github.com/SocialGouv/iterion"

// mintVerb is one primitive that fabricates a team token, in the two
// spellings source can reach it by.
type mintVerb struct {
	// call is how a file outside home spells it ("webhooks.MintToken"), and
	// for the method verb the selector's own name, whoever the receiver is.
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

// mintCall is one use of a mint primitive a declaration makes — the call of
// it, or the reference holding it as a value — with the verb it spells.
type mintCall struct {
	ref  ast.Node
	verb string
}

// mintCallExprs answers EVERY use of a team-token mint primitive a node
// makes — the calls of it and the references holding it as a VALUE
// (`var alias = webhooks.MintToken`, a composite-literal field, a return, an
// argument) — resolved the way the source resolves it: a selector spelled
// through the file's own alias for the primitive's package, a bare spelling
// inside the primitive's home package, the bare spelling anywhere else (a
// dot-import qualifies no name, so the spelling is all there is).
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

// exprText renders an expression the way the source spells it, for the few
// shapes this file compares ("auth.MintTeamToken", "*Service").
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprText(v.X)
	}
	return ""
}

// parsedFile is one parsed non-test source file, with the file set its
// positions are read by.
type parsedFile struct {
	name string
	fset *token.FileSet
	file *ast.File
}

// parseNonTestFiles parses every non-test .go file of a module-relative
// directory, in file-name order.
func parseNonTestFiles(t *testing.T, dir string) []parsedFile {
	t.Helper()
	full := filepath.Join(moduleRoot, dir)
	entries, err := os.ReadDir(full)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []parsedFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, filepath.Join(full, e.Name()), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s/%s: %v", dir, e.Name(), perr)
		}
		out = append(out, parsedFile{name: e.Name(), fset: fset, file: file})
	}
	return out
}

// TestEveryTeamTokenMintPrimitiveDemandsTheVerdict reads the class from its
// demanding side: every primitive of the table above takes a context and
// carries the demand in its own body — a call to auth.RequireMintVerdict —
// so a mint without the chokepoint's verdict refuses WHATEVER calls it. This
// is the whole residue of the shape-chasing guards this file once carried:
// there is no caller form left to recognise, because the form that matters
// is the primitive's own argument list.
func TestEveryTeamTokenMintPrimitiveDemandsTheVerdict(t *testing.T) {
	for _, p := range teamTokenMintPrimitives {
		p := p
		found := false
		for _, pf := range parseNonTestFiles(t, p.dir) {
			for _, decl := range pf.file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != p.name || fn.Body == nil {
					continue
				}
				if (p.recv == "") != (fn.Recv == nil) {
					continue
				}
				if p.recv != "" && exprText(fn.Recv.List[0].Type) != p.recv {
					continue
				}
				found = true
				if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 || exprText(fn.Type.Params.List[0].Type) != "context.Context" {
					t.Errorf("%s does not take a context.Context first: the verdict travels in the context, so there is nothing to demand", p.verb)
					continue
				}
				demands := false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == requireMintVerdict {
						demands = true
						return false
					}
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == requireMintVerdict {
						demands = true
						return false
					}
					return true
				})
				if !demands {
					t.Errorf("%s mints without demanding the derivation verdict: its body must call auth.%s and refuse on its error — a call whose context carries no verdict for auth.MintTeamToken is the class, fail closed", p.verb, requireMintVerdict)
				}
			}
		}
		if !found {
			t.Fatalf("%s is declared nowhere in %s: the table names a primitive that is gone — re-read the class before editing this file", p.verb, p.dir)
		}
	}
}

// TestEveryTeamTokenMintVerdictStamperIsTheGateOrATest holds the verdict's
// currency to its two legitimate holders: the printer that defines it
// (pkg/auth/mint_context.go) and the gate that asks before it stamps
// (derivation_gate.go, the forge seam's server side included). The inventory
// matches SYMBOLS, not spellings — every way production source can reach the
// currency carries its own named refusal:
//
//   - auth.NewMintVerdict / auth.ContextWithMintVerdict, through ANY alias of
//     pkg/auth (all of them, judged as a union in a stable order — a file
//     importing the package twice never leaves the verdict to the import
//     map's iteration);
//   - the same names bare, inside pkg/auth itself or in a file that
//     dot-imports it (a dot-import qualifies no name, so the bare spelling
//     is all such a file can reach the constructors by — and the dot-import
//     is its own refusal);
//   - the read, auth.MintVerdictFromContext — outside the printer it bypasses
//     the demand the primitives make (RequireMintVerdict's typed refusal);
//   - a MintVerdict{…} literal, bare in pkg/auth (the unexported fields make
//     it legal only where the package owns them) or qualified anywhere (the
//     zero value, which approves nothing, refused all the same);
//   - the private context key mintVerdictKey handled by hand inside pkg/auth
//     — With it in a bare context.WithValue, or reading it straight off a
//     context — which mints the currency through neither constructor.
//
// A currency site anywhere else is a door around the gate and fails here by
// name. Tests stamp through the same constructor (testMintVerdictContext
// below), so a test mints under exactly the context a gated handler runs
// under — that is the design, not a trapdoor: no production caller can reach
// a test's context.
func TestEveryTeamTokenMintVerdictStamperIsTheGateOrATest(t *testing.T) {
	var offenders []string
	allowed := 0
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
		rel, rerr := filepath.Rel(moduleRoot, filepath.Dir(p))
		if rerr != nil {
			return rerr
		}
		rel = path.Clean(filepath.ToSlash(rel))
		file, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", p, perr)
			return nil
		}
		for _, what := range verdictStampings(file, importAliases(file)) {
			where := rel + "/" + filepath.Base(p)
			switch where {
			case derivationGateFile:
				allowed++
			case mintContextFile:
				// The verdict's printer: the currency exists here by
				// construction.
			default:
				offenders = append(offenders, where+" "+what)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	if allowed == 0 {
		t.Fatalf("no verdict is stamped in %s: the gate no longer puts the chokepoint's verdict in the request's context, and every mint primitive refuses — reading nothing is not a pass", derivationGateFile)
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s: the verdict's currency outside its printer (%s) and the gate (%s) is a door around the gate — ask the chokepoint there instead, or make your case for the inventory", o, mintContextFile, derivationGateFile)
	}
}

// verdictStampings answers every production reference to the verdict's
// currency a file makes, each with its own named refusal. It matches SYMBOLS,
// not spellings:
//
//   - a selector through ANY alias of pkg/auth (all of them, in a stable
//     order, never the import map's last word: a file importing the package
//     twice is judged under both);
//   - the same names bare, inside pkg/auth itself or in a file that
//     dot-imports it;
//   - a MintVerdict{…} composite literal, bare in pkg/auth or dot-imported,
//     or qualified through an alias;
//   - the private context key mintVerdictKey, whose unexported type keeps
//     every site of it in pkg/auth.
//
// A declaration's own name is not a use of the currency: only bodies and
// initializers are read.
func verdictStampings(file *ast.File, aliases map[string]string) []string {
	var authAliases []string
	dotImportsAuth := false
	for alias, p := range aliases {
		if p != modulePath+"/pkg/auth" {
			continue
		}
		if alias == "." {
			dotImportsAuth = true
			continue
		}
		authAliases = append(authAliases, alias)
	}
	sort.Strings(authAliases)

	inAuth := file.Name.Name == "auth"
	reachesBare := inAuth || dotImportsAuth
	bareWhy := " (bare, in pkg/auth)"
	if dotImportsAuth {
		bareWhy = " (bare: this file dot-imports pkg/auth)"
	}

	var out []string
	refuse := func(what string) {
		out = append(out, what)
	}
	if dotImportsAuth {
		refuse(`dot-imports pkg/auth (".") — its verdict constructors spell bare, and a bare spelling is what the inventory must be able to name`)
	}
	var bodies []ast.Node
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				bodies = append(bodies, d.Body)
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, val := range vs.Values {
						bodies = append(bodies, val)
					}
				}
			}
		}
	}
	for _, body := range bodies {
		ast.Inspect(body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				switch t := v.Type.(type) {
				case *ast.Ident:
					if t.Name == "MintVerdict" && reachesBare {
						refuse("builds the verdict by literal (MintVerdict{…} — the fields are unexported, so the literal only mints where the package owns them)")
					}
				case *ast.SelectorExpr:
					if x, ok := t.X.(*ast.Ident); ok && t.Sel.Name == "MintVerdict" && holdsAlias(authAliases, x.Name) {
						refuse("builds the verdict by literal (auth.MintVerdict{…} — the zero value, which approves nothing, refused all the same)")
					}
				}
			case *ast.SelectorExpr:
				if x, ok := v.X.(*ast.Ident); ok && holdsAlias(authAliases, x.Name) {
					if what, ok := mintVerdictUses[v.Sel.Name]; ok {
						refuse(what)
					}
				}
			case *ast.Ident:
				if what, ok := mintVerdictUses[v.Name]; ok && reachesBare {
					refuse(what + bareWhy)
				}
				if inAuth && v.Name == "mintVerdictKey" {
					refuse("handles the verdict's private context key by hand (mintVerdictKey) — With it in a bare context.WithValue, or reading it off a context, mints the currency through neither constructor")
				}
			}
			return true
		})
	}
	return out
}

// holdsAlias answers whether name is one of the aliases a file reaches
// pkg/auth by.
func holdsAlias(aliases []string, name string) bool {
	for _, a := range aliases {
		if a == name {
			return true
		}
	}
	return false
}

// TestTheForgeReservesItsHookIdentityBehindTheApproval reads the forge's one
// mint the way the probes drive it: the reservation of a NEW hook identity
// asks the chokepoint (o.approveTeamTokenMint) before it mints
// (webhooks.MintToken), on the same body — the ask sits where the decision
// is taken, and the verdict it returns is the context the mint runs under.
// The behavioural side lives in pkg/forge's provision_mint_approval_test.go:
// no seam wired refuses, a refusing seam aborts with ErrMintNotApproved, and
// the granted verdict is what the mint runs under.
func TestTheForgeReservesItsHookIdentityBehindTheApproval(t *testing.T) {
	var asks, mints token.Position
	for _, pf := range parseNonTestFiles(t, "pkg/forge") {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "reserveHookIdentity" || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pos := pf.fset.Position(call.Pos())
				switch sel.Sel.Name {
				case "approveTeamTokenMint":
					asks = pos
				case "MintToken":
					mints = pos
				}
				return true
			})
		}
	}
	if !asks.IsValid() {
		t.Fatal("reserveHookIdentity no longer asks the chokepoint (o.approveTeamTokenMint): the forge mints a team token with nothing asked")
	}
	if !mints.IsValid() {
		t.Fatal("reserveHookIdentity no longer mints through webhooks.MintToken: the demanding primitive of the forge's hook identity is gone — re-read the class before editing this file")
	}
	if asks.Line >= mints.Line {
		t.Errorf("the chokepoint ask (line %d) does not precede the mint (line %d) in reserveHookIdentity", asks.Line, mints.Line)
	}
}

// routeRegistration is one s.mux.Handle("METHOD /pattern", …) of pkg/server,
// reduced to what the OpenAPI confrontation reads: the pattern, the file,
// and the mint its gateDerivation wrapper names (empty when none wraps it).
type routeRegistration struct {
	key      string
	file     string
	gateMint string
}

// gateMintOf answers the mint a registration's handler value is wrapped in
// when gateDerivation names one, wherever the wrapper sits on the spine the
// value flows through — s.requireAuth(s.gateDerivation(auth.MintTeamToken,
// …)) names it through the requireAuth around it.
func gateMintOf(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "gateDerivation" && len(call.Args) == 2 {
		return exprText(call.Args[0])
	}
	if m := gateMintOf(call.Fun); m != "" {
		return m
	}
	for _, a := range call.Args {
		if m := gateMintOf(a); m != "" {
			return m
		}
	}
	return ""
}

// parsedRouteRegistrations reads every Handle / HandleFunc call of
// pkg/server taking two arguments. It does NOT require the pattern to be a
// string literal (studio_legacy_redirect.go builds its patterns).
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
		file, perr := parser.ParseFile(fset, e.Name(), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", e.Name(), perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			out = append(out, routeRegistration{
				key:      patternText(call.Args[0]),
				file:     e.Name(),
				gateMint: gateMintOf(call.Args[1]),
			})
			return true
		})
	}
	if len(out) < 100 {
		t.Fatalf("only %d route registrations read: the guard is reading nothing", len(out))
	}
	return out
}

// patternText renders the route pattern for the message: the literal when it
// is one, the source's own spelling otherwise ("GET /"+seg), so a
// registration whose pattern is computed is still named.
func patternText(e ast.Expr) string {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if s, serr := strconv.Unquote(lit.Value); serr == nil {
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

// TestTheForgeOrchestratorIsBuiltWithTheMintChokepoint closes the one hole
// the verdict's construction cannot see from inside the forge: the approval
// asked at the mint is asked through an injected seam, and a seam left nil
// REFUSES (a server that cannot mint is down, not open) — so wherever this
// package builds a forge.Orchestrator, ApproveTeamTokenMint is wired.
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
				t.Errorf("%s builds a forge.Orchestrator with ApproveTeamTokenMint %q: the approval asked at the mint is a seam, and an unwired seam refuses every provisioning that mints",
					e.Name(), wired)
			}
			return true
		})
	}
	if built == 0 {
		t.Fatal("no forge.Orchestrator is built in this package: the guard is reading nothing")
	}
}

// TestEveryMintPrimitiveCallerLivesInTheGuardsGraph is the belt behind the
// construction: an EXACT pass over every non-vendor directory of the module
// looks for direct users of the mint primitives — resolved through each
// file's own imports, so `pat.MintToken` is not mistaken for
// `webhooks.MintToken` — across whole function declarations AND
// package-level variables, a primitive either called or HELD AS A VALUE (an
// initializer, a composite-literal field, an argument), and fails when one
// lives outside the directories the guards above read. A mint reached
// through a third package is then either read or reported, never unseen —
// held by a function or held by a var alike.
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
		t.Errorf("%s lives outside the directories the mint guards read (%v): a caller of a team-token mint primitive answers to the verdict the gate stamps, and this file cannot see it — add its directory to mintGraphDirs or move the caller", o, mintGraphDirs)
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

// testMintVerdictContext carries the verdict the derivation gate stamps
// after its ask grants — issued through the same constructor the gate uses
// (auth.NewMintVerdict), so a test mints under exactly the context a gated
// handler runs under. The mint primitives this package reaches
// (webhooks.MintToken, configshare.MintToken, auth.Service.CreateInvitation)
// refuse a context without one.
func testMintVerdictContext(ctx context.Context) context.Context {
	return auth.ContextWithMintVerdict(ctx, auth.NewMintVerdict(auth.MintTeamToken, "test"))
}
