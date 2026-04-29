package ir

import (
	"testing"
)

func TestParseRefs_Single(t *testing.T) {
	refs, err := ParseRefs("{{vars.review_rules}}")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	r := refs[0]
	if r.Kind != RefVars {
		t.Errorf("expected RefVars, got %v", r.Kind)
	}
	if len(r.Path) != 1 || r.Path[0] != "review_rules" {
		t.Errorf("expected path [review_rules], got %v", r.Path)
	}
}

func TestParseRefs_OutputsWithField(t *testing.T) {
	refs, err := ParseRefs("{{outputs.technical_decision_gate.needs_human_input}}")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	r := refs[0]
	if r.Kind != RefOutputs {
		t.Errorf("expected RefOutputs, got %v", r.Kind)
	}
	if len(r.Path) != 2 || r.Path[0] != "technical_decision_gate" || r.Path[1] != "needs_human_input" {
		t.Errorf("expected path [technical_decision_gate needs_human_input], got %v", r.Path)
	}
}

func TestParseRefs_Input(t *testing.T) {
	refs, err := ParseRefs("{{input.pr_context}}")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].Kind != RefInput {
		t.Errorf("expected RefInput, got %v", refs[0].Kind)
	}
}

func TestParseRefs_Artifacts(t *testing.T) {
	refs, err := ParseRefs("{{artifacts.run_summary}}")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].Kind != RefArtifacts {
		t.Errorf("expected RefArtifacts, got %v", refs[0].Kind)
	}
}

func TestParseRefs_NoRefs(t *testing.T) {
	refs, err := ParseRefs("plain text with no templates")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected 0 refs, got %d", len(refs))
	}
}

func TestParseRefs_MultipleInText(t *testing.T) {
	refs, err := ParseRefs("Review: {{input.review}} Context: {{outputs.context_builder}}")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].Kind != RefInput {
		t.Errorf("ref[0]: expected RefInput, got %v", refs[0].Kind)
	}
	if refs[1].Kind != RefOutputs {
		t.Errorf("ref[1]: expected RefOutputs, got %v", refs[1].Kind)
	}
}

func TestParseRefs_UnknownNamespace(t *testing.T) {
	_, err := ParseRefs("{{bogus.field}}")
	if err == nil {
		t.Fatal("expected error for unknown namespace")
	}
}

func TestParseRefs_MissingPath(t *testing.T) {
	_, err := ParseRefs("{{vars}}")
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestParseRefs_Unterminated(t *testing.T) {
	_, err := ParseRefs("{{vars.x")
	if err == nil {
		t.Fatal("expected error for unterminated expression")
	}
}
