package lease

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// specSite is one lease.Spec construction the sweep judged: where it is, the
// symbol that builds it, and whether it arms the handover.
type specSite struct {
	rel    string
	symbol string
	armed  bool
	where  string
}

// key is how the exception ledger names a site: the file AND the enclosing
// symbol. Keyed by the symbol rather than by the file, a renamed or moved
// constructor re-arms the guard instead of inheriting an exemption written
// for the code that used to be there.
//
// A site no symbol names falls back to its file and LINE rather than to the
// bare file, which every such site in that file would share: one exemption
// covering the next spec written beside it is the hole the symbol key exists
// to prevent. The symbol key has that hole one scale down — two specs built
// inside one function share it — so an entry covers exactly ONE site
// (judgeSpecSites counts what it matched), and a site may be put on the
// record under its file and LINE instead, which is how ONE of several specs
// a symbol builds carries a decision of its own.
func (s specSite) key() string {
	if s.symbol == "" {
		return s.where
	}
	return s.rel + "::" + s.symbol
}

// Every repeating net that campaigns on a lease has to arm the handover
// (lease.Spec.Health): a net whose term can only end on lease loss or
// shutdown is blackholed by one replica whose passes all fail, with the
// healthy ones standing by — #2685, measured at 60 failed passes and 0
// healthy passes over 20 TTLs.
//
// The ground truth is the MODULE's source, walked from the module root the
// test process resolves (internal/sweepwalk, never runtime.Caller — a
// cached package compiled in another checkout walks that other tree, #2916).
// The guard this replaces globbed ONE directory, pkg/server's, so a net wired
// anywhere else — pkg/cloudsched already builds one — was judged by nothing.
//
// An entry in the exception ledger is a decision on the record, not an
// exemption: it carries the reason, and the reason has to be written beside
// the spec too.
func TestEveryLeaseSpecInTheModuleArmsTheHandover(t *testing.T) {
	exempt := map[string]string{
		// TryRun serves ONE term for ONE schedule launch: no run of passes
		// to classify, and nothing a term could hand over to. The decision
		// is written beside the spec, in pkg/cloudsched.
		"pkg/cloudsched/exclusion.go::(*MongoStore).WithSchedule": "one TryRun term for one schedule launch",
	}

	sites := leaseSpecSitesInModule(t)
	// A sweep that sees nothing passes over anything: a refactor that moved
	// every spec out of the packages below, or a walk that stopped reaching
	// them, must redden here rather than report a clean tree.
	byPackage := map[string]int{}
	for _, s := range sites {
		byPackage[filepath.ToSlash(filepath.Dir(s.rel))]++
	}
	for _, want := range []string{"pkg/server", "pkg/cloudsched"} {
		if byPackage[want] == 0 {
			t.Errorf("the sweep judged no lease.Spec under %s — it did not reach that package, so every net wired there is judged by nothing (saw %v)", want, byPackage)
		}
	}
	if len(sites) < 4 {
		t.Fatalf("the sweep judged %d lease.Spec in the module (%v), want the nets it guards", len(sites), sites)
	}

	for _, complaint := range judgeSpecSites(sites, exempt) {
		t.Error(complaint)
	}
}

// judgeSpecSites answers every complaint the guard has about the sites it
// found: one per net whose handover is neither armed nor on the record, and
// one per ledger entry that does not cover exactly one site. A pure function
// over the sites, so the ORDER its rules apply in is witnessed on planted
// sites rather than on whatever the tree happens to hold today.
//
// What a site is JUDGED on is whether it arms the handover; the symbol is
// only how a decision gets on the record. So an armed net is never an
// offender, attributable or not — a guard that demanded the ledger key first
// accused a net satisfying the rule in full. That question is asked BEFORE
// the ledger: an entry naming a site somebody has since armed matches
// nothing and is reported stale, which is the right answer — the decision it
// records is no longer needed — and it keeps an armed neighbour out of the
// count below.
func judgeSpecSites(sites []specSite, exempt map[string]string) []string {
	var complaints []string
	seen := map[string]int{}
	for _, s := range sites {
		if s.armed {
			continue
		}
		if key := exemptKeyOf(s, exempt); key != "" {
			seen[key]++
			continue
		}
		who := s.symbol + " builds"
		if s.symbol == "" {
			who = "no symbol the sweep can name builds"
		}
		complaints = append(complaints, fmt.Sprintf("%s: %s a lease.Spec with no Health — this net's term cannot end on the work failing, so one replica whose egress is broken keeps renewing while every pass fails and no peer covers (#2685). Arm it, or put the decision on the record in this test's exempt map under %q AND beside the spec.", s.where, who, s.key()))
	}
	// A ledger entry covers ONE site, and its matches are counted: matching
	// none is the hole the symbol key exists to prevent seen from one side
	// — the site it named moved, was renamed or got armed, and the sweep
	// carries an exemption for code that is no longer there — while
	// matching SEVERAL is the same hole from the other: two unarmed specs
	// one symbol builds share its key, so the second would inherit the
	// first's reason without a word, in a gate whose whole job is to redden
	// on an unarmed net. The count has to match exactly, which is the rule
	// the sibling construction sweeps already apply (pkg/backend/mcp's
	// manager sweep).
	for key, why := range exempt {
		switch n := seen[key]; {
		case n == 0:
			complaints = append(complaints, fmt.Sprintf("the exception %q (%s) matches no lease.Spec the sweep found: the site moved, was renamed or is armed now, so drop the entry or re-key it", key, why))
		case n > 1:
			complaints = append(complaints, fmt.Sprintf("the exception %q (%s) covers ONE site and matches %d unarmed lease.Spec constructions: every spec that symbol builds would inherit this one reason, so arm the others — or put each decision on the record under its own %q key.", key, why, n, "file.go:line"))
		}
	}
	slices.Sort(complaints)
	return complaints
}

// exemptKeyOf answers the ledger key that puts this site on the record, or ""
// when none does. The file-and-LINE spelling is read first: it is how ONE of
// several specs a symbol builds carries a decision of its own, which the
// symbol key — shared by every spec inside that symbol — cannot express. For
// a site no symbol names the two spellings are the same key.
func exemptKeyOf(s specSite, exempt map[string]string) string {
	for _, key := range []string{s.where, s.key()} {
		if _, ok := exempt[key]; ok {
			return key
		}
	}
	return ""
}

// leaseSpecSitesInModule walks the module from the root the test process
// resolves and returns every lease.Spec construction in its non-test source.
//
// Test files are not judged, and that is this predicate's policy rather than
// the walk's (internal/sweepwalk reaches them): a spec built in a test
// exercises the mechanism — several of this package's own vary one field at a
// time — while a net is WIRED only from non-test source.
func leaseSpecSitesInModule(t *testing.T) []specSite {
	t.Helper()
	var sites []specSite
	sweepwalk.GoFiles(t, sweepwalk.ModuleRoot(t), func(rel, path string) {
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Errorf("parse %s: %v", rel, err)
			return
		}
		sites = append(sites, leaseSpecSitesIn(rel, fset, file)...)
	})
	slices.SortFunc(sites, func(a, b specSite) int { return strings.Compare(a.where, b.where) })
	return sites
}

// leaseSpecSitesIn reports every lease.Spec composite literal of one parsed
// file, attributed to the function or method that builds it.
//
// Three shapes of the SAME construction are judged, because each of them
// wires a net: the named literal, the literal inside a slice, array or map of
// Specs — which elides its own type, `[]lease.Spec{{Name: …}}` — and the one
// built through an aliased import. The package is recognised by its import
// PATH and the name THIS file binds it to; a file that does not import
// pkg/lease builds no Spec at all.
func leaseSpecSitesIn(rel string, fset *token.FileSet, file *ast.File) []specSite {
	local, imports := leaseImportName(file)
	if !imports {
		return nil
	}
	var sites []specSite
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if isLeaseSpecType(lit.Type, local) {
			sites = append(sites, leaseSpecSite(rel, fset, file, lit))
			return true
		}
		elem, ok := collectionElement(lit.Type)
		if !ok || !isLeaseSpecType(elem, local) {
			return true
		}
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				elt = kv.Value
			}
			// Only the elided literals: an element that names its own type
			// is reached by this walk on its own.
			if inner, ok := elt.(*ast.CompositeLit); ok && inner.Type == nil {
				sites = append(sites, leaseSpecSite(rel, fset, file, inner))
			}
		}
		return true
	})
	return sites
}

// leaseSpecSite reads one Spec literal: where it is, the symbol that builds
// it, and whether it arms the handover.
//
// `Health: nil` is NOT armed: pkg/lease documents a nil *Health as the
// behaviour without the net — a term ends on lease loss or shutdown, never on
// the work failing — so a predicate that reads the KEY's presence would call
// the disarmed site armed, which is the one answer this guard exists to
// refuse. Only a value the compiler could not call nil arms it; a constructor
// that RETURNS nil is beyond any parser, and is why the handover is witnessed
// by the nets' own tests too.
func leaseSpecSite(rel string, fset *token.FileSet, file *ast.File, lit *ast.CompositeLit) specSite {
	site := specSite{
		rel:    rel,
		symbol: enclosingSymbol(file, lit.Pos()),
		where:  rel + ":" + fmt.Sprint(fset.Position(lit.Pos()).Line),
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Health" {
			continue
		}
		if id, ok := kv.Value.(*ast.Ident); !ok || id.Name != "nil" {
			site.armed = true
		}
	}
	return site
}

// leaseImportName answers the name this file binds pkg/lease to — its alias
// when it has one, "lease" otherwise, "." for a dot import — and whether it
// imports the package at all. A blank import binds no name, so no Spec can be
// written through it.
func leaseImportName(file *ast.File) (string, bool) {
	const path = `"github.com/SocialGouv/iterion/pkg/lease"`
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != path {
			continue
		}
		if imp.Name == nil {
			return "lease", true
		}
		if imp.Name.Name == "_" {
			return "", false
		}
		return imp.Name.Name, true
	}
	return "", false
}

// isLeaseSpecType reports whether expr names lease.Spec, through the local
// name of the import and past a pointer: `*lease.Spec` is the element type of
// a slice of Specs as readily as the value type.
func isLeaseSpecType(expr ast.Expr, local string) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		return isLeaseSpecType(star.X, local)
	}
	if local == "." {
		id, ok := expr.(*ast.Ident)
		return ok && id.Name == "Spec"
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Spec" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == local
}

// collectionElement answers the element type of a slice, array or map type,
// which is what a literal inside it elides.
func collectionElement(expr ast.Expr) (ast.Expr, bool) {
	switch t := expr.(type) {
	case *ast.ArrayType:
		return t.Elt, true
	case *ast.MapType:
		return t.Value, true
	}
	return nil, false
}

// enclosingSymbol names the declaration that holds pos, as the exception
// ledger keys it: "Name" for a function, "(*T).Name" for a method, and the
// var or const a value is bound to for everything else. Empty when no
// declaration of the file holds pos at all.
//
// A net is wired as readily by a function VALUE (`var buildNet = func() …`)
// or by a package-level var as by a func declaration, and all three deserve
// the same key: a sweep that could name only the func declaration attributed
// every other spelling to the empty symbol, which is no key at all.
func enclosingSymbol(file *ast.File, pos token.Pos) string {
	for _, decl := range file.Decls {
		if pos < decl.Pos() || pos > decl.End() {
			continue
		}
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil || len(d.Recv.List) == 0 {
				return d.Name.Name
			}
			return "(" + types(d.Recv.List[0].Type) + ")." + d.Name.Name
		case *ast.GenDecl:
			if name := boundName(d, pos); name != "" {
				return name
			}
		}
	}
	return ""
}

// boundName names the var or const of decl whose VALUE holds pos: the symbol
// a reader greps for, and a stable ledger key, for a spec built anywhere but
// a function body. Empty when no value of the declaration holds pos — a spec
// inside a type declaration, which binds no name to it.
func boundName(decl *ast.GenDecl, pos token.Pos) string {
	for _, spec := range decl.Specs {
		value, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, v := range value.Values {
			if i < len(value.Names) && pos >= v.Pos() && pos <= v.End() {
				return value.Names[i].Name
			}
		}
	}
	return ""
}

// types renders a receiver type as source does: T, *T.
func types(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + types(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // a generic receiver, T[P]
		return types(t.X)
	}
	return ""
}

// The guard above is only worth its walk if its predicate can actually
// accuse: it reads a parsed file, so the unarmed site it must name is planted
// in memory rather than in the tree — a .go file written into a checkout
// under test is a dirty tree and a broken parallel build, and deleting it
// again is the paid class this repository refuses.
//
// The symbol attribution is asserted here too, because it is what the
// exception ledger is keyed by: a renamed constructor yields another key, so
// the exemption stops applying instead of silently covering new code.
func TestTheSpecHealthSweepNamesAnUnarmedSite(t *testing.T) {
	const src = `package other

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
)

type netWiring struct{}

func wireANetWithoutAHandover() lease.Spec {
	return lease.Spec{Name: "unarmed", Owner: "o", TTL: time.Second}
}

func (w *netWiring) wireANetWithAHandover() lease.Spec {
	return lease.Spec{Name: "armed", Owner: "o", TTL: time.Second, Health: &lease.Health{Passes: 3}}
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "wiring.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites := leaseSpecSitesIn("pkg/other/wiring.go", fset, file)
	if len(sites) != 2 {
		t.Fatalf("the sweep found %d lease.Spec in the planted source, want 2: %v", len(sites), sites)
	}
	want := []specSite{
		{rel: "pkg/other/wiring.go", symbol: "wireANetWithoutAHandover", armed: false, where: "pkg/other/wiring.go:12"},
		{rel: "pkg/other/wiring.go", symbol: "(*netWiring).wireANetWithAHandover", armed: true, where: "pkg/other/wiring.go:16"},
	}
	if !slices.Equal(sites, want) {
		t.Fatalf("sites = %+v, want %+v", sites, want)
	}
	// The key the ledger would carry names the symbol, so an exemption
	// written for one constructor cannot cover another.
	if got := sites[0].key(); got != "pkg/other/wiring.go::wireANetWithoutAHandover" {
		t.Errorf("exception key = %q, want the file and the enclosing symbol", got)
	}
}

// The guard judges the net, not the spelling of its construction: the same
// wiring written with `Health: nil`, through an aliased import, or as a
// literal inside a slice or a map of Specs has to be judged the same. A
// predicate that read the Health KEY's presence called the explicitly
// disarmed site armed — the one answer this guard exists to refuse, since
// pkg/lease documents a nil *Health as the behaviour WITHOUT the net.
func TestTheSpecHealthSweepJudgesEverySpellingOfTheWiring(t *testing.T) {
	const src = `package other

import (
	"time"

	l "github.com/SocialGouv/iterion/pkg/lease"
)

func wireANetWithANilHandover() l.Spec {
	return l.Spec{Name: "nil-health", Owner: "o", TTL: time.Second, Health: nil}
}

func wireEveryNetOfAFleet() []l.Spec {
	return []l.Spec{
		{Name: "in-a-slice", Owner: "o", TTL: time.Second},
		{Name: "armed-in-a-slice", Owner: "o", TTL: time.Second, Health: &l.Health{}},
	}
}

func wireNetsByName() map[string]l.Spec {
	return map[string]l.Spec{"by-name": {Name: "in-a-map", Owner: "o", TTL: time.Second}}
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "aliased.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	const rel = "pkg/other/aliased.go"
	want := []specSite{
		{rel: rel, symbol: "wireANetWithANilHandover", armed: false, where: rel + ":10"},
		{rel: rel, symbol: "wireEveryNetOfAFleet", armed: false, where: rel + ":15"},
		{rel: rel, symbol: "wireEveryNetOfAFleet", armed: true, where: rel + ":16"},
		{rel: rel, symbol: "wireNetsByName", armed: false, where: rel + ":21"},
	}
	if got := leaseSpecSitesIn(rel, fset, file); !slices.Equal(got, want) {
		t.Fatalf("sites = %+v, want %+v", got, want)
	}

	// And it accuses only pkg/lease's Spec: a package-local type of the same
	// name, in a file that does not import pkg/lease, wires no net.
	const lookalike = `package other

type Spec struct {
	Name   string
	Health *int
}

func notANet() Spec { return Spec{Name: "a lookalike"} }
`
	fset = token.NewFileSet()
	file, err = parser.ParseFile(fset, "lookalike.go", lookalike, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := leaseSpecSitesIn("pkg/other/lookalike.go", fset, file); len(got) != 0 {
		t.Errorf("the sweep accused %d site in a file that does not import pkg/lease: %+v", len(got), got)
	}
}

// A net is wired as readily by a function VALUE or by a package-level var as
// by a func declaration, and the sweep has to name what builds it in all
// three: the ledger is keyed by that name, and a site the sweep attributed to
// the EMPTY symbol shared one non-key with every other such site in its file
// — an exemption written for one would have covered the next spec added
// beside it.
func TestTheSpecHealthSweepNamesWhatBuildsASpecOutsideAFuncDecl(t *testing.T) {
	const src = `package other

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
)

var buildNet = func(owner string) lease.Spec {
	return lease.Spec{Name: "a function value", Owner: owner, TTL: time.Second, Health: &lease.Health{Passes: 3}}
}

var defaultNet = lease.Spec{Name: "a package-level net", Owner: "o", TTL: time.Second, Health: &lease.Health{Passes: 3}}

var unarmedAtPackageLevel = lease.Spec{Name: "unarmed", Owner: "o", TTL: time.Second}

var armedByName, unarmedByName = lease.Spec{Name: "a", Owner: "o", Health: &lease.Health{}}, lease.Spec{Name: "b", Owner: "o"}

func wireANetInABody() lease.Spec {
	return lease.Spec{Name: "the shape that already worked", Owner: "o", TTL: time.Second, Health: &lease.Health{Passes: 3}}
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "nets.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	const rel = "pkg/other/nets.go"
	want := []specSite{
		{rel: rel, symbol: "buildNet", armed: true, where: rel + ":10"},
		{rel: rel, symbol: "defaultNet", armed: true, where: rel + ":13"},
		{rel: rel, symbol: "unarmedAtPackageLevel", armed: false, where: rel + ":15"},
		{rel: rel, symbol: "armedByName", armed: true, where: rel + ":17"},
		{rel: rel, symbol: "unarmedByName", armed: false, where: rel + ":17"},
		{rel: rel, symbol: "wireANetInABody", armed: true, where: rel + ":20"},
	}
	got := leaseSpecSitesIn(rel, fset, file)
	if !slices.Equal(got, want) {
		t.Fatalf("sites = %+v, want %+v", got, want)
	}
	// Two specs on ONE declaration line are told apart by the name each is
	// bound to, which is the only thing that can key them separately.
	if a, b := got[3].key(), got[4].key(); a == b {
		t.Errorf("the two specs of one var declaration share the ledger key %q: an exemption for either would cover both", a)
	}
}

// What a site is JUDGED on is whether it arms the handover; the symbol is
// only how a decision gets on the record, which an armed net needs none of.
// A judgment that demanded the key FIRST reported a fully armed net — one
// built by a function value, say — as an offender, in a blocking gate, with
// a message telling its author to put on the record something that is
// already compliant.
func TestTheSpecHealthJudgmentAccusesOnlyUnarmedNets(t *testing.T) {
	const rel = "pkg/other/nets.go"
	sites := []specSite{
		{rel: rel, symbol: "", armed: true, where: rel + ":10"},
		{rel: rel, symbol: "wireAnArmedNet", armed: true, where: rel + ":20"},
		{rel: rel, symbol: "", armed: false, where: rel + ":30"},
		{rel: rel, symbol: "wireAnUnarmedNet", armed: false, where: rel + ":40"},
		{rel: rel, symbol: "wireAnExemptNet", armed: false, where: rel + ":50"},
	}
	exempt := map[string]string{rel + "::wireAnExemptNet": "on the record"}
	complaints := judgeSpecSites(sites, exempt)
	if len(complaints) != 2 {
		t.Fatalf("the judgment made %d complaints, want 2 (the two unarmed nets that are not on the record): %q", len(complaints), complaints)
	}
	// The unattributable site is named by its file and LINE: a reader can
	// open it, and the ledger can key it without covering its neighbours.
	if !strings.Contains(complaints[0], rel+":30") {
		t.Errorf("complaint %q does not name the unattributable unarmed site at %s:30", complaints[0], rel)
	}
	if !strings.Contains(complaints[1], "wireAnUnarmedNet") {
		t.Errorf("complaint %q does not name the symbol that builds the unarmed net", complaints[1])
	}
	for _, c := range complaints {
		for _, armed := range []string{rel + ":10", rel + ":20", "wireAnArmedNet", "wireAnExemptNet"} {
			if strings.Contains(c, armed) {
				t.Errorf("the judgment accused %s, which arms the handover or is on the record: %q", armed, c)
			}
		}
	}
	// An exemption for a site no symbol names covers THAT site and no
	// other: keyed by the bare file, the next unarmed spec written beside an
	// exempt one would have inherited its exemption.
	twoUnattributable := []specSite{
		{rel: rel, symbol: "", armed: false, where: rel + ":30"},
		{rel: rel, symbol: "", armed: false, where: rel + ":60"},
	}
	neighbour := judgeSpecSites(twoUnattributable, map[string]string{rel + ":30": "on the record"})
	if len(neighbour) != 1 || !strings.Contains(neighbour[0], rel+":60") {
		t.Errorf("exempting the site at %s:30 yielded %q, want the one at :60 still accused", rel, neighbour)
	}
	// And a ledger entry nobody matches is still a complaint of its own.
	stale := judgeSpecSites(sites[:2], map[string]string{rel + "::gone": "moved away"})
	if len(stale) != 1 || !strings.Contains(stale[0], rel+"::gone") {
		t.Errorf("a ledger entry matching no site yielded %q, want one complaint naming it", stale)
	}
}

// An entry in the exception ledger is a decision about ONE net, and the
// ledger names a net by the SYMBOL that builds it — so two unarmed specs
// built inside one function (another stage's claim, a second TryRun in a
// nested closure) share a key. The entry's matches are therefore counted: a
// second spec written beside an exempt one is accused on its own instead of
// inheriting a reason nobody wrote for it, in a gate whose whole job is to
// redden on an unarmed net. The decision a symbol cannot express per site is
// written under the site's own file and line.
func TestTheSpecHealthLedgerCoversOneSiteNotEverySpecASymbolBuilds(t *testing.T) {
	const rel = "pkg/other/schedules.go"
	const symbol = "(*MongoStore).WithSchedule"
	first := specSite{rel: rel, symbol: symbol, armed: false, where: rel + ":30"}
	second := specSite{rel: rel, symbol: symbol, armed: false, where: rel + ":42"}
	bySymbol := map[string]string{rel + "::" + symbol: "one TryRun term for one schedule launch"}

	// One site, one entry: the decision stands and nothing is accused.
	if got := judgeSpecSites([]specSite{first}, bySymbol); len(got) != 0 {
		t.Fatalf("the exempt site alone yielded %q, want no complaint", got)
	}
	// A second unarmed spec inside that same symbol is not covered by it.
	got := judgeSpecSites([]specSite{first, second}, bySymbol)
	if len(got) != 1 {
		t.Fatalf("a second unarmed spec under the exempt symbol yielded %d complaints %q, want 1", len(got), got)
	}
	if !strings.Contains(got[0], rel+"::"+symbol) || !strings.Contains(got[0], "ONE site") {
		t.Errorf("the complaint %q does not say the entry covers one site and matched more", got[0])
	}
	// The remedy that complaint names works: each site on the record under
	// its own file and line, and each judged by its own entry.
	perSite := map[string]string{first.where: "one TryRun term", second.where: "a second TryRun term"}
	if leftover := judgeSpecSites([]specSite{first, second}, perSite); len(leftover) != 0 {
		t.Fatalf("one per-site entry each yielded %q, want no complaint", leftover)
	}
	one := judgeSpecSites([]specSite{first, second}, map[string]string{first.where: "one TryRun term"})
	if len(one) != 1 || !strings.Contains(one[0], second.where) {
		t.Fatalf("exempting %s alone yielded %q, want the site at %s still accused", first.where, one, second.where)
	}
	// An ARMED spec sharing the exempt symbol is no offender and no match:
	// the question a site is judged on is asked before the ledger, so the
	// entry still covers exactly the one site it was written for.
	armedNeighbour := specSite{rel: rel, symbol: symbol, armed: true, where: rel + ":55"}
	if got := judgeSpecSites([]specSite{first, armedNeighbour}, bySymbol); len(got) != 0 {
		t.Fatalf("an armed spec beside the exempt one yielded %q, want no complaint", got)
	}
	// And a net somebody armed leaves its entry matching nothing, which is
	// the stale complaint: the decision is no longer needed.
	armedNow := first
	armedNow.armed = true
	stale := judgeSpecSites([]specSite{armedNow}, bySymbol)
	if len(stale) != 1 || !strings.Contains(stale[0], "armed now") {
		t.Fatalf("arming the exempt site yielded %q, want one complaint saying the entry is stale", stale)
	}
}
