package ir

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// SameProgram reports whether two compilations describe the same program:
// the same diagnostic codes — positions excluded, since one side may have
// come through a transport that carries none — and, when both compiled, the
// same workflow field for field. It returns "" when they are the same and
// otherwise names the first difference.
//
// It is the oracle behind every "this text or document IS the program the
// author wrote" guarantee: the JSON transport (a cloud launch), the unparser
// (the studio's save path) and their tests all ask the same question.
func SameProgram(a, b *CompileResult) string {
	if ca, cb := diagnosticCodes(a), diagnosticCodes(b); !reflect.DeepEqual(ca, cb) {
		return fmt.Sprintf("diagnostics differ: %v vs %v", ca, cb)
	}
	if a.Workflow == nil || b.Workflow == nil {
		if (a.Workflow == nil) != (b.Workflow == nil) {
			return "one side compiled to a workflow and the other did not"
		}
		return ""
	}
	if reflect.DeepEqual(a.Workflow, b.Workflow) {
		return ""
	}
	return firstWorkflowDifference(a.Workflow, b.Workflow)
}

func diagnosticCodes(cr *CompileResult) []string {
	codes := make([]string, 0, len(cr.Diagnostics))
	for _, d := range cr.Diagnostics {
		codes = append(codes, string(d.Code))
	}
	sort.Strings(codes)
	return codes
}

// firstWorkflowDifference names what differs between two compiled workflows
// that are not DeepEqual, coarsely: enough for a test failure or a refused
// save to point at the construct, without reproducing the whole IR.
func firstWorkflowDifference(a, b *Workflow) string {
	for id := range a.Nodes {
		if _, ok := b.Nodes[id]; !ok {
			return fmt.Sprintf("node %q is missing", id)
		}
	}
	for id := range b.Nodes {
		if _, ok := a.Nodes[id]; !ok {
			return fmt.Sprintf("node %q appeared", id)
		}
	}
	var differing []string
	for id, na := range a.Nodes {
		if !reflect.DeepEqual(na, b.Nodes[id]) {
			differing = append(differing, id)
		}
	}
	if len(differing) > 0 {
		sort.Strings(differing)
		return fmt.Sprintf("node(s) differ: %s", strings.Join(differing, ", "))
	}
	if len(a.Edges) != len(b.Edges) {
		return fmt.Sprintf("%d edges vs %d", len(a.Edges), len(b.Edges))
	}
	for i := range a.Edges {
		if !reflect.DeepEqual(a.Edges[i], b.Edges[i]) {
			return fmt.Sprintf("edge %s -> %s differs", a.Edges[i].From, a.Edges[i].To)
		}
	}
	for _, part := range []struct {
		name string
		x, y any
	}{
		{"prompts", a.Prompts, b.Prompts},
		{"schemas", a.Schemas, b.Schemas},
		{"vars", a.Vars, b.Vars},
		{"loops", a.Loops, b.Loops},
		{"budget", a.Budget, b.Budget},
		{"resources", a.Resources, b.Resources},
	} {
		if !reflect.DeepEqual(part.x, part.y) {
			return part.name + " differ"
		}
	}
	return "the workflows differ outside nodes, edges, prompts, schemas, vars, loops, budget and resources"
}
