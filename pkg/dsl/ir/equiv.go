package ir

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
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
// (the studio's save path), the conversions of an author document and E056
// on the .bot beside one all ask the same question — and every one of them
// shows the answer to someone, so the difference named is the WORKFLOW's
// wherever both sides compiled one. A diagnostic difference between two
// files that are not the same workflow is that difference's consequence,
// and the sorted code list names no construct to go and read. The codes
// remain the only answer available where one side compiled no workflow, and
// the only one that can see a difference two equal workflows still hold.
func SameProgram(a, b *CompileResult) string {
	if why := FirstWorkflowDifference(a.Workflow, b.Workflow); why != "" {
		return why
	}
	if ca, cb := diagnosticCodes(a), diagnosticCodes(b); !reflect.DeepEqual(ca, cb) {
		return fmt.Sprintf("diagnostics differ: %v vs %v", ca, cb)
	}
	if (a.Workflow == nil) != (b.Workflow == nil) {
		return "one side compiled to a workflow and the other did not"
	}
	return ""
}

func diagnosticCodes(cr *CompileResult) []string {
	codes := make([]string, 0, len(cr.Diagnostics))
	for _, d := range cr.Diagnostics {
		codes = append(codes, string(d.Code))
	}
	sort.Strings(codes)
	return codes
}

// FirstWorkflowDifference names what differs between two compiled workflows,
// coarsely: enough for a test failure, a refused save or a warning about the
// file beside a document to point at the construct, without reproducing the
// whole IR. A field is named by its IR name, which is the camel case of the
// DSL key it is read from for all but a handful: the permission lists are
// `PermissionAllow`/`Ask`/`Deny` for `allow:`/`ask:`/`deny:`, and
// `ActiveMCPServers` and `ResolvedMCPServers` are filled after project
// config resolution, from no key at all. Nodes are walked by sorted
// id, so the difference named is the same on every run — never the one a
// map's iteration happened on. It is "" where the two are one workflow, and
// where either side compiled none: there is then no workflow to differ by,
// and SameProgram's own answer is the one to say.
func FirstWorkflowDifference(a, b *Workflow) string {
	if a == nil || b == nil || reflect.DeepEqual(a, b) {
		return ""
	}
	for _, id := range slices.Sorted(maps.Keys(a.Nodes)) {
		if _, ok := b.Nodes[id]; !ok {
			return fmt.Sprintf("node %q is missing", id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(b.Nodes)) {
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
		{"contracts", a.Contracts, b.Contracts},
		{"the workflow's contract", a.Contract, b.Contract},
	} {
		if !reflect.DeepEqual(part.x, part.y) {
			return part.name + " differ"
		}
	}
	// Every other field, under its own name and in the order the workflow
	// declares them: a field the IR gains later is named without anyone
	// listing it here.
	va, vb := reflect.ValueOf(*a), reflect.ValueOf(*b)
	for i := 0; i < va.NumField(); i++ {
		if f := va.Type().Field(i); f.IsExported() && !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			return "the workflows differ in " + f.Name
		}
	}
	// Every field of a workflow is exported, so the walk above names one
	// whenever the two are not DeepEqual. What is left to say is the fact
	// itself, never a field no workflow has.
	return "the workflows differ"
}
