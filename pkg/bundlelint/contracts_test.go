package bundlelint_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func find(diags []bundlelint.Diag, code bundlelint.Code) *bundlelint.Diag {
	for i := range diags {
		if diags[i].Code == code {
			return &diags[i]
		}
	}
	return nil
}

func contracted(inputs, outputs []*ir.PublicPort) *ir.Workflow {
	w := wf("b", []string{"goal", "depth"}, nil, nil)
	w.Contract = &ir.PublicContract{Name: "pub", Inputs: inputs, Outputs: outputs}
	return w
}

// The manifest's launch form and hand-off are held to the contract: a
// launch.primary name the contract declares no input for, a produces node
// the contract's outputs do not come from — warnings that name both sides.
// launch.hidden — the inputs the form never renders — is not held. A bot
// without a contract, or a manifest that agrees, draws nothing.
func TestContractManifestMismatch(t *testing.T) {
	inputs := []*ir.PublicPort{{Name: "goal", Type: "string", Required: true}}
	outputs := []*ir.PublicPort{{Name: "url", Type: "string", FromNode: "build", FromField: "url"}}
	m := &bundle.Manifest{
		Name:     "b",
		Launch:   &bundle.LaunchHints{Primary: []string{"goal", "depth"}, Hidden: []string{"secret_flag"}},
		Produces: []bundle.ProducedArtifact{{Kind: "review", Node: "review"}, {Kind: "fix", Node: "build"}},
	}
	diags := bundlelint.CheckConsistency(bundlelint.Input{Manifest: m, Workflow: contracted(inputs, outputs)})
	var fields []string
	for _, d := range diags {
		if d.Code == bundlelint.DiagContractManifestMismatch {
			fields = append(fields, d.Field)
			if d.Severity != bundlelint.SeverityWarning {
				t.Errorf("%s is a %s, want a warning", d.Field, d.Severity)
			}
		}
	}
	if got := strings.Join(fields, " "); got != "launch.primary.depth produces[0].node" { // sorted by field
		t.Fatalf("C254 fields: %q\n%v", got, diags)
	}
	if find(bundlelint.CheckConsistency(bundlelint.Input{Manifest: m, Workflow: wf("b", []string{"goal"}, nil, nil)}), bundlelint.DiagContractManifestMismatch) != nil {
		t.Fatal("a bot without a contract was held to one")
	}
	agreeing := &bundle.Manifest{Name: "b", Launch: &bundle.LaunchHints{Primary: []string{"goal"}}, Produces: []bundle.ProducedArtifact{{Kind: "fix", Node: "build"}}}
	if d := find(bundlelint.CheckConsistency(bundlelint.Input{Manifest: agreeing, Workflow: contracted(inputs, outputs)}), bundlelint.DiagContractManifestMismatch); d != nil {
		t.Fatalf("an agreeing manifest drew %v", d)
	}
}

// A subbot node is held to its child's contract on what it passes: a
// required input — one whose var has no default (C300) — the with: block
// does not pass is named, with a hint that forwards a parent var of that
// name only when the parent declares one; an optional input is not asked
// for. The parent's output: schema is not held: a subbot's output is the
// child's terminal node output, which the contract's outputs do not
// define. A child without a contract is held to nothing.
func TestSubbotContractMismatch(t *testing.T) {
	parent := wf("p", []string{"goal"}, nil, nil, &ir.SubbotNode{
		BaseNode:     ir.BaseNode{ID: "child"},
		Source:       "kids/k.bot",
		With:         []*ir.DataMapping{{Key: "goal", Raw: "{{vars.goal}}"}},
		OutputSchema: "result",
	})
	parent.Schemas = map[string]*ir.Schema{"result": {Name: "result", Fields: []*ir.SchemaField{
		{Name: "url", Type: ir.FieldTypeString}, {Name: "notes", Type: ir.FieldTypeString},
	}}}
	child := &ir.PublicContract{Name: "kid",
		Inputs: []*ir.PublicPort{
			{Name: "goal", Type: "string", Required: true},
			{Name: "depth", Type: "int", Required: true},
			{Name: "mode", Type: "string", Required: false, Default: json.RawMessage(`"fast"`)},
			{Name: "tags", Type: "string[]", Required: false},
		},
		Outputs: []*ir.PublicPort{{Name: "url", Type: "string", FromNode: "b", FromField: "url"}},
	}
	diags := bundlelint.CheckConsistency(bundlelint.Input{Workflow: parent, SubbotContracts: map[string]*ir.PublicContract{"child": child}})
	var fields []string
	for _, d := range diags {
		if d.Code == bundlelint.DiagSubbotContractMismatch {
			fields = append(fields, d.Field)
			if !strings.Contains(d.Hint, "with { depth: <value> }") || strings.Contains(d.Hint, "{{vars.depth}}") {
				t.Errorf("the parent declares no var depth, yet the hint forwards one: %q", d.Hint)
			}
		}
	}
	if got := strings.Join(fields, " "); got != "subbot.child.with.depth" {
		t.Fatalf("C255 fields: %q\n%v", got, diags)
	}
	forwarding := wf("p", []string{"goal", "depth"}, nil, nil, parent.Nodes["child"])
	d := find(bundlelint.CheckConsistency(bundlelint.Input{Workflow: forwarding, SubbotContracts: map[string]*ir.PublicContract{"child": child}}), bundlelint.DiagSubbotContractMismatch)
	if d == nil || !strings.Contains(d.Hint, "{{vars.depth}}") {
		t.Fatalf("the parent declares depth, yet the hint does not forward it: %+v", d)
	}
	if diags := bundlelint.CheckConsistency(bundlelint.Input{Workflow: parent}); find(diags, bundlelint.DiagSubbotContractMismatch) != nil {
		t.Fatal("a child without a contract was held to one")
	}
}
