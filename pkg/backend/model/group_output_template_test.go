package model

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"testing"
)

func TestGroupOutputTemplatesUseDeclaredIdentity(t *testing.T) {
	td := &TemplateData{
		Nodes: map[string]ir.Node{"r1": &ir.ComputeNode{}, "r1.gate": &ir.ComputeNode{}, "r2.gate": &ir.ComputeNode{}},
		Outputs: map[string]map[string]any{
			"r1":      {"gate": map[string]any{"value": "wrong"}, "nested": map[string]any{"value": "nested"}},
			"r1.gate": {"value": "A; B"}, "r2.gate": {"value": "C"},
		},
	}
	e := &ClawExecutor{}
	const raw = "{{outputs.r1.gate.value}}"
	refs := mustRefs(raw)
	if got := e.resolveTemplate(raw+"/{{outputs.r2.gate.value}}/{{outputs.r1.nested.value}}", nil, td); got != "A; B/C/nested" {
		t.Fatal(got)
	}
	if got := resolveCommandTemplate("echo "+raw, refs, nil, nil, td, "", nil); got != "echo 'A; B'" {
		t.Fatal(got)
	}
	if got := resolveScriptTemplate("let x = "+raw, refs, nil, nil, td, ""); got != `let x = "A; B"` {
		t.Fatal(got)
	}
	delete(td.Outputs, "r1.gate")
	if got := e.resolveTemplate(raw, nil, td); got != raw {
		t.Fatalf("missing node fell through to shorter node: %q", got)
	}
	if got := resolveCommandTemplate("echo "+raw, refs, nil, nil, td, "", nil); got != "echo "+raw {
		t.Fatal(got)
	}
	if got := resolveScriptTemplate("let x = "+raw, refs, nil, nil, td, ""); got != "let x = null" {
		t.Fatal(got)
	}
	// Hosts predating declaration metadata can still use dotted snapshot keys.
	td.Nodes = nil
	td.Outputs["r1.gate"] = map[string]any{"value": "legacy"}
	if got := e.resolveTemplate(raw, nil, td); got != "legacy" {
		t.Fatal(got)
	}
}
