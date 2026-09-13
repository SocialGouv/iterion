package ir

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublicViewExposesContractsWithoutTechnicalConfiguration(t *testing.T) {
	contract := &PublicContract{Name: "Report", DisplayName: "Report", Responsibility: "Deliver a report", Version: 1,
		Inputs:  []PublicPort{{Name: "brief", Type: PortType{Name: "string", ShapeHash: "builtin:string"}, Required: true}},
		Outputs: []PublicPort{{Name: "report", Type: PortType{Name: "file", ShapeHash: "builtin:file"}, Required: true}}}
	w := &Workflow{RuntimeSemantics: RuntimeSemanticsPortsV1, PublicContract: contract,
		Ports: &PortGraph{Order: []string{"write"}, Nodes: map[string]*PortInstance{"write": {
			ID: "write", Implementation: "secret_implementation", Contract: contract,
			Policy:      &PortPolicy{Effects: []PortEffectPolicy{{Name: "publish", Verifier: "secret_verifier"}}},
			Inputs:      map[string]PortEndpoint{"brief": {Node: "input", Port: "brief"}},
			OutputTypes: map[string]PortType{"report": {Name: "file", ShapeHash: "builtin:file"}},
		}}, Exports: map[string]PortEndpoint{"report": {Node: "write", Port: "report"}}, Products: []string{"report"}}}
	view := w.PublicView()
	if view == nil || len(view.Nodes) != 1 || view.Nodes[0].Contract.Responsibility != "Deliver a report" ||
		view.Exports["report"].Node != "write" || len(view.Products) != 1 {
		t.Fatalf("public graph lost its contract or product: %+v", view)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret_implementation", "secret_verifier"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("public view exposed technical setting %q: %s", secret, data)
		}
	}
	if (&Workflow{}).PublicView() != nil {
		t.Fatal("legacy workflow acquired a native public view")
	}
}
