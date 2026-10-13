package ir

import "sort"

// UnknownInputNames lists the keys of values that name no var of the
// workflow — launch inputs the program could never read, and that every
// launch surface would otherwise drop in silence (#1757): the run would
// execute on defaults while the operator believes it was parameterised.
// Sorted, so an error naming them is stable.
func UnknownInputNames[V any](wf *Workflow, values map[string]V) []string {
	var unknown []string
	for k := range values {
		if _, declared := wf.Vars[k]; !declared {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// NodeInputKeys is every key a node's input can hold, the way the runtime
// builds it (pkg/runtime/engine_resolve.go, buildNodeInputRS): the fields the
// node's input schema declares, plus every key an incoming edge's `with` maps
// onto it — a mapped key is written into the input whether the schema declares
// it or not. On the ENTRY node the run adds its own floor, so every declared
// var counts too: a launch may supply any of them (a launch key that names no
// var is refused unless the operator forces it — UnknownInputNames).
//
// A key outside this set never reaches the node, so a text naming it is
// describing plumbing that does not exist. Inside it, the set is an UPPER
// bound on the entry node: the floor seeds only the vars that declare a
// default, so an undefaulted var the launch omits is in no input, and a read
// of it delivers what an absent key delivers on its surface — the literal
// placeholder on a model's prompt (pkg/runtime/engine_resolve.go,
// TestBuildNodeInput_EntryFloorSeedsOnlyDefaultedVars).
func NodeInputKeys(wf *Workflow, nodeID string) map[string]bool {
	keys := map[string]bool{}
	if wf == nil {
		return keys
	}
	if node, ok := wf.Nodes[nodeID]; ok {
		if s := wf.Schemas[NodeInputSchema(node)]; s != nil {
			for _, f := range s.Fields {
				keys[f.Name] = true
			}
		}
	}
	for _, e := range wf.Edges {
		if e == nil || e.To != nodeID {
			continue
		}
		for _, m := range e.With {
			if m != nil {
				keys[m.Key] = true
			}
		}
	}
	if nodeID == wf.Entry {
		for name := range wf.Vars {
			keys[name] = true
		}
	}
	return keys
}

// DeclaredVarNames lists the workflow's var names, sorted — the set a
// refusal names as the remedy. "none" when the workflow declares none.
func DeclaredVarNames(wf *Workflow) []string {
	names := make([]string, 0, len(wf.Vars))
	for name := range wf.Vars {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}
