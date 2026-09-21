// Package dryrun executes a compiled workflow without the world: a
// NodeExecutor that renders what each node would send and answers with a
// schema-shaped output, an engine whose waits are answered at once, an
// ephemeral store — and a report of what the run met: references that
// resolve to nothing, shell text the interpreter refuses, nodes and edges
// no pass reached, conditions that rested on a shape, expressions that
// could not be decided on one. It reduces the class of failures a bot
// meets at its first paid run; it does not remove it: a shape is not data,
// and the report says where one was read.
package dryrun

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Value is the schema-shaped value of one field: an enum's first value (its
// last when bias is false), bias for a bool, 1, 1.0, "x", one element for a
// list — one enum value when the list is of an enum — an empty object for
// json, "" for a file. A shape, never data.
//
// A `json`-typed field has no defined shape — it may carry a scalar, an
// object or an array — and the dry run does not guess one: the field takes
// the object shape, its schema-less identity, and an expression that
// cannot digest it is read as inconclusive, never as the program's death
// (Executor.Inconclusive). The one exception is a field a downstream
// iteration reads — a fan_out_each `over:`, a foreach's collection, the
// collection of a `map`/`filter`/`reduce`: without a list there the
// iteration's body is never entered on either pass, so the caller says so
// through iterated and the field takes iteratedShape.
func Value(ft ir.FieldType, enum []string, bias bool, iterated bool) any {
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
		if iterated {
			return iteratedShape()
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

// iteratedShape is the one-element list a `json` field takes when a
// downstream iteration reads it — the same on both passes: arity is not
// what the bias splits (a bool's value is), and an empty list on one pass
// would leave the iteration's body unvisited there.
func iteratedShape() any {
	return []any{map[string]any{}}
}

// Synthesize is the schema-shaped output of a node with no consumer
// knowledge — a `json` field takes the object shape. Kept for the tests
// and for callers that hold no workflow. See SynthesizeAt for the
// consumer-aware variant the executor uses.
func Synthesize(schema *ir.Schema, bias bool) map[string]any {
	return SynthesizeAt(schema, bias, nil)
}

// SynthesizeAt is the schema-shaped output of a node whose downstream
// iterations are known: a field named in iterated takes iteratedShape, the
// rest keep the shape their type carries.
func SynthesizeAt(schema *ir.Schema, bias bool, iterated map[string]bool) map[string]any {
	out := map[string]any{}
	if schema == nil {
		return out
	}
	for _, f := range schema.Fields {
		if f == nil {
			continue
		}
		out[f.Name] = Value(f.Type, f.EnumValues, bias, iterated[f.Name])
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
// bool, 1, 1.0, "x", one element, an empty object (iteratedShape when a
// downstream iteration reads the var). The engine seeds the vars that have
// a default itself; this is for the others.
func VarValue(v *ir.Var, bias bool, iterated bool) any {
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
		if iterated {
			return iteratedShape()
		}
		return map[string]any{}
	case ir.VarStringArray:
		return []any{"x"}
	default:
		return "x"
	}
}

// iteratedRefs calls fn for every reference a downstream iteration reads —
// a fan_out_each router's `over:`, a foreach's collection, the collection
// of a lambda combinator in an edge or a compute expression — as a
// namespace and a path, whichever form the program holds it in.
func iteratedRefs(wf *ir.Workflow, fn func(namespace string, path []string)) {
	if wf == nil {
		return
	}
	irRefs := func(refs []*ir.Ref) {
		for _, ref := range refs {
			if ref != nil {
				fn(ref.Kind.String(), ref.Path)
			}
		}
	}
	for _, n := range wf.Nodes {
		switch v := n.(type) {
		case *ir.RouterNode:
			if v != nil {
				irRefs(v.OverRefs)
			}
		case *ir.ComputeNode:
			if v == nil {
				continue
			}
			for _, e := range v.Exprs {
				if e == nil || e.AST == nil {
					continue
				}
				for _, r := range e.AST.IteratedRefs() {
					fn(r.Namespace, r.Path)
				}
			}
		}
	}
	for _, fe := range wf.Foreaches {
		if fe != nil {
			irRefs(fe.CollectionRefs)
		}
	}
	for _, e := range wf.Edges {
		if e == nil || e.Expression == nil {
			continue
		}
		for _, r := range e.Expression.IteratedRefs() {
			fn(r.Namespace, r.Path)
		}
	}
}

// iteratedFields walks wf and returns, per node id, the output fields a
// downstream iteration reads. The dry run reads it once (at Executor
// construction) and passes each node's set into SynthesizeAt. Only a ref
// to the field itself counts: a deeper one (`outputs.a.cfg.items`) names a
// nested value the shape cannot reach, and the parent stays an object for
// the sibling that reads it as one.
func iteratedFields(wf *ir.Workflow) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	iteratedRefs(wf, func(namespace string, path []string) {
		if namespace != "outputs" {
			return
		}
		node, field, rest := nodeAndField(wf, path)
		if node == "" || field == "" || len(rest) > 0 {
			return
		}
		if out[node] == nil {
			out[node] = map[string]bool{}
		}
		out[node][field] = true
	})
	return out
}

// iteratedVars returns the vars a downstream iteration reads. The launch
// inputs use it to shape a `json` var without a default as a list when it
// is iterated. Only the var itself counts, as for iteratedFields.
func iteratedVars(wf *ir.Workflow) map[string]bool {
	out := map[string]bool{}
	iteratedRefs(wf, func(namespace string, path []string) {
		if namespace == "vars" && len(path) == 1 {
			out[path[0]] = true
		}
	})
	return out
}

// nodeAndField splits an `outputs.…` path into the node it names — the
// LONGEST dotted prefix that is a declared node, as the runtime resolves
// it (a group instance's id is dotted) — the field read on it, and what
// follows the field. node is empty when no prefix is a node; field is
// empty when the path names the whole output.
func nodeAndField(wf *ir.Workflow, path []string) (node, field string, rest []string) {
	if wf == nil {
		return "", "", nil
	}
	for n := len(path); n >= 1; n-- {
		id := strings.Join(path[:n], ".")
		if wf.Nodes[id] == nil {
			continue
		}
		if n < len(path) {
			return id, path[n], path[n+1:]
		}
		return id, "", nil
	}
	return "", "", nil
}
