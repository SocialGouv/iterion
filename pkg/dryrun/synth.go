// Package dryrun executes a compiled workflow without the world: a
// NodeExecutor that renders what each node would send and answers with a
// schema-shaped output, an engine whose waits are answered at once, an
// ephemeral store — and a report of what the run met: references that
// resolve to nothing, shell text the interpreter refuses, nodes and edges
// no pass reached, conditions that rested on a shape. It reduces the class
// of failures a bot meets at its first paid run; it does not remove it: a
// shape is not data, and the report says where one was read.
package dryrun

import (
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Value is the schema-shaped value of one field: an enum's first value (its
// last when bias is false), bias for a bool, 1, 1.0, "x", one element for a
// list — one enum value when the list is of an enum — an empty object for
// json, "" for a file. A shape, never data.
//
// A `json`-typed field has no defined shape — it may carry a scalar, an
// object or an array — and the dry run's synthesis has to pick one. It
// picks the object shape (its schema-less identity) unless the caller says
// the field is CONSUMED as an array: a fan_out_each router's `over:`, or an
// array-shaped expression (concat/length/map/…). The caller says so
// through wantArray, and the field becomes an empty array on the false
// pass, a one-element array on the true pass — so both fan-out coverage
// and array-op survival hold on the same shape.
func Value(ft ir.FieldType, enum []string, bias bool, wantArray bool) any {
	if len(enum) > 0 {
		pick := enumValue(enum, bias)
		if ft == ir.FieldTypeStringArray {
			return []any{pick}
		}
		return pick
	}
	switch ft {
	case ir.FieldTypeBool:
		return bias
	case ir.FieldTypeInt:
		return int64(1)
	case ir.FieldTypeFloat:
		return 1.0
	case ir.FieldTypeJSON:
		if wantArray {
			return arrayShape(bias)
		}
		return map[string]any{}
	case ir.FieldTypeStringArray:
		return []any{"x"}
	case ir.FieldTypeFile:
		return ""
	default:
		return "x"
	}
}

// arrayShape is the one-element / empty-array shape a json field takes
// when a downstream consumer reads it as an array: one element on the
// true pass (a fan_out_each fires once, a concat carries two elements —
// enough for both fanning and iteration), empty on the false pass (both
// branches of a `length(x) > 0` gate meet the same walk the bias makes
// for a bool).
func arrayShape(bias bool) any {
	if bias {
		return []any{map[string]any{}}
	}
	return []any{}
}

// Synthesize is the schema-shaped output of a node with no consumer
// knowledge — a `json` field takes the object shape. Kept for the tests
// and for callers that hold no workflow. See SynthesizeAt for the
// consumer-aware variant the executor uses.
func Synthesize(schema *ir.Schema, bias bool) map[string]any {
	return SynthesizeAt(schema, bias, nil)
}

// SynthesizeAt is the schema-shaped output of a node whose downstream
// consumers are known: any field named in arrayFields is shaped as an
// array (arrayShape), the rest keep the shape their type carries.
func SynthesizeAt(schema *ir.Schema, bias bool, arrayFields map[string]bool) map[string]any {
	out := map[string]any{}
	if schema == nil {
		return out
	}
	for _, f := range schema.Fields {
		if f == nil {
			continue
		}
		out[f.Name] = Value(f.Type, f.EnumValues, bias, arrayFields[f.Name])
	}
	return out
}

// enumValue is the enum's first value, its last when bias is false.
func enumValue(enum []string, bias bool) string {
	if bias {
		return enum[0]
	}
	return enum[len(enum)-1]
}

// VarValue is the launch value of a var the launch did not supply: a shape
// of its type — an enum's first value (last when bias is false), bias for a
// bool, 1, 1.0, "x", one element, an empty object. The engine seeds the
// vars that have a default itself; this is for the others.
func VarValue(v *ir.Var, bias bool, wantArray bool) any {
	if v == nil {
		return "x"
	}
	if len(v.EnumValues) > 0 {
		pick := enumValue(v.EnumValues, bias)
		if v.Type == ir.VarStringArray {
			return []any{pick}
		}
		return pick
	}
	switch v.Type {
	case ir.VarBool:
		return bias
	case ir.VarInt:
		return int64(1)
	case ir.VarFloat:
		return 1.0
	case ir.VarJSON:
		if wantArray {
			return arrayShape(bias)
		}
		return map[string]any{}
	case ir.VarStringArray:
		return []any{"x"}
	default:
		return "x"
	}
}

// arrayConsumers walks wf and returns, per node id, the set of output
// fields consumed by a downstream array-shaped position: the collection
// argument of a lambda combinator, the receiver of a non-string subscript,
// an argument of a builtin from expr's array-context registry, or a
// fan_out_each router's `over:` reference. The dry run reads it once
// (at Executor construction) and passes each node's set into SynthesizeAt.
func arrayConsumers(wf *ir.Workflow) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if wf == nil {
		return out
	}
	mark := func(nodeID, field string) {
		if out[nodeID] == nil {
			out[nodeID] = map[string]bool{}
		}
		out[nodeID][field] = true
	}
	// 1. `over:` refs on fan_out_each routers.
	for _, n := range wf.Nodes {
		r, ok := n.(*ir.RouterNode)
		if !ok || r == nil {
			continue
		}
		for _, ref := range r.OverRefs {
			if ref == nil {
				continue
			}
			// Mark only exact `outputs.<node>.<field>` refs (path length 2):
			// a deeper ref like `outputs.a.cfg.items` would name a nested
			// field the shape rule cannot reach anyway — marking `a.cfg`
			// wrongly shapes the parent as an array (PR #1491 review R06ab46,
			// #1318/#1456 stay object for nested refs).
			if ref.Kind == ir.RefOutputs && len(ref.Path) == 2 {
				mark(ref.Path[0], ref.Path[1])
			}
		}
	}
	// 2. `foreach` collection refs — the other iteration surface. A
	// foreach edge whose collection is a `json` output field the shape
	// rule leaves as an object dies on both passes at the source with
	// NO_OUTGOING_EDGE (coerceToArray of an object is nil, no traversal
	// left), a `--strict`-blocking false positive.
	for _, fe := range wf.Foreaches {
		if fe == nil {
			continue
		}
		for _, ref := range fe.CollectionRefs {
			if ref == nil {
				continue
			}
			// Mark only exact `outputs.<node>.<field>` refs (path length 2):
			// a deeper ref like `outputs.a.cfg.items` would name a nested
			// field the shape rule cannot reach anyway — marking `a.cfg`
			// wrongly shapes the parent as an array (PR #1491 review R06ab46,
			// #1318/#1456 stay object for nested refs).
			if ref.Kind == ir.RefOutputs && len(ref.Path) == 2 {
				mark(ref.Path[0], ref.Path[1])
			}
		}
	}
	// 3. array-context refs on every edge expression.
	for _, e := range wf.Edges {
		if e == nil || e.Expression == nil {
			continue
		}
		for _, ref := range e.Expression.ArrayContextRefs() {
			markOutputRef(mark, ref)
		}
	}
	// 4. array-context refs on every compute expression.
	for _, n := range wf.Nodes {
		c, ok := n.(*ir.ComputeNode)
		if !ok || c == nil {
			continue
		}
		for _, e := range c.Exprs {
			if e == nil || e.AST == nil {
				continue
			}
			for _, ref := range e.AST.ArrayContextRefs() {
				markOutputRef(mark, ref)
			}
		}
	}
	return out
}

// markOutputRef marks an outputs.<node>.<field> ref as array-consumed;
// refs of another namespace are ignored (vars are handled separately, the
// others do not target a schema-shaped output). Only exact leaf refs
// (path length 2) are marked — a deeper ref like `outputs.a.cfg.items`
// names a nested field the shape rule cannot reach, so leaving `a.cfg`
// object-shaped is what a sibling map read expects (PR #1491 review
// R06ab46).
func markOutputRef(mark func(string, string), ref expr.Ref) {
	if ref.Namespace != "outputs" || len(ref.Path) != 2 {
		return
	}
	mark(ref.Path[0], ref.Path[1])
}

// arrayConsumedVars returns the set of vars a downstream array-shaped
// position reads (a fan_out_each `over:` on `{{vars.x}}`, or `vars.x`
// inside an array-op expression). The launch inputs use it to shape a
// `json` var without a default as an array when it is consumed as one.
func arrayConsumedVars(wf *ir.Workflow) map[string]bool {
	out := map[string]bool{}
	if wf == nil {
		return out
	}
	mark := func(name string) { out[name] = true }
	for _, n := range wf.Nodes {
		r, ok := n.(*ir.RouterNode)
		if !ok || r == nil {
			continue
		}
		for _, ref := range r.OverRefs {
			if ref == nil {
				continue
			}
			// Mark only exact `vars.<name>` refs (path length 1): a deeper
			// ref like `vars.list.items` names a nested field of a `json`
			// var, and marking the whole var as an array wrongly reshapes
			// the parent (PR #1491 review R06ab46).
			if ref.Kind == ir.RefVars && len(ref.Path) == 1 {
				mark(ref.Path[0])
			}
		}
	}
	// Foreach collection refs are the second iteration surface: a
	// `foreach x in "{{vars.list}}"` on a `json` var without a default is
	// symmetric to a fan_out_each on the same var.
	for _, fe := range wf.Foreaches {
		if fe == nil {
			continue
		}
		for _, ref := range fe.CollectionRefs {
			if ref == nil {
				continue
			}
			// Mark only exact `vars.<name>` refs (path length 1): a deeper
			// ref like `vars.list.items` names a nested field of a `json`
			// var, and marking the whole var as an array wrongly reshapes
			// the parent (PR #1491 review R06ab46).
			if ref.Kind == ir.RefVars && len(ref.Path) == 1 {
				mark(ref.Path[0])
			}
		}
	}
	for _, e := range wf.Edges {
		if e == nil || e.Expression == nil {
			continue
		}
		for _, ref := range e.Expression.ArrayContextRefs() {
			if ref.Namespace == "vars" && len(ref.Path) == 1 {
				mark(ref.Path[0])
			}
		}
	}
	for _, n := range wf.Nodes {
		c, ok := n.(*ir.ComputeNode)
		if !ok || c == nil {
			continue
		}
		for _, e := range c.Exprs {
			if e == nil || e.AST == nil {
				continue
			}
			for _, ref := range e.AST.ArrayContextRefs() {
				if ref.Namespace == "vars" && len(ref.Path) == 1 {
					mark(ref.Path[0])
				}
			}
		}
	}
	return out
}
