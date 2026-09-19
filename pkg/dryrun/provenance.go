package dryrun

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// Inconclusive implements runtime.InventedValues: the engine hands it every
// expression it could not evaluate. When the expression reads at least one
// value the dry run invented, the failure decided nothing about the
// program: it is recorded as a KindInconclusive finding that names the
// value and what would decide it, and a stand-in takes the expression's
// place — a shape of the compute field's declared type, invented in turn
// (standIn), or the pass's bias for an edge, which a shaped bool decides
// the same way. When every value the expression reads is the program's own
// — a launch value, a fixture, a literal, a field the schema types — the
// failure is the program's, and stands.
func (x *Executor) Inconclusive(f runtime.ExpressionFailure) (any, bool) {
	var restsOn []string
	seen := map[string]bool{}
	for _, r := range f.Refs {
		if f.Field == "" && f.EdgeTo == "" && r.Namespace == "input" {
			// A collection (a fan_out_each `over:`, a foreach's collection)
			// resolves `input.` against the LAUNCH floor — the template
			// resolver's input namespace is the launch inputs, not the node
			// input an expression reads — so the launch value of the var is
			// the whole story, whatever the edges into a same-named node
			// map.
			r = expr.Ref{Namespace: "vars", Path: r.Path}
		}
		why, ok := x.invented(f.NodeID, r, nil)
		if !ok {
			continue
		}
		name := refName(r.Namespace, r.Path)
		if seen[name] {
			continue
		}
		seen[name] = true
		restsOn = append(restsOn, name+" — "+why)
	}
	if len(restsOn) == 0 {
		return nil, false
	}
	where := "collection"
	switch {
	case f.Field != "":
		where = "field " + f.Field
	case f.EdgeTo != "":
		where = "when -> " + f.EdgeTo
	case f.Collection == "foreach":
		// The node field of a foreach collection finding is the foreach's
		// name — not a node id, and a name a node may legally share — said
		// in the place, carried from the failing surface.
		where = "foreach " + f.NodeID + " (collection)"
	}
	x.add(Finding{Node: f.NodeID, Kind: KindInconclusive, Where: where, Detail: fmt.Sprintf("expression %q could not be decided (%v): it rests on %s", f.Source, f.Err, strings.Join(restsOn, "; "))})
	if f.Field == "" {
		if f.EdgeTo == "" {
			// An iteration's collection: the simulated one-element list
			// stands in, so the body is entered.
			return iteratedShape(), true
		}
		return x.bias, true
	}
	x.mu.Lock()
	if x.undecided == nil {
		x.undecided = map[string]map[string]bool{}
	}
	if x.undecided[f.NodeID] == nil {
		x.undecided[f.NodeID] = map[string]bool{}
	}
	x.undecided[f.NodeID][f.Field] = true
	x.mu.Unlock()
	return x.standIn(f.NodeID, f.Field), true
}

// standIn is the value an undecided compute field takes: a shape of the
// type the compute's schema declares for it — json when the compute
// declares no schema or no such field — a list when a downstream iteration
// reads the field.
func (x *Executor) standIn(node, field string) any {
	ft, enum := ir.FieldTypeJSON, []string(nil)
	if cn, ok := x.wf.Nodes[node].(*ir.ComputeNode); ok && cn != nil {
		if sch := x.wf.Schemas[cn.OutputSchema]; sch != nil {
			for _, sf := range sch.Fields {
				if sf != nil && sf.Name == field {
					ft, enum = sf.Type, sf.EnumValues
					break
				}
			}
		}
	}
	return Value(ft, enum, x.bias, x.iterated[node][field])
}

// invented reports whether a reference an expression of node reads names a
// value the dry run made up, and says which value and what would decide
// it. The dry run invents: a `json` field of an output it shaped (a fixture
// is the program's word, a typed field is a shape of the right type, the
// whole output is the map it is); a `json` var with neither a default nor
// a launch value; the element a fan_out_each or a foreach draws from an
// invented collection; a compute field whose expression read an invented
// value, or could not be decided; a node's input mapped from an invented
// value on an edge into the node, or the entry's floor of an invented var;
// the previous output a loop keeps of an invented field; an artifact
// published from one. visiting guards the walk through computes against a
// cycle.
func (x *Executor) invented(node string, r expr.Ref, visiting map[string]bool) (string, bool) {
	if visiting == nil {
		visiting = map[string]bool{}
	}
	key := node + "|" + refName(r.Namespace, r.Path)
	if visiting[key] {
		return "", false
	}
	visiting[key] = true
	defer delete(visiting, key)
	switch r.Namespace {
	case "vars":
		return x.inventedVar(r.Path)
	case "outputs":
		return x.inventedOutput(r.Path, visiting)
	case "input":
		return x.inventedInput(node, r.Path, visiting)
	case "loop":
		// loop.<name>.previous_output[.field]: the loop's source node's
		// output as of the previous crossing.
		if len(r.Path) >= 2 && r.Path[1] == "previous_output" {
			for _, e := range x.wf.Edges {
				if e != nil && e.LoopName == r.Path[0] {
					return x.inventedOutput(append([]string{e.From}, r.Path[2:]...), visiting)
				}
			}
		}
	case "artifacts":
		if len(r.Path) >= 1 {
			if pub := x.publisher(r.Path[0]); pub != "" {
				return x.inventedOutput(append([]string{pub}, r.Path[1:]...), visiting)
			}
		}
	}
	return "", false
}

// inventedVar: a `json` var with neither a default nor a launch value is a
// shape; any other var is typed, given, or the author's default.
func (x *Executor) inventedVar(path []string) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	name := path[0]
	v := x.wf.Vars[name]
	if v == nil || v.Type != ir.VarJSON || v.HasDefault {
		return "", false
	}
	if _, given := x.given[name]; given {
		return "", false
	}
	return fmt.Sprintf("a json var with neither a default nor a value: give it one with --var %s='<sample>'", name), true
}

// inventedOutput reads an `outputs.…` path against what produced it.
func (x *Executor) inventedOutput(path []string, visiting map[string]bool) (string, bool) {
	node, field, _ := nodeAndField(x.wf, path)
	if node == "" || field == "" {
		return "", false
	}
	if _, pinned := x.fixtures[node]; pinned {
		return "", false
	}
	switch n := x.wf.Nodes[node].(type) {
	case *ir.RouterNode:
		if n == nil || n.RouterMode != ir.RouterFanOutEach || len(n.OverRefs) == 0 || n.OverRefs[0] == nil {
			return "", false
		}
		if field != "item" && field != n.ItemBinding {
			return "", false
		}
		over := n.OverRefs[0]
		why, ok := x.inventedIRRef(node, over, visiting)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("an element of %s, %s", refName(over.Kind.String(), over.Path), why), true
	case *ir.ComputeNode:
		if n == nil {
			return "", false
		}
		x.mu.Lock()
		undecided := x.undecided[node][field]
		x.mu.Unlock()
		if undecided {
			return fmt.Sprintf("its own expression could not be decided (the finding on %s)", node), true
		}
		for _, ce := range n.Exprs {
			if ce == nil || ce.AST == nil || ce.Key != field {
				continue
			}
			for _, ref := range ce.AST.Refs() {
				if why, ok := x.invented(node, ref, visiting); ok {
					return fmt.Sprintf("computed from %s, %s", refName(ref.Namespace, ref.Path), why), true
				}
			}
		}
		return "", false
	default:
		schema := outputSchemaOf(n)
		sch := x.wf.Schemas[schema]
		if sch == nil {
			return "", false
		}
		for _, sf := range sch.Fields {
			if sf == nil || sf.Name != field {
				continue
			}
			if sf.Type != ir.FieldTypeJSON {
				return "", false
			}
			return fmt.Sprintf("a json field the dry run shaped: pin %s with --fixtures, or type the field in schema %s", node, schema), true
		}
		return "", false
	}
}

// inventedInput reads `input.<key>` at node: the entry's floor is the
// launch (a var's value), and every edge into the node may map the key
// from a value of its own.
func (x *Executor) inventedInput(node string, path []string, visiting map[string]bool) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	key := path[0]
	if node == x.wf.Entry {
		if why, ok := x.inventedVar([]string{key}); ok {
			return fmt.Sprintf("the launch value of vars.%s, %s", key, why), true
		}
	}
	for _, e := range x.wf.Edges {
		if e == nil || e.To != node {
			continue
		}
		for _, dm := range e.With {
			if dm == nil || dm.Key != key {
				continue
			}
			for _, ref := range dm.Refs {
				if ref == nil {
					continue
				}
				if why, ok := x.inventedIRRef(e.From, ref, visiting); ok {
					return fmt.Sprintf("mapped from %s on %s -> %s, %s", refName(ref.Kind.String(), ref.Path), e.From, e.To, why), true
				}
			}
		}
	}
	return "", false
}

// inventedIRRef reads a template reference as the runtime resolves it:
// `each.<foreach>.item` is the element the foreach drew from its
// collection, `input.<k>` in a mapping is the launch's value, the rest
// read as the expression namespace of the same name.
func (x *Executor) inventedIRRef(node string, ref *ir.Ref, visiting map[string]bool) (string, bool) {
	switch ref.Kind {
	case ir.RefEach:
		// The foreach is the longest dotted prefix of the path that names
		// one (a group instance scopes its foreach under its prefix).
		for i := len(ref.Path) - 1; i >= 1; i-- {
			fe := x.wf.Foreaches[strings.Join(ref.Path[:i], ".")]
			if fe == nil {
				continue
			}
			if ref.Path[i] != "item" || len(fe.CollectionRefs) == 0 || fe.CollectionRefs[0] == nil {
				return "", false
			}
			coll := fe.CollectionRefs[0]
			why, ok := x.inventedIRRef(node, coll, visiting)
			if !ok {
				return "", false
			}
			return fmt.Sprintf("an element of %s, %s", refName(coll.Kind.String(), coll.Path), why), true
		}
		return "", false
	case ir.RefInput:
		if why, ok := x.inventedVar(ref.Path); ok {
			return fmt.Sprintf("the launch value of vars.%s, %s", ref.Path[0], why), true
		}
		return "", false
	default:
		return x.invented(node, expr.Ref{Namespace: ref.Kind.String(), Path: ref.Path}, visiting)
	}
}

// publisher is the node that publishes the artifact name, or empty.
func (x *Executor) publisher(name string) string {
	for id, n := range x.wf.Nodes {
		var published string
		switch v := n.(type) {
		case *ir.AgentNode:
			published = v.Publish
		case *ir.JudgeNode:
			published = v.Publish
		case *ir.HumanNode:
			published = v.Publish
		case *ir.ToolNode:
			published = v.Publish
		case *ir.ComputeNode:
			published = v.Publish
		}
		if published != "" && published == name {
			return id
		}
	}
	return ""
}

// outputSchemaOf is the output schema a node answers with, empty when the
// kind declares none.
func outputSchemaOf(n ir.Node) string {
	switch v := n.(type) {
	case *ir.AgentNode:
		return v.OutputSchema
	case *ir.JudgeNode:
		return v.OutputSchema
	case *ir.HumanNode:
		return v.OutputSchema
	case *ir.ToolNode:
		return v.OutputSchema
	case *ir.SubbotNode:
		return v.OutputSchema
	}
	return ""
}

// refName is the dotted form of a reference: `outputs.plan.items`.
func refName(namespace string, path []string) string {
	if len(path) == 0 {
		return namespace
	}
	return namespace + "." + strings.Join(path, ".")
}
