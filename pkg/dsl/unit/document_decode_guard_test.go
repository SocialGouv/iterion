package unit

// The decode of a document has ONE door: ast.UnmarshalFile — which
// *ast.File's own UnmarshalJSON now routes every encoding/json decode of a
// File through, so even a decode no rule sees meets the refusal of a
// document marked a salvage (MarshalSalvageWithProvenance, ast.ErrSalvage).
// What no runtime can hold is a decode that loses the mark before a File is
// built: bytes re-encoded through a typed mirror of the keys first (the
// mirror carries no "salvage" field, the re-marshal drops it, and
// UnmarshalFile decodes an unmarked document). That shape is held
// statically: no production code outside pkg/dsl/ast decodes a document, or
// re-marshals bytes and decodes them, without being listed here — the rule
// is type-checked, by file and function: alias and local renames,
// dot-imported calls, decoder variables and chained NewDecoder(...).Decode
// calls, Named holders whose underlying struct carries the ast type,
// parenthesized callees, and a callee held in a value one hop from its
// assignment — a captured method value (d := dec.Decode), a hoisted method
// expression (d := (*json.Decoder).Decode), a func-typed local or a
// func-typed field the body initializes (H{unmarshal: json.Unmarshal}) —
// are seen whatever they are called. A decode whose target the body cannot
// name — a type parameter, whose concrete type is fixed at an instantiation
// the checker records outside the body (a generic helper decoding into T) —
// is judged at the instantiation sites the module's own packages pin: an
// instance pinned to an ast type lists the function, a generic never
// instantiated with one stays unlisted. What stays unlisted: a decode
// reached only through reflection, and an ast instantiation made outside
// the module — the rule reads this module's own packages only.
// (TestSalvageReferencesAreFoundWhateverTheSpelling's method.)

import (
	"go/ast"
	"go/types"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/modtypes"
)

// isASTType reports whether a type name is declared in pkg/dsl/ast: a
// document type, or anything carrying one.
func isASTType(tn *types.TypeName) bool {
	return tn.Pkg() != nil && tn.Pkg().Path() == astPackagePath
}

// calleeOf resolves a call's Fun to "<package path> <name>" — "encoding/json
// Unmarshal", "pkg/dsl/ast UnmarshalFile", "encoding/json (*Decoder).Decode"
// — "" for a call the type checker did not resolve (a builtin, a conversion).
// A qualified call's selector is a use; a method call through a value
// (dec.Decode(...)) is a selection; a dot-imported call is the bare
// identifier itself — the same decode spells all three. A parenthesized
// callee unwraps. A callee held in a value — a captured method value
// (d := dec.Decode; d(&f)), a hoisted method expression
// (d := (*json.Decoder).Decode; d(dec, &f)), a func-typed local, or a
// func-typed field the body initializes (h.unmarshal(b, &f) over
// H{unmarshal: json.Unmarshal}) — resolves one hop through the body's own
// assignments (holds); a callee the hop does not name answers "".
func calleeOf(p *modtypes.Package, fun ast.Expr, holds funcHolds) string {
	for {
		pe, ok := fun.(*ast.ParenExpr)
		if !ok {
			break
		}
		fun = pe.X
	}
	switch e := fun.(type) {
	case *ast.Ident:
		// a dot-imported call: the identifier IS the callee.
		if fn, ok := p.Info.Uses[e].(*types.Func); ok {
			return calleeName(fn)
		}
		if fn := heldFunc(p, e, holds, 0); fn != nil {
			return calleeName(fn)
		}
		return ""
	case *ast.SelectorExpr:
		obj := p.Info.Uses[e.Sel]
		if obj == nil {
			// a method call through a value is a selection, not a use; a
			// field's object is a *types.Var, no callee of its own.
			if s, ok := p.Info.Selections[e]; ok {
				obj = s.Obj()
			}
		}
		if fn, ok := obj.(*types.Func); ok {
			return calleeName(fn)
		}
		if fn := heldFunc(p, e, holds, 0); fn != nil {
			return calleeName(fn)
		}
		return ""
	default:
		return ""
	}
}

// funcHolds maps, for one function body, each variable a call's callee can
// name — a local, a func-typed field's object — to the right-side
// expressions the body assigns it, in source order: the one-hop dataflow a
// held callee resolves through.
type funcHolds map[types.Object][]ast.Expr

// holdsOf collects funcHolds for one body: the single-name declaration or
// assignment (x := f, x = f), the field assignment (h.field = f), and the
// keyed composite literal's field (H{unmarshal: f} — the key is a use of
// the field's Var, and only a field: a map literal's keys stay out).
func holdsOf(p *modtypes.Package, body ast.Node) funcHolds {
	holds := funcHolds{}
	add := func(obj types.Object, rhs ast.Expr) {
		if obj == nil || rhs == nil {
			return
		}
		holds[obj] = append(holds[obj], rhs)
	}
	assign := func(lhs, rhs ast.Expr) {
		switch v := lhs.(type) {
		case *ast.Ident:
			if obj := p.Info.Defs[v]; obj != nil {
				add(obj, rhs)
			} else {
				add(p.Info.Uses[v], rhs)
			}
		case *ast.SelectorExpr:
			if s, ok := p.Info.Selections[v]; ok {
				add(s.Obj(), rhs)
			}
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if len(v.Lhs) != 1 || len(v.Rhs) != 1 {
				return true
			}
			assign(v.Lhs[0], v.Rhs[0])
		case *ast.CompositeLit:
			for _, elt := range v.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				if fv, ok := p.Info.Uses[key].(*types.Var); !ok || !fv.IsField() {
					continue
				}
				add(p.Info.Uses[key], kv.Value)
			}
		}
		return true
	})
	return holds
}

// heldFunc resolves a callee held in a value: the object the expression
// names (a local, a field), read through the body's assignments — the last
// assigned right side the hop resolves to a function value wins (the call
// reads the value the body last gave it) — one bounded chain deep
// (u := json.Unmarshal; v := u; v(...)).
func heldFunc(p *modtypes.Package, e ast.Expr, holds funcHolds, depth int) *types.Func {
	if depth > 3 || holds == nil {
		return nil
	}
	var obj types.Object
	switch v := e.(type) {
	case *ast.Ident:
		obj = p.Info.Uses[v]
		if obj == nil {
			obj = p.Info.Defs[v]
		}
	case *ast.SelectorExpr:
		if s, ok := p.Info.Selections[v]; ok {
			obj = s.Obj()
		} else {
			obj = p.Info.Uses[v.Sel]
		}
	default:
		return nil
	}
	if obj == nil {
		return nil
	}
	var fn *types.Func
	for _, rhs := range holds[obj] {
		if f := assignedFunc(p, rhs, holds, depth+1); f != nil {
			fn = f
		}
	}
	return fn
}

// assignedFunc names the function value an assignment's right side carries:
// a selector the checker resolved to a *types.Func (json.Unmarshal, a
// method value dec.Decode, a method expression (*json.Decoder).Decode),
// parenthesized forms unwrapped, or — one hop deeper — another held
// variable named in the same body.
func assignedFunc(p *modtypes.Package, e ast.Expr, holds funcHolds, depth int) *types.Func {
	for {
		pe, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = pe.X
	}
	switch v := e.(type) {
	case *ast.SelectorExpr:
		if fn, ok := p.Info.Uses[v.Sel].(*types.Func); ok {
			return fn
		}
		if s, ok := p.Info.Selections[v]; ok {
			fn, _ := s.Obj().(*types.Func)
			return fn
		}
		return nil
	case *ast.Ident:
		if fn, ok := p.Info.Uses[v].(*types.Func); ok {
			return fn
		}
		if depth > 3 {
			return nil
		}
		return heldFunc(p, v, holds, depth)
	default:
		return nil
	}
}

// calleeName formats a resolved callee the way decodeTargetType reads it:
// "<package path> <name>" for a function, "<package path> (*T).<name>" for a
// method — the receiver spelled with its star whatever the declaration said,
// the one form the consumers match.
func calleeName(fn *types.Func) string {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil || sig.Recv().Type() == nil {
		return fn.Pkg().Path() + " " + fn.Name()
	}
	recv := sig.Recv().Type()
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = ptr.Elem()
	}
	named, ok := recv.(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return ""
	}
	return fn.Pkg().Path() + " (*" + named.Obj().Name() + ")." + fn.Name()
}

// decodeTargetType is the value a decode fills. Whatever the spelling — a
// direct json.Unmarshal(data, v), a decoder's Decode(v), a held method
// value or field, a method expression with its receiver passed, a
// parenthesized callee — the target sits at the callee type's LAST
// empty-interface (any) parameter: json.Unmarshal(data []byte, v any)
// answers 1, (*json.Decoder).Decode(v any) 0, the hoisted method expression
// (*json.Decoder, any) 1. nil when the callee's type names none.
func decodeTargetType(p *modtypes.Package, call *ast.CallExpr) types.Type {
	tv, ok := p.Info.Types[call.Fun]
	if !ok {
		return nil
	}
	sig, ok := tv.Type.(*types.Signature)
	if !ok {
		return nil
	}
	params := sig.Params()
	arg := -1
	for i := 0; i < params.Len(); i++ {
		if b, ok := params.At(i).Type().Underlying().(*types.Interface); ok && b.Empty() {
			arg = i
		}
	}
	if arg < 0 || arg >= len(call.Args) {
		return nil
	}
	if at, ok := p.Info.Types[call.Args[arg]]; ok {
		return at.Type
	}
	return nil
}

// refusesToJudge reports a decode target the body cannot name: a type
// parameter, whose concrete type is fixed at an instantiation the checker
// records at the instantiation sites, not in the body. The body alone
// cannot judge — but the module can: the type arguments the instantiation
// sites pin are checked against pkg/dsl/ast (instantiatedWithAST), and the
// function is listed only then. Flagged rather than passed: a decode into a
// type parameter is the shape the listing exists for, and an unjudged pass
// would leave it unlisted.
func refusesToJudge(t types.Type, instances map[string][]*types.TypeList, generic types.Object) bool {
	for {
		t = types.Unalias(t)
		ptr, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = ptr.Elem()
	}
	if _, ok := t.(*types.TypeParam); !ok {
		return false
	}
	return instantiatedWithAST(instances, generic)
}

// genericInstances collects, per package, the type arguments each
// instantiation site pins: keyed by the generic's ORIGIN func spelled
// "<package path> <name>" — the checker hands an instantiation in another
// package the export-data object, so identity is the spelled origin, not
// the pointer.
func genericInstances(p *modtypes.Package) map[string][]*types.TypeList {
	out := map[string][]*types.TypeList{}
	for _, f := range p.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			inst, ok := p.Info.Instances[id]
			if !ok {
				return true
			}
			// the site's ident is a use of the generic itself: that object
			// names the key, the instance only carries the pinned arguments.
			fn, ok := p.Info.Uses[id].(*types.Func)
			if !ok {
				return true
			}
			if fn == nil || fn.Pkg() == nil {
				return true
			}
			if inst.TypeArgs == nil || inst.TypeArgs.Len() == 0 {
				return true
			}
			key := fn.Pkg().Path() + " " + fn.Name()
			out[key] = append(out[key], inst.TypeArgs)
			return true
		})
	}
	return out
}

// instantiatedWithAST reports whether any recorded instantiation of the
// generic pins one of its type arguments to a type mentions reports on — a
// pkg/dsl/ast type here. A generic never instantiated in the scanned module
// (a helper exported and unused) is not listed: nothing decodes through it.
func instantiatedWithAST(instances map[string][]*types.TypeList, generic types.Object) bool {
	if generic == nil || generic.Pkg() == nil {
		return false
	}
	key := generic.Pkg().Path() + " " + generic.Name()
	for _, args := range instances[key] {
		for i := 0; i < args.Len(); i++ {
			if mentions(args.At(i), isASTType, map[types.Type]bool{}) {
				return true
			}
		}
	}
	return false
}

// documentDecodes names, by "<file> <function>", every production function
// of the package outside pkg/dsl/ast that
//
//   - fills a value whose type reaches pkg/dsl/ast with encoding/json's own
//     decode (Unmarshal, Decoder.Decode): the document decode goes through
//     the one door, by name, so a reader of this list can see every caller
//     that would write what a decode produced;
//   - marshals bytes with encoding/json AND decodes them as a document
//     (pkg/dsl/ast Unmarshal*) in the same function: the re-encode through
//     a mirror of the keys is how a salvage mark is dropped before any File
//     is built — the one shape no runtime refusal reaches.
func documentDecodes(p *modtypes.Package, instances map[string][]*types.TypeList) (intoAST, remarshal []string) {
	for i, f := range p.Files {
		if strings.HasSuffix(p.FileRels[i], "_test.go") {
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := p.FileRels[i] + " " + funcDeclName(fd)
			holds := holdsOf(p, fd.Body)
			decodesDoc := false
			marshals := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := calleeOf(p, call.Fun, holds)
				switch callee {
				case "encoding/json Unmarshal", "encoding/json (*Decoder).Decode":
					t := decodeTargetType(p, call)
					if t == nil {
						return true
					}
					if mentions(t, isASTType, map[types.Type]bool{}) {
						intoAST = append(intoAST, name)
						return true
					}
					if refusesToJudge(t, instances, p.Info.Defs[fd.Name]) {
						intoAST = append(intoAST, name)
					}
				case "encoding/json Marshal", "encoding/json MarshalIndent", "encoding/json (*Encoder).Encode":
					marshals = true
				default:
					if strings.HasPrefix(callee, astPackagePath+" Unmarshal") {
						decodesDoc = true
					}
				}
				return true
			})
			if decodesDoc && marshals {
				remarshal = append(remarshal, name)
			}
		}
	}
	sort.Strings(intoAST)
	sort.Strings(remarshal)
	return intoAST, remarshal
}

// funcDeclName names a declaration the way the guards write a holder:
// "f", "(A) open", "(*A) open".
func funcDeclName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	star := ""
	if t, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
		star = "*"
		fd.Recv.List[0].Type = t.X
	}
	if t, ok := fd.Recv.List[0].Type.(*ast.Ident); ok {
		return "(" + star + t.Name + ") " + fd.Name.Name
	}
	return fd.Name.Name
}

func TestADocumentIsDecodedThroughItsOneDoor(t *testing.T) {
	m := theModule(t)
	// The instantiations every package of the module pins, pooled so a
	// generic declared in one package and instantiated with an ast type in
	// another is judged there too (the checker keys the site's instance by
	// the generic's spelled object, not by pointer identity).
	instances := map[string][]*types.TypeList{}
	for _, p := range m.Packages {
		if p.Info == nil {
			continue
		}
		for key, args := range genericInstances(p) {
			instances[key] = append(instances[key], args...)
		}
	}
	for _, p := range m.Packages {
		if p.Info == nil || p.ImportPath == astPackagePath {
			// pkg/dsl/ast is the door: its own decode of the mirror is what
			// every other decode of a document must go through.
			continue
		}
		intoAST, remarshal := documentDecodes(p, instances)
		for _, k := range intoAST {
			t.Errorf("%s decodes a document with encoding/json's own decode into a pkg/dsl/ast type: decode it through ast.UnmarshalFile (the one door), or list it here with why it never writes what it decoded", k)
		}
		for _, k := range remarshal {
			t.Errorf("%s re-marshals bytes and decodes them as a document in the same function: a mirror of the keys drops the salvage mark the decode refuses — decode the document once, through ast.UnmarshalFile", k)
		}
	}
	// The shapes the rule exists for — the four r7 plants, the five r8 ones
	// and the five r9 ones, each of which decodes a document into a
	// pkg/dsl/ast type outside the one door:
	for name, tc := range map[string]struct {
		src  string
		kind string
	}{
		"jsondirect": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
func helper(b []byte, path string) error {
	var f ast.File
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	return nil
}
`, "intoAST"},
		"holderjson": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)
func holder(s *unit.Salvaged) {
	b, _ := s.MarshalDocument("")
	var f ast.File
	_ = json.Unmarshal(b, &f)
}
`, "intoAST"},
		"chanjson": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
func relay(ch <-chan []byte) error {
	var f ast.File
	if err := json.Unmarshal(<-ch, &f); err != nil {
		return err
	}
	return nil
}
`, "intoAST"},
		"mapstrip": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
func strip(b []byte) error {
	var mirror map[string]json.RawMessage
	_ = json.Unmarshal(b, &mirror)
	delete(mirror, "salvage")
	normalized, _ := json.Marshal(mirror)
	_, err := ast.UnmarshalFile(normalized)
	return err
}
`, "remarshal"},
		// the r8 plants: the spellings the rule answered "" on
		"dotjson": {`package x
import (
	. "encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
func helper(b []byte) error {
	var f ast.File
	return Unmarshal(b, &f)
}
`, "intoAST"},
		"decoder": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
func helper(r interface{ Read([]byte) (int, error) }) error {
	var f ast.File
	return json.NewDecoder(r).Decode(&f)
}
`, "intoAST"},
		"decvar": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
func helper(r interface{ Read([]byte) (int, error) }) error {
	dec := json.NewDecoder(r)
	var f ast.File
	return dec.Decode(&f)
}
`, "intoAST"},
		"embedfile": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
type Holder struct {
	ast.File
	Note string
}
func helper(b []byte) error {
	var h Holder
	return json.Unmarshal(b, &h)
}
`, "intoAST"},
		"embedalias": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
type Holder struct {
	ast.File
}
func helper(b []byte) error {
	var h Holder
	return json.Unmarshal(b, &h)
}
`, "intoAST"},
		// the r9 plants: the spellings the rule answered "" on before the
		// one-hop dataflow
		"methodvalue": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
func helper(r interface{ Read([]byte) (int, error) }) error {
	dec := json.NewDecoder(r)
	d := dec.Decode
	var f ast.File
	return d(&f)
}
`, "intoAST"},
		"methodexpr": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
func helper(r interface{ Read([]byte) (int, error) }) error {
	d := (*json.Decoder).Decode
	dec := json.NewDecoder(r)
	var f ast.File
	return d(dec, &f)
}
`, "intoAST"},
		"parencallee": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
func helper(r interface{ Read([]byte) (int, error) }) error {
	dec := json.NewDecoder(r)
	var f ast.File
	return (dec.Decode)(&f)
}
`, "intoAST"},
		"generictarget": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
func grab[T any](r interface{ Read([]byte) (int, error) }) error {
	dec := json.NewDecoder(r)
	var v T
	return dec.Decode(&v)
}
var _ = grab[ast.File]
`, "intoAST"},
		"funcfield": {`package x
import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)
type holder struct {
	unmarshal func([]byte, any) error
}
func helper(b []byte) error {
	h := holder{unmarshal: json.Unmarshal}
	var f ast.File
	return h.unmarshal(b, &f)
}
`, "intoAST"},
	} {
		p, err := m.CheckSource("example.com/x", "x.go", tc.src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		intoAST, remarshal := documentDecodes(p, genericInstances(p))
		got := strings.Join(append(append([]string{}, intoAST...), remarshal...), ",")
		if got == "" {
			t.Errorf("%s: the rule sees none of the shape (want it under %s)", name, tc.kind)
			continue
		}
		if tc.kind == "intoAST" && len(intoAST) == 0 || tc.kind == "remarshal" && len(remarshal) == 0 {
			t.Errorf("%s: the rule names the shape but not as %s (intoAST=%v remarshal=%v)", name, tc.kind, intoAST, remarshal)
		}
	}
	// The one door stays unseen: a decode THROUGH ast.UnmarshalFile of bytes
	// a caller handed over names nothing — and pkg/dsl/ast's own mirror
	// decode is the door's implementation, never an offender.
	p, err := m.CheckSource("example.com/x", "x.go", `package x
import "github.com/SocialGouv/iterion/pkg/dsl/ast"
func door(b []byte) error {
	_, err := ast.UnmarshalFile(b)
	return err
}
`)
	if err != nil {
		t.Fatal(err)
	}
	intoAST, remarshal := documentDecodes(p, genericInstances(p))
	if len(intoAST) != 0 || len(remarshal) != 0 {
		t.Errorf("the one door is named: %v / %v", intoAST, remarshal)
	}
}
