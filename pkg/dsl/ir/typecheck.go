package ir

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// ---------------------------------------------------------------------------
// Static cross-node typing (Phase 2): C103 / C107 / C108
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
		}
	}
}

// walkExprTypes is the single recursive pass over an expression snapshot. At
// each comparison it runs both the enum-literal check (C103) and the
// operand-compatibility check (C107); one walk so adding a third check never
// spawns a third traversal.
func (c *compiler) walkExprTypes(n *expr.Snapshot, env exprEnv, nodeID, eid, loc string) {
	if n == nil {
		return
	}
	if n.Kind == expr.SnapBinary && len(n.Children) == 2 {
		l, r := n.Children[0], n.Children[1]
		switch n.Op {
		case "==", "!=":
			c.checkEnumPair(l, r, env, nodeID, eid, loc)
			c.checkEnumPair(r, l, env, nodeID, eid, loc)
			c.checkOperandCompat(l, r, n.Op, env, nodeID, eid, loc)
		case "<", "<=", ">", ">=":
			c.checkOperandCompat(l, r, n.Op, env, nodeID, eid, loc)
		}
	}
	// C113: subscripting a value whose static type is a scalar (string/bool/
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

// checkEnumPair flags `field == "literal"` / `!=` where the field has an enum
// constraint and the literal is not a member — the comparison can then never
// match, so it is almost always a typo (C103).
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
		node, ok := env.w.Nodes[path[0]]
		if !ok {
			return nil, false
		}
		return lookupField(env.w, NodeOutputSchema(node), path[1])
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
// and callers MUST treat it as compatible with everything.
type inferredType struct {
	t     FieldType
	known bool
}

var unknownType = inferredType{}

func knownT(t FieldType) inferredType { return inferredType{t: t, known: true} }

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
		}
		// concat/unique/if: element/result type not statically known → bail
		return unknownType
	}
	return unknownType
}
