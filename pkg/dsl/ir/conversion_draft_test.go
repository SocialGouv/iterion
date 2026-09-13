package ir

import (
	"reflect"
	"testing"
)

func TestLegacyConversionDraftKeepsUnverifiedWorkIncomplete(t *testing.T) {
	w := &Workflow{
		Name: "legacy",
		Vars: map[string]*Var{
			"outline": {Name: "outline", Type: VarJSON},
			"title":   {Name: "title", Type: VarString, HasDefault: true},
		},
		Nodes: map[string]Node{
			"publish": &ToolNode{BaseNode: BaseNode{ID: "publish"}},
			"done":    &DoneNode{BaseNode: BaseNode{ID: "done"}},
		},
	}
	draft := w.ConversionDraft()
	if draft == nil || draft.Status != "incomplete" || draft.WorkflowName != "legacy" ||
		!reflect.DeepEqual(draft.CandidateInputs, []LegacyInputCandidate{
			{Name: "outline", LegacyType: "json", Required: true},
			{Name: "title", LegacyType: "string", Required: false},
		}) || !reflect.DeepEqual(draft.CandidateNodes, []LegacyNodeCandidate{
		{ID: "done", Kind: "done"}, {ID: "publish", Kind: "tool"},
	}) {
		t.Fatalf("unexpected conversion draft: %+v", draft)
	}
	for _, kind := range []string{"effect_review", "input_type", "node_bindings", "workflow_outputs", "file_guarantees", "control_semantics"} {
		found := false
		for _, gap := range draft.Unresolved {
			found = found || gap.Kind == kind
		}
		if !found {
			t.Fatalf("missing unresolved %s: %+v", kind, draft.Unresolved)
		}
	}
	w.Ports = &PortGraph{}
	if draft := w.ConversionDraft(); draft != nil {
		t.Fatalf("native workflow received legacy extraction aid: %+v", draft)
	}
}
