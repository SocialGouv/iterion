package ir

import (
	"bytes"
	"encoding/json"
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
	left, right := withoutPortSerializationDetails(a.Workflow), withoutPortSerializationDetails(b.Workflow)
	if reflect.DeepEqual(left, right) {
		return ""
	}
	return firstWorkflowDifference(left, right)
}

// Source positions explain diagnostics; they do not change behavior. JSON
// defaults also retain their syntax's whitespace and key order, neither of
// which changes their value. Normalize copies so the editor's transport and
// reparsed .bot source compare as the same program without altering the IR.
func withoutPortSerializationDetails(w *Workflow) *Workflow {
	if w == nil || w.Ports == nil {
		return w
	}
	copy := *w
	canonicalJSON := func(raw json.RawMessage) json.RawMessage {
		if raw == nil {
			return nil
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return raw
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return raw
		}
		return canonical
	}
	cleanPorts := func(ports []PublicPort) []PublicPort {
		clean := append([]PublicPort(nil), ports...)
		for i := range clean {
			clean[i].Source = PortSource{}
			clean[i].Default = canonicalJSON(clean[i].Default)
		}
		return clean
	}
	stripContract := func(contract *PublicContract) *PublicContract {
		if contract == nil {
			return nil
		}
		clean := *contract
		clean.Source = PortSource{}
		clean.Inputs = cleanPorts(contract.Inputs)
		clean.Outputs = cleanPorts(contract.Outputs)
		clean.Criteria = append([]PublicCriterion(nil), contract.Criteria...)
		for i := range clean.Criteria {
			clean.Criteria[i].Source = PortSource{}
			clean.Criteria[i].Params = canonicalJSON(clean.Criteria[i].Params)
		}
		clean.Effects = append([]PublicEffect(nil), contract.Effects...)
		for i := range clean.Effects {
			clean.Effects[i].Source = PortSource{}
		}
		return &clean
	}
	copy.PublicContract = stripContract(w.PublicContract)
	graph := *w.Ports
	graph.Nodes = make(map[string]*PortInstance, len(w.Ports.Nodes))
	for name, instance := range w.Ports.Nodes {
		if instance == nil {
			graph.Nodes[name] = nil
			continue
		}
		clean := *instance
		clean.Source = PortSource{}
		clean.Contract = stripContract(instance.Contract)
		graph.Nodes[name] = &clean
	}
	graph.Bindings = append([]PortBinding(nil), w.Ports.Bindings...)
	for i := range graph.Bindings {
		graph.Bindings[i].Source = PortSource{}
	}
	copy.Ports = &graph
	copy.Schemas = make(map[string]*Schema, len(w.Schemas))
	for name, schema := range w.Schemas {
		if schema == nil || !schema.NativePorts {
			copy.Schemas[name] = schema
			continue
		}
		clean := *schema
		clean.PublicPorts = cleanPorts(schema.PublicPorts)
		copy.Schemas[name] = &clean
	}
	return &copy
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
			if part.name == "schemas" {
				for name, schema := range a.Schemas {
					if !reflect.DeepEqual(schema, b.Schemas[name]) {
						return fmt.Sprintf("schema %q differs", name)
					}
				}
			}
			return part.name + " differ"
		}
	}
	return "the workflows differ outside nodes, edges, prompts, schemas, vars, loops, budget and resources"
}
