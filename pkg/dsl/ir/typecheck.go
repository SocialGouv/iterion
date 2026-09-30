package ir

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// ---------------------------------------------------------------------------
// Static cross-node typing (Phase 2): C121 / C107 / C108 / C120
//
// This pass is conservative by construction: every inference bails to
// "unknown" on the slightest doubt (a json field, an unresolved ref, a
// builtin whose element type we can't see). It NEVER inspects values that
// reach a template-stringification context (prompts, tool commands, with-Raw)
// — only the operands of compute/when EXPRESSIONS and enum-typed
// comparisons, where the field TYPE genuinely matters at runtime.
//
// It is distinct from the runtime conformance check
// (pkg/backend/model.ValidateOutput): that validates the ACTUAL LLM output
// against the output schema at run time; this validates AUTHOR INTENT in the
// expression source at compile time.
// ---------------------------------------------------------------------------

// validateExprTypes type-checks every `when "expr"` edge and every
// compute-node expression. It deliberately mirrors the edge walk in
// validateConditionFields (which validates the simple `when <field>` form and
// expression ref existence) — keep the two in sync if either changes.
func (c *compiler) validateExprTypes(w *Workflow) {
	// Edge `when "expr"` forms. The runtime exposes the SOURCE node's output
	// as both `outputs.<source>` and the bare `input` namespace, so an
	// `input.X` ref here resolves against the source node's OUTPUT schema.
	for _, e := range w.Edges {
		if e.Expression == nil {
			continue
		}
		src, ok := w.Nodes[e.From]
		if !ok {
			continue
		}
		env := exprEnv{w: w, inputSchema: NodeOutputSchema(src)}
		root := expr.ToSnapshot(e.Expression)
		eid := edgeID(e.From, e.To)
		loc := fmt.Sprintf("edge %s -> %s", e.From, e.To)
		c.walkExprTypes(root, env, e.From, eid, loc)

		// C108: a bare numeric `when "count"` is almost certainly a missing
		// comparison — int/float coerce to truthy (non-zero), which is rarely
		// the author's intent. Other bare types are accepted: bool is the
		// normal form, string[]/string ride the documented truthy idiom, and
		// unknown/json bail.
		if root != nil && root.Kind == expr.SnapPath {
			if rt := env.inferType(root); rt.known && (rt.t == FieldTypeInt || rt.t == FieldTypeFloat) {
				c.warnfAt(DiagWhenExprNotBoolish, e.From, eid,
					"edge %s -> %s: when-expression %q is a bare %s value, not a boolean; did you mean a comparison (e.g. > 0)?",
					e.From, e.To, e.Expression.Source(), rt.t)
			}
		}
	}

	// Compute nodes. An `input.X` ref resolves against the node's own input
	// schema.
	for _, n := range w.Nodes {
		cn, ok := n.(*ComputeNode)
		if !ok {
			continue
		}
		env := exprEnv{w: w, inputSchema: cn.InputSchema}
		for _, ce := range cn.Exprs {
			if ce.AST == nil {
				continue
			}
			loc := fmt.Sprintf("compute %q field %q", cn.ID, ce.Key)
			c.walkExprTypes(expr.ToSnapshot(ce.AST), env, cn.ID, "", loc)
			c.checkIntDivision(w, cn, ce, env)
			c.checkCollectionLiteralConform(w, cn, ce, env)
			c.checkEnumMembership(w, cn, ce)
		}
	}
}

// checkIntDivision warns when a compute field typed int is fed by a division
// with an operand the compiler knows to be a float, outside floor()/round()
// (C146): the runtime refuses the fractional value at the node
// (EXPRESSION_FAILED), and only then. An operand it cannot type — a
// function's result, an arithmetic — is not held against the author: the
// runtime divides ints to an int, and the warning must be true when it
// speaks.
func (c *compiler) checkIntDivision(w *Workflow, cn *ComputeNode, ce *ComputeExpr, env exprEnv) {
	schema := w.Schemas[cn.OutputSchema]
	if schema == nil {
		return
	}
	var field *SchemaField
	for _, f := range schema.Fields {
		if f != nil && f.Name == ce.Key {
			field = f
			break
		}
	}
	if field == nil || field.Type != FieldTypeInt {
		return
	}
	if unroundedDivision(expr.ToSnapshot(ce.AST), env) {
		c.warnfAt(DiagIntDivisionUnrounded, cn.ID, "",
			"compute %q field %q is an int and its expression %q divides a float without floor() or round(): the fractional result fails at run time — wrap the division in floor(...) or round(...), or type the field float",
			cn.ID, ce.Key, ce.Raw)
	}
}

// checkEnumMembership warns when a compute field whose output-schema
// declaration carries an enum constraint is fed by a statically-known
// string literal that is not a member (C183): the enum arm of the runtime
// schema check (checkFieldType, pkg/backend/model/validate.go) refuses the
// value at the node — SCHEMA_VALIDATION — so the typo is cheaper to name
// here. A scalar literal is held against a `string` field's enum, an
// all-string list literal against a `string[]` field's (the runtime applies
// the same enum per element); an expression the compiler cannot fully
// evaluate is not held against the author.
func (c *compiler) checkEnumMembership(w *Workflow, cn *ComputeNode, ce *ComputeExpr) {
	schema := w.Schemas[cn.OutputSchema]
	if schema == nil {
		return
	}
	field := findField(schema, ce.Key)
	if field == nil || len(field.EnumValues) == 0 {
		return
	}
	lits, list, ok := staticStringLiterals(expr.ToSnapshot(ce.AST))
	if !ok {
		return
	}
	// Hold the literal against the runtime arm that will actually judge it:
	// a string field's value, or each element of a string[] field. A shape
	// that feeds the other type fails on the type, not the enum — another
	// check's story.
	want := FieldTypeString
	if list {
		want = FieldTypeStringArray
	}
	if field.Type != want {
		return
	}
	for _, lit := range lits {
		if slices.Contains(field.EnumValues, lit) {
			continue
		}
		c.warnfAt(DiagComputeEnumLiteral, cn.ID, "",
			"compute %q field %q is the literal %q, not a member of its enum %v — the value fails schema validation at run time (SCHEMA_VALIDATION); fix the literal or widen the enum",
			cn.ID, ce.Key, lit, field.EnumValues)
	}
}

// staticStringLiterals returns the string values an expression statically
// evaluates to when it is a string literal or an all-string list literal.
// list reports which shape was read, so the caller can hold a scalar
// against a `string` field's enum and a list against a `string[]` field's
// per-element enum, the two arms the runtime checkFieldType enforces. ok is
// false for anything the compiler cannot fully evaluate (a ref, a call, a
// mixed list) — no opinion, no warning.
func staticStringLiterals(n *expr.Snapshot) (lits []string, list, ok bool) {
	if n == nil {
		return nil, false, false
	}
	switch n.Kind {
	case expr.SnapString:
		return []string{n.Str}, false, true
	case expr.SnapList:
		// The collection arm (#1525): an all-string list literal activates
		// the caller's per-element membership check against a string[]
		// field's enum; a non-string element is a type failure, not an
		// enum one, so it stays silent here (C307 owns it).
		lits := make([]string, 0, len(n.Children))
		for _, ch := range n.Children {
			if ch.Kind != expr.SnapString {
				return nil, false, false
			}
			lits = append(lits, ch.Str)
		}
		return lits, true, true
	}
	return nil, false, false
}

// isFloat says the compiler knows the operand to be a float.
func isFloat(t inferredType) bool {
	return t.known && t.t == FieldTypeFloat
}

// unroundedDivision reports a division in n with an operand typed float
// that no floor()/round() wraps.
func unroundedDivision(n *expr.Snapshot, env exprEnv) bool {
	if n == nil {
		return false
	}
	if n.Kind == expr.SnapFuncCall && (n.Func == "floor" || n.Func == "round") {
		return false
	}
	if n.Kind == expr.SnapBinary && n.Op == "/" && len(n.Children) == 2 {
		l, r := env.inferType(n.Children[0]), env.inferType(n.Children[1])
		if isFloat(l) || isFloat(r) {
			return true
		}
	}
	for _, ch := range n.Children {
		if unroundedDivision(ch, env) {
			return true
		}
	}
	return false
}

// walkExprTypes is the single recursive pass over an expression snapshot. At
// each comparison it runs both the enum-literal check (C121) and the
// operand-compatibility check (C107); one walk so adding a third check never
// spawns a third traversal.
func (c *compiler) walkExprTypes(n *expr.Snapshot, env exprEnv, nodeID, eid, loc string) {
	if n == nil {
		return
	}
	if n.Kind == expr.SnapBinary && len(n.Children) == 2 {
		l, r := n.Children[0], n.Children[1]
		if n.Op == "==" || n.Op == "!=" {
			c.checkEnumPair(l, r, env, nodeID, eid, loc)
			c.checkEnumPair(r, l, env, nodeID, eid, loc)
			c.checkOperandCompat(l, r, n.Op, env, nodeID, eid, loc)
			c.checkCollectionCompare(l, r, n.Op, env, nodeID, eid, loc)
		}
		if n.Op == "<" || n.Op == "<=" || n.Op == ">" || n.Op == ">=" {
			c.checkOperandCompat(l, r, n.Op, env, nodeID, eid, loc)
		}
	}
	// C120: subscripting a value whose static type is a scalar (string/bool/
	// int/float) can never index anything — almost always an author mistake.
	// StringArray and json/unknown receivers are fine and bail.
	if n.Kind == expr.SnapIndex && len(n.Children) == 2 {
		if rt := env.inferType(n.Children[0]); rt.known && isScalarType(rt.t) {
			c.warnfAt(DiagIndexOnScalar, nodeID, eid,
				"%s: subscript [...] applied to a %s value, which is not indexable", loc, rt.t)
		}
	}
	for _, ch := range n.Children {
		c.walkExprTypes(ch, env, nodeID, eid, loc)
	}
}

// checkCollectionCompare flags `==` / `!=` where a collection is on either
// side (C306): the evaluator's equals() never walks into a slice or a map,
// so the comparison is CONSTANT — `==` is false and `!=` true even between
// identical contents (`[1] == [1]` is false, `xs != xs` true). The trigger
// is a literal on either side (always a collection, whatever it holds) or an
// operand the compiler knows as one — a string[] field, or the result of a
// total collection helper (`keys`, `values`, `sort`, `unique`, `flatten`,
// `tail`, `slice`, `concat`, `map`, `filter`): `keys(m) == keys(m)` is the
// same constant false. A json field bails to no-opinion like everywhere
// else.
func (c *compiler) checkCollectionCompare(l, r *expr.Snapshot, op string, env exprEnv, nodeID, eid, loc string) {
	isLit := func(s *expr.Snapshot) bool { return s.Kind == expr.SnapList || s.Kind == expr.SnapObject }
	knownColl := func(s *expr.Snapshot) bool { return env.inferType(s).collection }
	if !isLit(l) && !isLit(r) && (!knownColl(l) || !knownColl(r)) {
		return
	}
	// When both operands are statically known AND incompatible, C107 owns
	// the comparison — one finding per site, and the type-mismatch wording
	// already says the comparison will not behave as written.
	if !compatibleOperands(env.inferType(l), env.inferType(r)) {
		return
	}
	c.warnfAt(DiagCollectionCompare, nodeID, eid,
		"%s: operator %q never compares collections by value — `==` is false and `!=` true even for identical contents (`[1] == [1]` is false): compare what you mean — length(...), an element ([0]), keys(...) / values(...), or join(...) against a string",
		loc, op)
}

// checkCollectionLiteralConform warns when a compute field's expression IS a
// collection literal the declared field type cannot hold (C307) — the mirror
// of checkIntDivision for the collection types: the runtime conforms the
// value to the schema and a value that cannot conform fails the node
// (SCHEMA_VALIDATION), and only there. An element is judged by its INFERRED
// type, not its syntax: a literal scalar, a nested collection, and a
// statically-known non-string expression (`[input.n == 1]` is a bool
// element) are all named, because their kind is known from the source
// alone; an element the compiler cannot type — a ref into a json field, an
// untypable call — is not held against the author.
func (c *compiler) checkCollectionLiteralConform(w *Workflow, cn *ComputeNode, ce *ComputeExpr, env exprEnv) {
	schema := w.Schemas[cn.OutputSchema]
	if schema == nil {
		return
	}
	var field *SchemaField
	for _, f := range schema.Fields {
		if f != nil && f.Name == ce.Key {
			field = f
			break
		}
	}
	if field == nil {
		return
	}
	root := expr.ToSnapshot(ce.AST)
	if root == nil {
		return
	}
	if field.Type == FieldTypeStringArray {
		switch root.Kind {
		case expr.SnapList:
			for i, el := range root.Children {
				et := env.inferType(el)
				var what string
				switch {
				case et.collection:
					what = "a nested collection"
				case !et.known || et.t == FieldTypeString:
					continue // untypable, or a string: conforms
				default:
					what = fmt.Sprintf("a statically-known %s", et.t)
				}
				// The remedy matches the element's KIND (L2): "quote it"
				// is a fix for a literal scalar ('42' for 42) — for an
				// expression element quoting yields the constant string
				// 'input.n', and no string() builtin exists, so the fix
				// there is the field's type.
				remedy := "type the field json"
				switch el.Kind {
				case expr.SnapBool, expr.SnapInt, expr.SnapFloat:
					remedy = "quote it ('42'), or type the field json"
				}
				c.warnfAt(DiagCollectionLiteralConformance, cn.ID, "",
					"compute %q field %q is a string[] but its list literal's element %d is %s — it fails SCHEMA_VALIDATION at run time (`field[%d]: expected string, got ...`): %s",
					cn.ID, ce.Key, i, what, i, remedy)
				return // one warning per field names the shape
			}
		case expr.SnapObject:
			c.warnfAt(DiagCollectionLiteralConformance, cn.ID, "",
				"compute %q field %q is a string[] but its expression is an object literal — the run fails SCHEMA_VALIDATION (`expected string array, got map[string]interface {}`): write a list `[...]`, or type the field json",
				cn.ID, ce.Key)
		}
		return
	}
	if isScalarType(field.Type) && (root.Kind == expr.SnapList || root.Kind == expr.SnapObject) {
		c.warnfAt(DiagCollectionLiteralConformance, cn.ID, "",
			"compute %q field %q is a %s but its expression is a collection literal — the run fails SCHEMA_VALIDATION (`expected %s, got ...`): type the field json (or string[] for a list of strings)",
			cn.ID, ce.Key, field.Type, conformExpectation(field.Type))
	}
}

// checkEnumPair flags `field == "literal"` / `!=` where the field has an enum
// constraint and the literal is not a member — the comparison can then never
// match, so it is almost always a typo (C121).
func (c *compiler) checkEnumPair(pathSide, litSide *expr.Snapshot, env exprEnv, nodeID, eid, loc string) {
	if pathSide == nil || litSide == nil || pathSide.Kind != expr.SnapPath || litSide.Kind != expr.SnapString {
		return
	}
	f, ok := env.refField(pathSide.Namespace, pathSide.Path)
	if !ok || len(f.EnumValues) == 0 {
		return
	}
	for _, v := range f.EnumValues {
		if v == litSide.Str {
			return // valid enum member
		}
	}
	c.errorfAt(DiagEnumLiteralMismatch, nodeID, eid,
		"%s: literal %q is compared against field %q whose enum is %v — not a member, so the comparison can never match (typo?)",
		loc, litSide.Str, strings.Join(pathSide.Path, "."), f.EnumValues)
}

// checkOperandCompat flags a comparison whose two operands have statically
// known but incompatible types (e.g. string[] == int, count < "x") — C107.
// Numerics compare with numerics; otherwise types must be identical. Unknown
// (json / unresolved ref) bails to compatible.
func (c *compiler) checkOperandCompat(l, r *expr.Snapshot, op string, env exprEnv, nodeID, eid, loc string) {
	lt, rt := env.inferType(l), env.inferType(r)
	if compatibleOperands(lt, rt) {
		return
	}
	c.warnfAt(DiagExprOperandTypeMismatch, nodeID, eid,
		"%s: operator %q compares %s with %s — incompatible operand types; the comparison will not behave as written",
		loc, op, lt.t, rt.t)
}

// isScalarType reports whether a FieldType is a non-indexable scalar.
func isScalarType(t FieldType) bool {
	switch t {
	case FieldTypeString, FieldTypeBool, FieldTypeInt, FieldTypeFloat:
		return true
	}
	return false
}

// compatibleOperands reports whether two inferred types may be compared.
// An unknown operand is compatible with anything (conservative bail).
func compatibleOperands(a, b inferredType) bool {
	if !a.known || !b.known {
		return true
	}
	numeric := func(t FieldType) bool { return t == FieldTypeInt || t == FieldTypeFloat }
	if numeric(a.t) && numeric(b.t) {
		return true
	}
	return a.t == b.t
}

// ---------------------------------------------------------------------------
// Type inference (conservative)
// ---------------------------------------------------------------------------

// exprEnv resolves the schema field a path expression targets. inputSchema is
// the schema name a bare `input.X` resolves against — which differs by
// context: for a `when "expr"` edge it is the SOURCE node's OUTPUT schema (the
// runtime exposes the source output as `input`); for a compute node it is that
// node's declared INPUT schema.
type exprEnv struct {
	w           *Workflow
	inputSchema string
}

// refField resolves a path reference to its SchemaField when statically
// knowable. It normalizes the path the way the evaluator does
// (expr.NormalizePath: a bare identifier is an implicit `input` field), then
// returns (nil, false) for any namespace/path we can't type
// (loop/run/artifacts/secrets, unknown node, missing schema, runtime-injected
// fields, …) so callers never flag uncertainty.
func (env exprEnv) refField(namespace string, path []string) (*SchemaField, bool) {
	namespace, path = expr.NormalizePath(namespace, path)
	switch namespace {
	case "outputs":
		if len(path) < 2 {
			return nil, false
		}
		id, fields := outputNodePath(env.w, path)
		node, ok := env.w.Nodes[id]
		if !ok || len(fields) == 0 {
			return nil, false
		}
		return lookupField(env.w, NodeOutputSchema(node), fields[0])
	case "input":
		if len(path) < 1 {
			return nil, false
		}
		return lookupField(env.w, env.inputSchema, path[0])
	}
	return nil, false
}

func lookupField(w *Workflow, schemaName, field string) (*SchemaField, bool) {
	if schemaName == "" || isRuntimeInjectedField(field) {
		return nil, false
	}
	s, ok := w.Schemas[schemaName]
	if !ok {
		return nil, false
	}
	f := findField(s, field)
	if f == nil {
		return nil, false
	}
	return f, true
}

// inferredType is the conservative static type of an expression sub-tree.
// known==false means "no opinion" (json, unresolved ref, ambiguous builtin)
// and callers MUST treat it as compatible with everything. collection==true
// says the value is definitely a collection (a list or a map) whatever t
// says — equality never compares one by value, which is C306's trigger;
// a collection with a known element type carries both (known, t=string[],
// collection).
type inferredType struct {
	t          FieldType
	known      bool
	collection bool
}

var unknownType = inferredType{}

// knownT types a sub-tree; a string[] is the vocabulary's one typed
// collection, so it carries the collection mark every reader agrees on.
func knownT(t FieldType) inferredType {
	return inferredType{t: t, known: true, collection: t == FieldTypeStringArray}
}

// inferType walks a Snapshot and returns its conservative static type.
func (env exprEnv) inferType(n *expr.Snapshot) inferredType {
	if n == nil {
		return unknownType
	}
	switch n.Kind {
	case expr.SnapBool:
		return knownT(FieldTypeBool)
	case expr.SnapInt:
		return knownT(FieldTypeInt)
	case expr.SnapFloat:
		return knownT(FieldTypeFloat)
	case expr.SnapString:
		return knownT(FieldTypeString)
	case expr.SnapList:
		// A list literal whose every element is statically a string IS the
		// vocabulary's one collection type; the empty literal conforms to it
		// too ([]any{} passes the field check). Anything else is still
		// definitely a collection — equality on it is C306's business —
		// without a nameable element type.
		for _, ch := range n.Children {
			if ct := env.inferType(ch); !ct.known || ct.t != FieldTypeString {
				return collectionT
			}
		}
		return knownT(FieldTypeStringArray)
	case expr.SnapObject:
		// An object literal is definitely a map, and the schema vocabulary
		// types that json — whose doctrine is "no opinion": equality on it
		// is C306's business, conformance C307's, both reading collection.
		return collectionT
	case expr.SnapPath:
		if n.Namespace == "vars" && len(n.Path) == 1 {
			if v, ok := env.w.Vars[n.Path[0]]; ok {
				if ft, ok := v.Type.AsFieldType(); ok {
					return knownT(ft)
				}
			}
			return unknownType
		}
		f, ok := env.refField(n.Namespace, n.Path)
		if !ok || f.Type == FieldTypeJSON {
			return unknownType // json = any → no opinion
		}
		return knownT(f.Type)
	case expr.SnapUnary:
		if n.Op == "!" {
			return knownT(FieldTypeBool)
		}
		// unary minus mirrors the child's numeric type
		if len(n.Children) == 1 {
			child := env.inferType(n.Children[0])
			if child.known && (child.t == FieldTypeInt || child.t == FieldTypeFloat) {
				return child
			}
		}
		return unknownType
	case expr.SnapBinary:
		switch n.Op {
		case "&&", "||", "==", "!=", "<", "<=", ">", ">=":
			return knownT(FieldTypeBool)
		}
		return unknownType // arithmetic: don't over-claim
	case expr.SnapFuncCall:
		switch n.Func {
		case "length":
			return knownT(FieldTypeInt)
		case "contains":
			return knownT(FieldTypeBool)
		case "join":
			return knownT(FieldTypeString)
		case "keys", "values":
			// keys()/values() of a map are a list of its (string) keys /
			// of its values. A KNOWN non-map argument — a typed scalar or
			// list, or a list literal — fails the helper at run time
			// (EXPRESSION_FAILED): no result exists to compare, so no
			// collection is claimed (C306 would over-claim a constant
			// comparison that never happens). An unknown argument (a json
			// ref) keeps the claim: these helpers name collections, and
			// an unknown input usually is one.
			if len(n.Children) >= 1 {
				if at := env.inferType(n.Children[0]); at.known || n.Children[0].Kind == expr.SnapList {
					return unknownType
				}
			}
			if n.Func == "keys" {
				return knownT(FieldTypeStringArray)
			}
			// values() claims the collection fact alone (L4): its caps
			// stay silent deliberately — the loop-cap check reads t/known
			// and a collection of unknowable elements claims nothing,
			// where keys()/sort()/filter()/concat() of a string[] refuse
			// a non-integer cap at compile time. The run refuses the bad
			// cap either way; compile says nothing it cannot prove.
			return collectionT
		case "sort", "unique", "flatten", "tail", "slice":
			// Element-preserving: the result carries the input's element
			// type when it is known.
			if len(n.Children) >= 1 {
				if n.Children[0].Kind == expr.SnapObject {
					return unknownType // a map fails these helpers at run time
				}
				return mirrorCollection(env.inferType(n.Children[0]))
			}
			return collectionT
		case "concat":
			// concat of all-string[] inputs is a string[]; any result it
			// returns is a list either way. A known scalar argument fails
			// the call at run time — no claim.
			if len(n.Children) == 0 {
				return collectionT
			}
			allStringArrays := true
			for _, ch := range n.Children {
				ct := env.inferType(ch)
				if ct.known && !ct.collection {
					return unknownType
				}
				if !ct.known || ct.t != FieldTypeStringArray {
					allStringArrays = false
				}
			}
			if allStringArrays {
				return knownT(FieldTypeStringArray)
			}
			return collectionT
		}
		// if/min/max/sum and arithmetic forms: result type not statically
		// known → bail
		return unknownType
	case expr.SnapLambdaComb:
		// map/filter always produce a list; filter preserves the element
		// type (map's body is opaque). reduce's accumulator is a scalar as
		// often as a collection — no claim. Like values() (L4), map()
		// claims the collection fact alone: its caps stay silent
		// deliberately — the loop-cap check reads t/known, and a
		// collection of unknowable elements claims nothing, where the
		// element-preserving helpers of a string[] refuse a non-integer
		// cap at compile time.
		switch n.Func {
		case "map":
			if len(n.Children) >= 1 {
				ct := env.inferType(n.Children[0])
				if (ct.known && !ct.collection) || n.Children[0].Kind == expr.SnapObject {
					return unknownType // a scalar or a map fails the combinator at run time (L1)
				}
			}
			return collectionT
		case "filter":
			if len(n.Children) >= 1 {
				if n.Children[0].Kind == expr.SnapObject {
					return unknownType // a map fails the combinator at run time (L1)
				}
				return mirrorCollection(env.inferType(n.Children[0]))
			}
			return collectionT
		}
		return unknownType
	}
	return unknownType
}

// collectionT is the inference for a value that is DEFINITELY a collection
// (a list — or a map, equality never walks either) whose element type the
// compiler cannot name. known stays false: every consumer but C306 reads
// t/known, and "a collection of unknowable elements" must not become a
// type claim there — `values(m) == 'x'` is constant-false for the same
// reason `[1] == [1]` is (C306), but it is not a string[]-vs-string
// mismatch (C107 stays out).
var collectionT = inferredType{collection: true}

// mirrorCollection is the element-preserving collection helpers' inference:
// the result carries the input's element type when it is known (sort/unique/
// tail/slice/flatten of a string[] is a string[]), a collection otherwise.
// A KNOWN scalar input is neither: the helper fails it at run time
// (EXPRESSION_FAILED), so no collection — and no comparison a diagnostic
// could call constant — exists (L1).
func mirrorCollection(t inferredType) inferredType {
	if t.known && t.t == FieldTypeStringArray {
		return knownT(FieldTypeStringArray)
	}
	if t.known && !t.collection {
		return unknownType
	}
	return collectionT
}
