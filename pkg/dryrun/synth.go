// Package dryrun executes a compiled workflow without the world: a
// NodeExecutor that renders what each node would send and answers with a
// schema-shaped output, an engine whose waits are answered at once, an
// ephemeral store — and a report of what the run met: references that
// resolve to nothing, shell text the interpreter refuses, nodes and edges
// no pass reached, conditions that rested on a shape. It reduces the class
// of failures a bot meets at its first paid run; it does not remove it: a
// shape is not data, and the report says where one was read.
package dryrun

import "github.com/SocialGouv/iterion/pkg/dsl/ir"

// Value is the schema-shaped value of one field: an enum's first value (its
// last when bias is false), bias for a bool, 1, 1.0, "x", one element for a
// list, an empty object for json, "" for a file. A shape, never data.
func Value(ft ir.FieldType, enum []string, bias bool) any {
	if len(enum) > 0 {
		if bias {
			return enum[0]
		}
		return enum[len(enum)-1]
	}
	switch ft {
	case ir.FieldTypeBool:
		return bias
	case ir.FieldTypeInt:
		return int64(1)
	case ir.FieldTypeFloat:
		return 1.0
	case ir.FieldTypeJSON:
		return map[string]any{}
	case ir.FieldTypeStringArray:
		return []any{"x"}
	case ir.FieldTypeFile:
		return ""
	default:
		return "x"
	}
}

// Synthesize is the schema-shaped output of a node: every field of its
// output schema set to Value. A node without a schema produces nothing.
func Synthesize(schema *ir.Schema, bias bool) map[string]any {
	out := map[string]any{}
	if schema == nil {
		return out
	}
	for _, f := range schema.Fields {
		if f == nil {
			continue
		}
		out[f.Name] = Value(f.Type, f.EnumValues, bias)
	}
	return out
}

// VarValue is the launch value of a var the launch did not supply: a shape
// of its type — an enum's first value (last when bias is false), bias for a
// bool, 1, 1.0, "x", one element, an empty object. The engine seeds the
// vars that have a default itself; this is for the others.
func VarValue(v *ir.Var, bias bool) any {
	if v == nil {
		return "x"
	}
	if len(v.EnumValues) > 0 {
		if bias {
			return v.EnumValues[0]
		}
		return v.EnumValues[len(v.EnumValues)-1]
	}
	switch v.Type {
	case ir.VarBool:
		return bias
	case ir.VarInt:
		return int64(1)
	case ir.VarFloat:
		return 1.0
	case ir.VarJSON:
		return map[string]any{}
	case ir.VarStringArray:
		return []any{"x"}
	default:
		return "x"
	}
}
