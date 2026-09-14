package bundlelint_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The two halves of a host-event gate live in different files: the DSL mode in
// main.bot, the field name in manifest.yaml. Only bundlelint sees both — the
// IR compiler never reads a manifest, and the manifest loader never sees the
// compiled graph.
func TestHostEventMode_CrossChecksBothHalves(t *testing.T) {
	build := func(mode ir.InteractionMode, field string) []bundlelint.Diag {
		node := &ir.HumanNode{}
		node.ID = "chat"
		node.Interaction = mode
		node.OutputSchema = "chat_out"
		w := &ir.Workflow{
			Nodes: map[string]ir.Node{"chat": node},
			Schemas: map[string]*ir.Schema{
				"chat_out": {Name: "chat_out", Fields: []*ir.SchemaField{
					{Name: "message", Type: ir.FieldTypeString},
					{Name: "host_event", Type: ir.FieldTypeJSON},
				}},
			},
		}
		m := &bundle.Manifest{Chat: &bundle.ChatSurface{
			Nodes: map[string]bundle.ChatNode{
				"chat": {Kind: bundle.ChatNodeHuman, TextField: "message", HostEventField: field},
			},
		}}
		return bundlelint.CheckConsistency(bundlelint.Input{Manifest: m, Workflow: w})
	}

	find := func(diags []bundlelint.Diag) *bundlelint.Diag {
		for i := range diags {
			if diags[i].Code == bundlelint.DiagChatHostEventModeMismatch {
				return &diags[i]
			}
		}
		return nil
	}

	// Both halves declared: the shipped, correct shape.
	if d := find(build(ir.InteractionHumanOrHost, "host_event")); d != nil {
		t.Fatalf("a fully declared gate must be clean, got %+v", *d)
	}

	// The severe direction: a gate advertising a standby nothing can deliver.
	d := find(build(ir.InteractionHumanOrHost, ""))
	if d == nil {
		t.Fatal("a mode with no host_event_field must be reported")
	}
	if d.Severity != bundlelint.SeverityError {
		t.Fatalf("an inert declared gate must be an error, got %v", d.Severity)
	}

	// The mild direction: it works, it is just invisible in the graph.
	d = find(build(ir.InteractionHuman, "host_event"))
	if d == nil {
		t.Fatal("a host_event_field with no mode must be reported")
	}
	if d.Severity != bundlelint.SeverityWarning {
		t.Fatalf("a working but undeclared gate must be a warning, got %v", d.Severity)
	}
}
