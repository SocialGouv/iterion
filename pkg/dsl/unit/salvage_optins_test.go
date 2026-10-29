package unit

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"os/exec"

	"github.com/SocialGouv/iterion/pkg/git"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/modtypes"
)

const (
	modulePath        = "github.com/SocialGouv/iterion"
	unitPackagePath   = "github.com/SocialGouv/iterion/pkg/dsl/unit"
	authorPackagePath = "github.com/SocialGouv/iterion/pkg/dsl/author"
	astPackagePath    = "github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// A reading a bound of the reader refused — a source past the read budget or
// the size cap, a bot its shared budget cut short — is no reading of the
// file, and a route decided from it, a file written from it or a document
// emitted as the file's acts on what nobody read. The type system carries
// most of the rule: parser.Parse and ParseSpending answer such a source as a
// *parser.ReadRefusal and hand no reading over (no exported door of the
// parser does), every unit loader answers a short load as a
// *unit.ShortError, and the one way to see a short load — unit.Salvage — is
// a view (unit.Salvaged) whose methods hand out no unit, no file's AST and
// no program, so no API that takes one of those accepts it.
//
// What the compiler does not hold, these guards hold, on the module as the
// type checker resolves it (internal/modtypes: every package the build
// compiles, wherever its directory is) — never on a spelling:
//
//   - every production reference to a salvage door, keyed by the function
//     holding it and the call it makes (TestEverySalvageOptInIsListed);
//   - every production function that holds a view, and in none of them a
//     decoding of a document (TestEveryFunctionHoldingASalvageIsListed) —
//     the bytes a short view marshals for display are marked a salvage,
//     which the decode refuses wherever it runs (ast.ErrSalvage:
//     ast.UnmarshalFile by name, any encoding/json decode of an ast.File
//     through the type's UnmarshalJSON), so the bytes handed on to a helper
//     no guard lists decode into no program — and a decode that would drop
//     the mark before a File is built (a mirror of the keys, re-marshalled)
//     is refused by name (TestADocumentIsDecodedThroughItsOneDoor);
//   - the export surface of the reading packages: an exported function,
//     method, field or variable — promoted ones included — whose type
//     reaches an AST, a unit, a unit file, a parse result or an author
//     result, through an unexported type's members or a carrier of any value
//     (reflect.Value, context.Context) too, is listed
//     (TestEveryReadingAPIIsListed);
//   - unsafe and reflect's pointer doors (NewAt, Value.UnsafePointer,
//     UnsafeAddr, Pointer), by file; a file another build compiles that
//     imports a reading door, by its imports; //go:linkname into the
//     language's packages; the test helpers in the build graph of production
//     code; and a Go file of the repository that no package of the module
//     holds, which no rule reads (TestNoGuardIsReadAroundByTheBuild).
//
// salvageDoors are the functions that still hand a refused reading over in
// some form: the view (unit.Salvage), the author document's reading
// (author.Parse: the empty file of a document refused whole, its refusal
// among the diagnostics), and the lexer (parser.NewLexer, ScanSource, Scan:
// a source refused whole lexes as one error token).
var salvageDoors = map[string][]string{
	unitPackagePath:   {"Salvage"},
	authorPackagePath: {"Parse"},
	parserPackagePath: {"NewLexer", "ScanSource", "Scan"},
}

// testOnlyPackages are the test helpers that take a loader's or the parser's
// answer as one value, panicking on a refusal: no package the module builds
// for production depends on them.
var testOnlyPackages = map[string]bool{
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest": true,
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest":     true,
}

// theModule is the module, listed and type-checked once for this test
// binary.
func theModule(t *testing.T) *modtypes.Module {
	t.Helper()
	m, err := modtypes.Load(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// walkModuleSources calls fn with every production Go file of the module's
// packages, by slash path from the module root: the files the build compiles
// on this platform and the ones it compiles on another (IgnoredGoFiles) —
// every Go file of the repository, which TestNoGuardIsReadAroundByTheBuild
// holds to a package.
func walkModuleSources(t *testing.T, fn func(rel string, src []byte)) {
	t.Helper()
	m := theModule(t)
	for _, p := range m.Packages {
		for _, rel := range append(append([]string{}, p.FileRels...), p.IgnoredGoFiles...) {
			if strings.HasSuffix(rel, "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(m.Root, filepath.FromSlash(rel))) // #nosec G304 -- this repository's own tree
			if err != nil {
				t.Fatal(err)
			}
			fn(rel, src)
		}
	}
}

// holder is a span of a file's code that a reference is attributed to: a
// function or method declaration, by its receiver-qualified name —
// "(*Server).handleOpenFile", "LoadSchema" — or a package-level variable's
// value ("var x").
type holder struct {
	name       string
	pos, end   token.Pos
	code       []ast.Node
	signatures []ast.Node
}

func holdersOf(p *modtypes.Package, f *ast.File) []holder {
	var out []holder
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if fn, ok := p.Info.Defs[d.Name].(*types.Func); ok {
				name = funcName(fn)
			}
			h := holder{name: name, pos: d.Pos(), end: d.End(), signatures: []ast.Node{d.Type}}
			if d.Recv != nil {
				h.signatures = append(h.signatures, d.Recv)
			}
			if d.Body != nil {
				h.code = append(h.code, d.Body)
			}
			out = append(out, h)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				var names []string
				for _, n := range vs.Names {
					names = append(names, n.Name)
				}
				h := holder{name: "var " + strings.Join(names, ","), pos: vs.Pos(), end: vs.End()}
				for _, v := range vs.Values {
					h.code = append(h.code, v)
				}
				if vs.Type != nil {
					h.signatures = append(h.signatures, vs.Type)
				}
				out = append(out, h)
			}
		}
	}
	return out
}

// funcName is a function's name as a holder spells it: a method qualified
// by its receiver's type, its package left out.
func funcName(fn *types.Func) string {
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil {
		return fn.Name()
	}
	return "(" + types.TypeString(sig.Recv().Type(), func(*types.Package) string { return "" }) + ")." + fn.Name()
}

// inspectWithParents walks n, handing each node the chain of its ancestors
// (outermost first).
func inspectWithParents(n ast.Node, fn func(n ast.Node, parents []ast.Node)) {
	var stack []ast.Node
	ast.Inspect(n, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		fn(n, stack)
		stack = append(stack, n)
		return true
	})
}

// nodeText is the source of n as go/printer spells it, on one line.
func nodeText(fset *token.FileSet, n ast.Node) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, n); err != nil {
		return fmt.Sprintf("<%T>", n)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// refText is what a reference to an object is in the code: the call it is
// the function of, or the selector or name it is.
func refText(fset *token.FileSet, id *ast.Ident, parents []ast.Node) string {
	var expr ast.Node = id
	if len(parents) > 0 {
		if sel, ok := parents[len(parents)-1].(*ast.SelectorExpr); ok && sel.Sel == id {
			expr = sel
		}
	}
	if i := len(parents) - 1 - btoi(expr != ast.Node(id)); i >= 0 {
		if call, ok := parents[i].(*ast.CallExpr); ok && call.Fun == expr {
			return nodeText(fset, call)
		}
	}
	return nodeText(fset, expr)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isObject reports whether obj is the package-level object name of the
// package at path — compared by path and name: an object read from export
// data and the one the type checker declared are two values of one object.
func isObject(obj types.Object, path, name string) bool {
	return obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == path && obj.Name() == name && obj.Pkg().Scope().Lookup(name) == obj
}

// salvageReferences names every production reference to a salvage door —
// called, or taken as a value, under any spelling the type checker resolves
// to the door — as "<file> <holder> <door> <call>": the call the reference
// makes (its source, on one line), its occurrence counted only among
// identical calls of the holder ("#2"). The door's own declaration is no
// reference; a reference inside its own package is one.
func salvageReferences(m *modtypes.Module, p *modtypes.Package) []string {
	var out []string
	for i, f := range p.Files {
		rel := p.FileRels[i]
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		for _, h := range holdersOf(p, f) {
			count := map[string]int{}
			for _, code := range h.code {
				inspectWithParents(code, func(n ast.Node, parents []ast.Node) {
					id, ok := n.(*ast.Ident)
					if !ok {
						return
					}
					obj := p.Info.Uses[id]
					for path, doors := range salvageDoors {
						for _, door := range doors {
							if !isObject(obj, path, door) {
								continue
							}
							key := fmt.Sprintf("%s %s %s %s", rel, h.name, door, refText(m.Fset, id, parents))
							count[key]++
							if count[key] > 1 {
								key += fmt.Sprintf(" #%d", count[key])
							}
							out = append(out, key)
						}
					}
				})
			}
		}
	}
	return out
}

// TestEverySalvageOptInIsListed: every production reference to a salvage
// door is listed with the reason it neither writes, emits as the file nor
// decides from what the reading lacks; a new one fails until it is listed.
// A reference is keyed by its file, the function holding it (receiver
// included: two methods of one name are two holders) and the call it makes
// — a new call in a listed function is a new key, whatever its place among
// the others, and an entry no reference answers is stale.
func TestEverySalvageOptInIsListed(t *testing.T) {
	optIns := map[string]string{
		// The view of a load: what an answer shows of a bot, whole or short.
		// Every write refuses a short one — the studio refuses a salvage
		// (salvageRefusal, applyParsedSource), each save reads the stored
		// file under its lock (checkStoredFile) and the unit saves load
		// strictly (saveUnit, unparseUnitFiles).
		"pkg/server/server_files.go (*Server).handleOpenFile Salvage unit.Salvage(unit.LoadDirWithMain(absPath, absPath, data))": "the editor's open of a bot in several files: the view marshalled with provenance, bindable false unless Whole, read_refusal, a revision no whole load has",
		"pkg/server/server_dsl.go (*Server).serveDiskExample Salvage unit.Salvage(u, loadErr)":                                   "the bot picker's open, as handleOpenFile's; only the unit the strict loader answered (no error) is flattened",
		"pkg/server/server_files_unit.go (*Server).parseUnitFiles Salvage unit.Salvage(unit.LoadMap(req.Files, main))":           "the cloud editor's open of a bundle, as handleOpenFile's",
		"pkg/server/server_files_unit.go (*Server).parseUnitWithFile Salvage unit.Salvage(staged, nil)":                          "the per-file apply's answer shows the staged unit — loaded whole, by the strict loader — through the view (unitInfoOf)",
		"pkg/botregistry/schema.go LoadSchema Salvage unit.Salvage(loaded, loadErr)":                                             "the view's file NAMES key the cached refusal of a short load — the files it read decide where the budget ran out; the schema is the whole unit's alone",
		// The author document's reading: refused whole, it is the empty file
		// beside its refusal, never a partial program.
		"pkg/dsl/canon/document.go Document Parse author.Parse(name, text)":              "canon.Document refuses a document whose reading holds an error before using its File",
		"pkg/dsl/canon/document.go Document Parse author.Parse(name, out)":               "reads back the document canon.Document wrote, refusing it on an error before comparing",
		"pkg/cli/fmt_author.go convertTwin Parse author.Parse(abs, raw)":                 "refuses a document whose reading holds an error (HasErrors) before writing anything",
		"pkg/cli/fmt_author.go convertTwin Parse author.Parse(abs, out)":                 "reads back the document it wrote, for the comparison that refuses a write reading otherwise",
		"pkg/cli/validate_author.go openAuthorDocument Parse author.Parse(abs, src)":     "validate REPORTS the reading's diagnostics, the refusal among them: a document holding an error is INVALID whatever its empty file loads to, and E054/E056 are asked only of a document that reads (doc.res.HasErrors)",
		"pkg/dsl/author/internal/probecheck/main.go check Parse author.Parse(path, src)": "the authoring probe's harness REPORTS the reading's diagnostics and its verdict, writing nothing",
		// The lexer: a source refused whole lexes as one error token.
		"pkg/dsl/parser/comments.go ScanSource NewLexer NewLexer(name, src)":                    "ScanSource is the lexer door's own composition",
		"pkg/dsl/parser/comments.go ScanSource Scan Scan(NewLexer(name, src).All())":            "ScanSource is the lexer door's own composition",
		"pkg/dsl/canon/canon.go valuesOf NewLexer parser.NewLexer(name, text)":                  "Folds' values: a lexer that refused the text answers an error (\"cannot be told\"), never \"nothing folds\"",
		"pkg/dsl/fix/fix.go PlanFor NewLexer parser.NewLexer(name, norm.Text)":                  "positions of the edits a validator annotates: a refused text has no diagnostic to place (its compile never ran), and fix writes only a text whose verify reads whole",
		"pkg/dsl/migrate/migrate.go Bytes NewLexer parser.NewLexer(name, norm.Text)":            "lexes a text parser.Parse read whole just above (a refusal returns ErrRefused before)",
		"pkg/dsl/unparse/comments.go placeComments ScanSource parser.ScanSource(name, text)":    "places comments into the text the unparser wrote, which unparse.Verify reads strictly before anything is written",
		"pkg/dsl/unparse/comments.go blockScalarBodyLines NewLexer parser.NewLexer(name, text)": "the block scalars of the text the unparser wrote, verified strictly before any write",
		"pkg/dsl/unit/unit.go cutsAToken NewLexer parser.NewLexer(\"head\", head)":              "MembersDir's head cut: a head the lexer refuses has no literal to close, and the head's own parse then refuses it (ShortError)",
	}
	m := theModule(t)
	seen := map[string]bool{}
	var offenders []string
	for _, p := range m.Packages {
		if p.Info == nil {
			continue
		}
		for _, key := range salvageReferences(m, p) {
			seen[key] = true
			if _, ok := optIns[key]; !ok {
				offenders = append(offenders, key)
			}
		}
	}
	sort.Strings(offenders)
	for _, key := range offenders {
		t.Errorf("%s opts into a reading a bound of the reader may have refused: list it here with the reason it neither writes, emits as the file nor decides from what the reading lacks — or call the strict door (parser.Parse, unit.Load*) and answer its error", key)
	}
	var stale []string
	for key := range optIns {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("%s is listed as a salvage opt-in but no reference answers it: remove the entry", key)
	}
}

// mentions reports whether t names a type match accepts, anywhere in its
// structure: behind a pointer, in a slice, a map, a channel, a function's
// parameters or results, a struct's fields, a type's arguments.
func mentions(t types.Type, match func(*types.TypeName) bool, seen map[types.Type]bool) bool {
	t = types.Unalias(t)
	if seen[t] {
		return false
	}
	seen[t] = true
	switch x := t.(type) {
	case *types.Named:
		if match(x.Obj()) {
			return true
		}
		if args := x.TypeArgs(); args != nil {
			for i := 0; i < args.Len(); i++ {
				if mentions(args.At(i), match, seen) {
					return true
				}
			}
		}
		// a named holder: its underlying struct carries the matched type in a
		// field (type Holder struct { ast.File }); the type itself is already
		// in seen, so the descent cannot cycle.
		return mentions(x.Underlying(), match, seen)
	case *types.Pointer:
		return mentions(x.Elem(), match, seen)
	case *types.Slice:
		return mentions(x.Elem(), match, seen)
	case *types.Array:
		return mentions(x.Elem(), match, seen)
	case *types.Chan:
		return mentions(x.Elem(), match, seen)
	case *types.Map:
		return mentions(x.Key(), match, seen) || mentions(x.Elem(), match, seen)
	case *types.Signature:
		return mentions(x.Params(), match, seen) || mentions(x.Results(), match, seen)
	case *types.Tuple:
		for i := 0; i < x.Len(); i++ {
			if mentions(x.At(i).Type(), match, seen) {
				return true
			}
		}
	case *types.Struct:
		for i := 0; i < x.NumFields(); i++ {
			if mentions(x.Field(i).Type(), match, seen) {
				return true
			}
		}
	}
	return false
}

// isView reports whether a type name is the view of a load or one of its
// files.
func isView(tn *types.TypeName) bool {
	return tn.Pkg() != nil && tn.Pkg().Path() == unitPackagePath && (tn.Name() == "Salvaged" || tn.Name() == "SalvagedFile")
}

// viewHolders names, by "<file> <holder>", every production function outside
// pkg/dsl/unit in which a value of the view's type appears — a parameter, a
// result, a variable, any expression — with what it does with the view;
// decodes lists the holders that also decode a document (pkg/dsl/ast's
// Unmarshal*).
func viewHolders(m *modtypes.Module, p *modtypes.Package) (holders, decodes []string) {
	type span struct {
		rel string
		h   holder
	}
	var spans []span
	for i, f := range p.Files {
		if strings.HasSuffix(p.FileRels[i], "_test.go") {
			continue
		}
		for _, h := range holdersOf(p, f) {
			spans = append(spans, span{p.FileRels[i], h})
		}
	}
	at := func(pos token.Pos) *span {
		for i := range spans {
			if spans[i].h.pos <= pos && pos < spans[i].h.end {
				return &spans[i]
			}
		}
		return nil
	}
	held := map[string]bool{}
	decoding := map[string]bool{}
	note := func(pos token.Pos, t types.Type) {
		if t == nil || !mentions(t, isView, map[types.Type]bool{}) {
			return
		}
		if s := at(pos); s != nil {
			held[s.rel+" "+s.h.name] = true
		}
	}
	for expr, tv := range p.Info.Types {
		note(expr.Pos(), tv.Type)
	}
	for id, obj := range p.Info.Defs {
		if obj != nil {
			note(id.Pos(), obj.Type())
		}
	}
	for id, obj := range p.Info.Uses {
		if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != astPackagePath || !strings.HasPrefix(obj.Name(), "Unmarshal") {
			continue
		}
		if s := at(id.Pos()); s != nil {
			decoding[s.rel+" "+s.h.name] = true
		}
	}
	for k := range held {
		holders = append(holders, k)
		if decoding[k] {
			decodes = append(decodes, k)
		}
	}
	sort.Strings(holders)
	sort.Strings(decodes)
	return holders, decodes
}

// TestEveryFunctionHoldingASalvageIsListed: the view of a load reaches no
// function that is not listed here with what it shows of it — a helper
// handed the view is a holder, whatever it is called and whatever type
// spells the view (an alias included) — and no listed holder decodes a
// document through pkg/dsl/ast's Unmarshal*: a holder answers the view, and
// decodes a client's document elsewhere. A short view's display bytes are
// marked a salvage, which the decode refuses wherever it runs
// (ast.ErrSalvage: UnmarshalFile by name, any encoding/json decode of an
// ast.File through UnmarshalJSON); a whole view's decode into the bot it
// read whole.
func TestEveryFunctionHoldingASalvageIsListed(t *testing.T) {
	listed := map[string]string{
		"pkg/server/server_files.go (*Server).handleOpenFile":         "the editor's open (TestEverySalvageOptInIsListed)",
		"pkg/server/server_files.go unitReadRefusal":                  "the answer's word for the view's refusal: its message and hint",
		"pkg/server/server_files_unit.go unitInfoOf":                  "the answer's unit: the view's file names, revision, verdicts",
		"pkg/server/server_files_unit.go (*Server).parseUnitFiles":    "the cloud editor's open (TestEverySalvageOptInIsListed)",
		"pkg/server/server_files_unit.go (*Server).parseUnitWithFile": "the per-file apply's answer (TestEverySalvageOptInIsListed)",
		"pkg/server/server_dsl.go (*Server).serveDiskExample":         "the bot picker's open (TestEverySalvageOptInIsListed)",
		"pkg/botregistry/schema.go LoadSchema":                        "the cached refusal is keyed by the view's file names (TestEverySalvageOptInIsListed)",
	}
	m := theModule(t)
	seen := map[string]bool{}
	var offenders []string
	for _, p := range m.Packages {
		if p.Info == nil || p.ImportPath == unitPackagePath {
			continue
		}
		holders, decodes := viewHolders(m, p)
		for _, k := range holders {
			seen[k] = true
			if _, ok := listed[k]; !ok {
				offenders = append(offenders, k)
			}
		}
		for _, k := range decodes {
			t.Errorf("%s holds the view of a load and decodes a document (pkg/dsl/ast Unmarshal*): the view's display bytes would become a writable program — decode the client's document elsewhere", k)
		}
	}
	sort.Strings(offenders)
	for _, k := range offenders {
		t.Errorf("%s holds the view of a load (unit.Salvaged): list it here with what it shows of the view, or take the strict loader's answer", k)
	}
	for k := range listed {
		if !seen[k] {
			t.Errorf("%s is listed as holding the view of a load but holds none: remove the entry", k)
		}
	}
}

// readingAPI is every exported function, method, field, variable and alias
// of the parser, the unit loader and the author reader whose type reaches a
// reading — an AST node, a unit, a unit file, a parse result, an author
// result — or any value at all (an empty interface), each with why it never
// hands out a reading a bound refused. A new one is a new door until it says
// so here.
var readingAPI = map[string]string{
	"pkg/dsl/parser Parse":            "answers a refused source as *ReadRefusal (whole)",
	"pkg/dsl/parser ParseSpending":    "answers a refused source as *ReadRefusal (whole)",
	"pkg/dsl/unit Load":               "answers a short load as *ShortError (whole)",
	"pkg/dsl/unit LoadDir":            "answers a short load as *ShortError (whole)",
	"pkg/dsl/unit LoadDirStaged":      "answers a short load as *ShortError (whole)",
	"pkg/dsl/unit LoadDirWithMain":    "answers a short load as *ShortError (whole)",
	"pkg/dsl/unit LoadDirWithMainAST": "answers a short load as *ShortError (whole); the AST it takes is the caller's",
	"pkg/dsl/unit LoadMap":            "answers a short load as *ShortError (whole)",
	"pkg/dsl/author Parse":            "listed in TestEverySalvageOptInIsListed: a refused document is the empty file beside its refusal",
	// Doors that take a reading in and hand none out.
	"pkg/dsl/parser Frontmatter":          "takes a parsed file, hands out its frontmatter's lines",
	"pkg/dsl/unit Digest":                 "takes a unit's files, hands out the digest of their bytes",
	"pkg/dsl/unit Salvage":                "takes a loader's answer, hands out the view (unit.Salvaged), whose surface is held here",
	"pkg/dsl/author Write":                "takes an AST, hands out the document's bytes",
	"pkg/dsl/author WriteKeepingComments": "takes an AST and a document's bytes, hands out the document's bytes",
	// A reading of one line, which no budget refuses whole as a file.
	"pkg/dsl/parser ParseEdgeLine": "one edge line read by the grammar: a line the lexer refuses reports the refusal among its diagnostics, and the one caller (the author speller) reads the diagnostics alone",
}

// sensitive is a type that IS a reading: holding one, a caller holds what
// the reader read — the AST's nodes, a unit, a unit file, a parse result,
// an author result. Their own fields and methods are no door.
func sensitive(tn *types.TypeName) bool {
	if tn.Pkg() == nil {
		return false
	}
	switch tn.Pkg().Path() {
	case astPackagePath:
		// The AST's nodes; an enumeration (a basic type underneath, as
		// ast.CommentPlace) is a value, no node of the program.
		_, basic := tn.Type().Underlying().(*types.Basic)
		return !basic
	case unitPackagePath:
		return tn.Name() == "Unit" || tn.Name() == "File"
	case parserPackagePath:
		return tn.Name() == "ParseResult"
	case authorPackagePath:
		return tn.Name() == "Result"
	}
	return false
}

// readingPackages are the packages whose export surface is held.
var readingPackages = []string{parserPackagePath, unitPackagePath, authorPackagePath}

// reaches reports whether a value of type t hands a reading over: t names a
// sensitive type anywhere in its structure, or is an empty interface (it
// may hold anything). An EXPORTED named type of a reading package is no
// reach by itself — its own exported surface is held entry by entry
// (readingSurface) — while every other named type is read through what a
// holder can get out of it: an unexported type of a reading package (whose
// surface no entry holds) and a type of another package of the module
// through their exported methods and fields, promoted ones included; a type
// outside the module — reflect.Value, context.Context, any carrier of a
// value — through what its exported methods answer and its exported fields
// hold (Value.Interface, Context.Value: any value at all).
func reaches(t types.Type, seen map[types.Type]bool) bool {
	t = types.Unalias(t)
	if seen[t] {
		return false
	}
	seen[t] = true
	switch x := t.(type) {
	case *types.Named:
		obj := x.Obj()
		if sensitive(obj) {
			return true
		}
		if args := x.TypeArgs(); args != nil {
			for i := 0; i < args.Len(); i++ {
				if reaches(args.At(i), seen) {
					return true
				}
			}
		}
		if obj.Pkg() == nil {
			return false // error: its one method answers a string
		}
		inModule := strings.HasPrefix(obj.Pkg().Path(), modulePath+"/")
		if inModule && obj.Exported() {
			for _, p := range readingPackages {
				if obj.Pkg().Path() == p {
					return false
				}
			}
		}
		if reachesMembers(x, inModule, seen) {
			return true
		}
		if !inModule {
			return false
		}
		if st, ok := x.Underlying().(*types.Struct); ok {
			return reachesFields(st, seen)
		}
		return reaches(x.Underlying(), seen)
	case *types.Pointer:
		return reaches(x.Elem(), seen)
	case *types.Slice:
		return reaches(x.Elem(), seen)
	case *types.Array:
		return reaches(x.Elem(), seen)
	case *types.Chan:
		return reaches(x.Elem(), seen)
	case *types.Map:
		return reaches(x.Key(), seen) || reaches(x.Elem(), seen)
	case *types.Signature:
		return reaches(x.Params(), seen) || reaches(x.Results(), seen)
	case *types.Tuple:
		for i := 0; i < x.Len(); i++ {
			if reaches(x.At(i).Type(), seen) {
				return true
			}
		}
	case *types.Struct:
		return reachesFields(x, seen)
	case *types.Interface:
		if x.NumMethods() == 0 {
			return true
		}
		for i := 0; i < x.NumMethods(); i++ {
			if reaches(x.Method(i).Type(), seen) {
				return true
			}
		}
	case *types.TypeParam:
		return reaches(x.Constraint(), seen)
	}
	return false
}

func reachesFields(st *types.Struct, seen map[types.Type]bool) bool {
	for _, f := range exportedFields(st) {
		if reaches(f.Type(), seen) {
			return true
		}
	}
	return false
}

// reachesMembers reports whether a holder of a value of the named type n
// gets a reading out of it through an exported method of n or *n —
// promoted ones included — or an exported field, promoted ones included. A
// method of a type of the module is read whole (a callback it takes may be
// handed a reading); one of a type outside the module by what it answers
// (its results), the parameters a foreign API takes being its own.
func reachesMembers(n *types.Named, inModule bool, seen map[types.Type]bool) bool {
	for _, recv := range []types.Type{n, types.NewPointer(n)} {
		ms := types.NewMethodSet(recv)
		for i := 0; i < ms.Len(); i++ {
			fn, ok := ms.At(i).Obj().(*types.Func)
			if !ok || !fn.Exported() {
				continue
			}
			sig := fn.Type().(*types.Signature)
			if inModule && reaches(sig, seen) || !inModule && reaches(sig.Results(), seen) {
				return true
			}
		}
	}
	if st, ok := n.Underlying().(*types.Struct); ok {
		return reachesFields(st, seen)
	}
	return false
}

// exportedFields is every exported field a holder of a struct reaches by
// name: its own, and those promoted from an embedded struct — an unexported
// embedded type's exported fields included — at any depth.
func exportedFields(st *types.Struct) []*types.Var {
	var out []*types.Var
	seen := map[*types.Struct]bool{}
	var walk func(*types.Struct)
	walk = func(st *types.Struct) {
		if seen[st] {
			return
		}
		seen[st] = true
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if f.Exported() {
				out = append(out, f)
			}
			if !f.Embedded() {
				continue
			}
			t := f.Type()
			if p, ok := t.(*types.Pointer); ok {
				t = p.Elem()
			}
			if inner, ok := t.Underlying().(*types.Struct); ok {
				walk(inner)
			}
		}
	}
	walk(st)
	return out
}

// readingSurface names every exported object of a reading package whose
// type reaches a reading: "<dir> <name>" for a function, "<dir> var <name>",
// "<dir> type <name>" for an alias, "<dir> (<T>).<m>" for a method and
// "<dir> <T>.<field>" for a field of a type that is no reading itself.
func readingSurface(p *modtypes.Package) []string {
	dir := strings.TrimPrefix(p.ImportPath, modulePath+"/")
	var out []string
	check := func(key string, t types.Type) {
		if reaches(t, map[types.Type]bool{}) {
			out = append(out, key)
		}
	}
	scope := p.Types.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		switch o := obj.(type) {
		case *types.Func:
			check(dir+" "+name, o.Type())
		case *types.Var:
			check(dir+" var "+name, o.Type())
		case *types.TypeName:
			if o.IsAlias() {
				check(dir+" type "+name, o.Type())
				continue
			}
			named, ok := o.Type().(*types.Named)
			if !ok || sensitive(o) {
				continue
			}
			// Methods and fields at every depth: one promoted from an embedded
			// type — an unexported one included — is the type's own to a
			// holder.
			for _, recv := range []types.Type{named, types.NewPointer(named)} {
				ms := types.NewMethodSet(recv)
				for i := 0; i < ms.Len(); i++ {
					fn := ms.At(i).Obj()
					if fn.Exported() && ms.At(i).Kind() == types.MethodVal {
						check(dir+" ("+name+")."+fn.Name(), fn.Type())
					}
				}
			}
			if st, ok := named.Underlying().(*types.Struct); ok {
				for _, f := range exportedFields(st) {
					check(dir+" "+name+"."+f.Name(), f.Type())
				}
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// TestEveryReadingAPIIsListed holds the export surface of the reading
// packages on the types the checker resolves — a field, a method value, a
// callback's parameter, an alias, a channel, an anonymous struct, a
// variable, any value at all: an export that hands a reading over is a new
// entry in readingAPI, never a silent export.
func TestEveryReadingAPIIsListed(t *testing.T) {
	m := theModule(t)
	seen := map[string]bool{}
	for _, path := range readingPackages {
		p := m.Package(path)
		if p == nil || p.Types == nil {
			t.Fatalf("%s is no package of the module: its export surface is held by nothing", path)
		}
		for _, key := range readingSurface(p) {
			seen[key] = true
			if _, ok := readingAPI[key]; !ok {
				t.Errorf("%s hands a reading over (its type reaches an AST, a unit, a unit file, a parse or author result, or any value): list it in readingAPI with why it never hands out a reading a bound refused", key)
			}
		}
	}
	for key := range readingAPI {
		if !seen[key] {
			t.Errorf("readingAPI lists %s, which no longer hands out a reading: remove the entry", key)
		}
	}
}

// unsafeFiles are the production files that import unsafe, each with why:
// a conversion through unsafe.Pointer reads any value as any other — a view
// as the unit it wraps — and no type rule holds it.
var unsafeFiles = map[string]string{
	"pkg/server/assistant_authoring_local_windows.go": "the NT rename's FILE_RENAME_INFORMATION buffer (NtSetInformationFile)",
	"pkg/authoringlock/platform_windows.go":           "the NT object attributes' Length (unsafe.Sizeof) and the ACL entry's SID read through unsafe.Pointer",
	"pkg/cli/tty_darwin.go":                           "the terminal's ioctl (TIOCGETA) argument",
	"pkg/cli/tty_linux.go":                            "the terminal's ioctl (TCGETS) argument",
	"pkg/cli/tty_windows.go":                          "the console mode's syscall argument",
	"cmd/iterion-desktop/prgname_linux.go":            "the C string handed to g_set_prgname",
	"cmd/iterion-desktop/window_icon_linux.go":        "the GTK window handle the icon is set on",
}

// reflectPointerFiles are the production files that turn a reflect.Value
// into a pointer or a pointer into a value — reflect.NewAt, and
// Value.UnsafePointer, UnsafeAddr, Pointer — each with why: with no unsafe
// import, UnsafePointer and NewAt read an unexported field — a view's unit —
// as a value of its type, and the read-only mark of the field does not hold
// it. None does today.
var reflectPointerFiles = map[string]string{}

// reflectPointerMethods are the reflect.Value methods that hand out an
// address: a pointer to read any value as any type through.
var reflectPointerMethods = map[string]bool{"UnsafePointer": true, "UnsafeAddr": true, "Pointer": true}

// otherBuildFiles are the production files this platform's build excludes
// (another GOOS, a build tag: windows, darwin, desktop) that import a
// reading door — pkg/dsl/unit, author, canon, parser, ast — or reflect,
// each with why: the type-checked guards read this platform's build alone,
// so such a file is held here by its imports. None does today.
var otherBuildFiles = map[string]string{}

// otherBuildDoors are the imports that put a file another build compiles
// under otherBuildFiles.
var otherBuildDoors = map[string]bool{
	unitPackagePath:               true,
	authorPackagePath:             true,
	parserPackagePath:             true,
	astPackagePath:                true,
	modulePath + "/pkg/dsl/canon": true,
	"reflect":                     true,
}

// reflectPointerUses names, by file, the production code of p that calls or
// takes reflect.NewAt or a reflect.Value method of reflectPointerMethods.
func reflectPointerUses(p *modtypes.Package) map[string]bool {
	files := map[string]bool{}
	for id, obj := range p.Info.Uses {
		fn, ok := obj.(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "reflect" {
			continue
		}
		sig, _ := fn.Type().(*types.Signature)
		switch {
		case sig != nil && sig.Recv() == nil && fn.Name() == "NewAt":
		case sig != nil && sig.Recv() != nil && types.TypeString(sig.Recv().Type(), nil) == "reflect.Value" && reflectPointerMethods[fn.Name()]:
		default:
			continue
		}
		if i := fileIndex(p, id.Pos()); i >= 0 && !strings.HasSuffix(p.FileRels[i], "_test.go") {
			files[p.FileRels[i]] = true
		}
	}
	return files
}

// fileIndex is the index in p.Files of the file holding pos, -1 for none.
func fileIndex(p *modtypes.Package, pos token.Pos) int {
	for i, f := range p.Files {
		if f.FileStart <= pos && pos <= f.FileEnd {
			return i
		}
	}
	return -1
}

// TestNoGuardIsReadAroundByTheBuild holds what the guards above read: no
// //go:linkname reaches into the language's packages, unsafe is imported
// only by the files listed in unsafeFiles, reflect's pointer doors are used
// only by the files listed in reflectPointerFiles, a file another build
// compiles imports no reading door unless otherBuildFiles lists it, no
// production package depends on a test helper (the build's own dependency
// graph, transitively), and every Go file of the repository — tracked, or
// untracked and not ignored — is a file of a package of the module, or a
// fixture under testdata/: a file in a directory the build does not list (a
// hidden one, a stray tree) is read by no guard.
func TestNoGuardIsReadAroundByTheBuild(t *testing.T) {
	m := theModule(t)
	held := map[string]bool{}
	built := map[string]bool{}
	for _, p := range m.Packages {
		for _, rel := range p.FileRels {
			built[rel] = true
		}
		for _, rel := range append(append([]string{}, p.FileRels...), p.IgnoredGoFiles...) {
			held[rel] = true
		}
		if !testOnlyPackages[p.ImportPath] {
			for _, dep := range p.Deps {
				if testOnlyPackages[dep] {
					t.Errorf("%s depends on %s, a test helper that panics on a refusal: production code reads parser.Parse / the unit loaders and answers their error", p.ImportPath, dep)
				}
			}
		}
	}
	reflectSeen := map[string]bool{}
	for _, p := range m.Packages {
		if p.Info == nil {
			continue
		}
		for rel := range reflectPointerUses(p) {
			reflectSeen[rel] = true
			if _, ok := reflectPointerFiles[rel]; !ok {
				t.Errorf("%s turns a reflect.Value into a pointer or back (reflect.NewAt, Value.UnsafePointer, UnsafeAddr, Pointer): list it in reflectPointerFiles with why — with no unsafe import they read a view of a load as the unit it wraps", rel)
			}
		}
	}
	for rel := range reflectPointerFiles {
		if !reflectSeen[rel] {
			t.Errorf("%s is listed in reflectPointerFiles but uses none of reflect's pointer doors: remove the entry", rel)
		}
	}
	unsafeSeen := map[string]bool{}
	otherSeen := map[string]bool{}
	walkModuleSources(t, func(rel string, src []byte) {
		f, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case path == "unsafe":
				unsafeSeen[rel] = true
				if _, ok := unsafeFiles[rel]; !ok {
					t.Errorf("%s imports unsafe: list it in unsafeFiles with why — a conversion through unsafe.Pointer reads a view of a load as the unit it wraps", rel)
				}
			case testOnlyPackages[path] && !built[rel]:
				// A file this platform's build excludes is in no dependency
				// graph go list answers here.
				t.Errorf("%s imports %s, a test helper that panics on a refusal", rel, path)
			case otherBuildDoors[path] && !built[rel]:
				otherSeen[rel] = true
				if _, ok := otherBuildFiles[rel]; !ok {
					t.Errorf("%s, compiled by another build than this one (another GOOS, a build tag), imports %s, which no type-checked guard reads there: list it in otherBuildFiles with what it does with the reading", rel, path)
				}
			}
		}
		for _, target := range linknames(f) {
			if strings.HasPrefix(target, modulePath+"/pkg/dsl/") {
				t.Errorf("%s: //go:linkname reaches %s, around every door of the language's packages", rel, target)
			}
		}
	})
	for rel := range unsafeFiles {
		if !unsafeSeen[rel] {
			t.Errorf("%s is listed in unsafeFiles but imports no unsafe: remove the entry", rel)
		}
	}
	for rel := range otherBuildFiles {
		if !otherSeen[rel] {
			t.Errorf("%s is listed in otherBuildFiles but is built here or imports no reading door: remove the entry", rel)
		}
	}
	cmd := exec.Command("git", git.NoAutoMaintenance("-C", m.Root, "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", "*.go")...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files did not answer, so the repository's Go files were not held to a package: %v", err)
	}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") || held[rel] || strings.HasPrefix(rel, "vendor/") {
			continue
		}
		if strings.Contains("/"+rel, "/testdata/") || inNestedModule(m.Root, rel) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(m.Root, filepath.FromSlash(rel))); err != nil {
			continue // listed by the index, gone from the tree
		}
		t.Errorf("%s is a Go file no package of the module holds (a directory the build does not list): no guard reads it — move it into a package, or under testdata/", rel)
	}
}

// inNestedModule reports whether the file at rel belongs to another module:
// a directory between it and the root holds a go.mod (the module's own ./...
// stops there, and that module answers for its files).
func inNestedModule(root, rel string) bool {
	for dir := filepath.Dir(filepath.FromSlash(rel)); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(root, dir, "go.mod")); err == nil {
			return true
		}
	}
	return false
}

// linknames are the targets of a file's //go:linkname directives.
func linknames(f *ast.File) []string {
	var out []string
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if fields := strings.Fields(c.Text); len(fields) >= 3 && fields[0] == "//go:linkname" {
				out = append(out, fields[2])
			}
		}
	}
	return out
}

// The detectors find a door however it is spelled — and only the door of
// the package it names, not a same-named function or field of another.
func TestSalvageReferencesAreFoundWhateverTheSpelling(t *testing.T) {
	m := theModule(t)
	for name, tc := range map[string]struct {
		src  string
		want string
	}{
		"unit salvage":  {"package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/unit\"\nfunc f() { _ = unit.Salvage(unit.LoadDir(\"m\")) }\n", "x.go f Salvage unit.Salvage(unit.LoadDir(\"m\"))"},
		"alias":         {"package x\nimport u \"github.com/SocialGouv/iterion/pkg/dsl/unit\"\nvar g = u.Salvage\n", "x.go var g Salvage u.Salvage"},
		"dot unit":      {"package x\nimport . \"github.com/SocialGouv/iterion/pkg/dsl/unit\"\nfunc f() { _ = Salvage(nil, nil) }\n", "x.go f Salvage Salvage(nil, nil)"},
		"author parse":  {"package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/author\"\nfunc f() { _ = author.Parse(\"a\", nil) }\n", "x.go f Parse author.Parse(\"a\", nil)"},
		"lexer":         {"package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\nfunc f() { _ = parser.NewLexer(\"a\", \"b\").All() }\n", "x.go f NewLexer parser.NewLexer(\"a\", \"b\")"},
		"second use":    {"package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/unit\"\nfunc f() { _ = unit.Salvage(nil, nil); _ = unit.Salvage(nil, nil) }\n", "x.go f Salvage unit.Salvage(nil, nil),x.go f Salvage unit.Salvage(nil, nil) #2"},
		"another call":  {"package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/unit\"\nfunc f(a, b error) { _ = unit.Salvage(nil, b); _ = unit.Salvage(nil, a) }\n", "x.go f Salvage unit.Salvage(nil, a),x.go f Salvage unit.Salvage(nil, b)"},
		"receivers":     {"package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/unit\"\ntype A struct{}\ntype B struct{}\nfunc (A) open() { _ = unit.Salvage(nil, nil) }\nfunc (*B) open() { _ = unit.Salvage(nil, nil) }\n", "x.go (*B).open Salvage unit.Salvage(nil, nil),x.go (A).open Salvage unit.Salvage(nil, nil)"},
		"strict":        {"package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\nfunc f() { parser.Parse(\"a\", \"b\") }\n", ""},
		"other package": {"package x\nimport \"go/parser\"\nfunc f() { parser.ParseFile(nil, \"\", nil, 0) }\n", ""},
		"a field":       {"package x\nfunc f(v struct{ Salvage int }) { _ = v.Salvage }\n", ""},
		"prose only":    {"package x\nimport \"github.com/SocialGouv/iterion/pkg/dsl/unit\"\n// unit.Salvage( is prose\nvar _ = unit.FragmentDir\n", ""},
	} {
		p, err := m.CheckSource("example.com/x", "x.go", tc.src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := salvageReferences(m, p)
		sort.Strings(got)
		if strings.Join(got, ",") != tc.want {
			t.Errorf("%s: found %q, want %q", name, strings.Join(got, ","), tc.want)
		}
	}
	// A view handed to a helper makes the helper a holder, under an alias
	// of its type too; a decoding there is said.
	p, err := m.CheckSource("example.com/x", "x.go", "package x\nimport (\n\t\"github.com/SocialGouv/iterion/pkg/dsl/ast\"\n\t\"github.com/SocialGouv/iterion/pkg/dsl/unit\"\n)\ntype V = unit.Salvaged\nfunc helper(v *V) { b, _ := v.MarshalDocument(\"\"); _, _ = ast.UnmarshalFile(b) }\nfunc other() {}\n")
	if err != nil {
		t.Fatal(err)
	}
	holders, decodes := viewHolders(m, p)
	if strings.Join(holders, ",") != "x.go helper" || strings.Join(decodes, ",") != "x.go helper" {
		t.Errorf("view holders %v, decoding %v — want the helper, holding and decoding", holders, decodes)
	}
	// Every shape that hands a reading over reaches it.
	p, err = m.CheckSource("example.com/x", "x.go", `package x
import (
	"context"
	"reflect"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)
type Program = ast.File
type View struct{ AST *ast.File; n int }
type Plain struct{ N int }
func (View) Raw() any                         { return nil }
func (View) Visit(func(*unit.Unit))           {}
func (View) Program() *Program                { return nil }
func (View) Agents() []*ast.AgentDecl         { return nil }
func (View) Inner() struct{ U *unit.Unit }    { return struct{ U *unit.Unit }{} }
func (View) Chan() <-chan *unit.Unit          { return nil }
func (View) Whole() bool                      { return true }
func (*View) Err() error                      { return nil }
var Last *unit.Unit
var Count int
type handle struct{ U *unit.Unit }
func (handle) Unit() *unit.Unit               { return nil }
type inner struct{ n int }
func (inner) Unit() *unit.Unit                { return nil }
type Promoted struct{ handle }
type Opaque struct{ n int }
func (Opaque) Inner() inner                   { return inner{} }
func (Opaque) Value() reflect.Value           { return reflect.Value{} }
func (Opaque) Context() context.Context       { return nil }
func (Opaque) Name() string                   { return "" }
`)
	if err != nil {
		t.Fatal(err)
	}
	want := "example.com/x (Opaque).Context,example.com/x (Opaque).Inner,example.com/x (Opaque).Value,example.com/x (Promoted).Unit,example.com/x (View).Agents,example.com/x (View).Chan,example.com/x (View).Inner,example.com/x (View).Program,example.com/x (View).Raw,example.com/x (View).Visit,example.com/x Promoted.U,example.com/x View.AST,example.com/x type Program,example.com/x var Last"
	if got := strings.Join(readingSurface(p), ","); got != want {
		t.Errorf("the reading surface of the shapes = %s, want %s", got, want)
	}
}
