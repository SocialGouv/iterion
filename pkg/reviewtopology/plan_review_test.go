package reviewtopology

import (
	"testing"
)

func fams(names ...string) FamilySet {
	f := FamilySet{}
	for _, n := range names {
		f[n] = true
	}
	return f
}

func TestResolvePlanReview(t *testing.T) {
	cases := []struct {
		name     string
		fams     FamilySet
		override string
		want     string
	}{
		{"auto two families", fams(FamilyClaude, FamilyGPT), "", PlanReviewOn},
		{"auto explicit", fams(FamilyClaude, FamilyGPT), "auto", PlanReviewOn},
		{"auto one family", fams(FamilyClaude), "", PlanReviewOff},
		{"auto no family", fams(), "", PlanReviewOff},
		{"forced on without peer", fams(FamilyClaude), "on", PlanReviewOn},
		{"forced off with both", fams(FamilyClaude, FamilyGPT), "off", PlanReviewOff},
		{"unknown override falls to auto", fams(FamilyClaude), "maybe", PlanReviewOff},
	}
	for _, tc := range cases {
		if got := ResolvePlanReview(tc.fams, tc.override); got != tc.want {
			t.Errorf("%s: ResolvePlanReview = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestInjectPlanReviewIfDeclared(t *testing.T) {
	// Not declared → untouched.
	inputs := map[string]any{}
	if _, injected := InjectPlanReviewIfDeclared(wfWithVars(), inputs, fams(FamilyClaude, FamilyGPT)); injected {
		t.Fatal("injected into a workflow that does not declare plan_review")
	}
	if len(inputs) != 0 {
		t.Fatalf("inputs mutated: %+v", inputs)
	}

	// Declared, auto → resolved from families.
	inputs = map[string]any{}
	mode, injected := InjectPlanReviewIfDeclared(wfWithVars(VarPlanReview), inputs, fams(FamilyClaude, FamilyGPT))
	if !injected || mode != PlanReviewOn || inputs[VarPlanReview] != PlanReviewOn {
		t.Fatalf("auto resolution failed: mode=%q inputs=%+v", mode, inputs)
	}

	// An operator --var wins over auto-detection.
	inputs = map[string]any{VarPlanReview: "off"}
	mode, _ = InjectPlanReviewIfDeclared(wfWithVars(VarPlanReview), inputs, fams(FamilyClaude, FamilyGPT))
	if mode != PlanReviewOff {
		t.Fatalf("operator off override lost: %q", mode)
	}
}

func TestInjectLLMFamiliesIfDeclared(t *testing.T) {
	inputs := map[string]any{}
	if _, injected := InjectLLMFamiliesIfDeclared(wfWithVars(), inputs, fams(FamilyGPT)); injected {
		t.Fatal("injected into a workflow that does not declare llm_families")
	}

	inputs = map[string]any{VarLLMFamilies: "stale"}
	list, injected := InjectLLMFamiliesIfDeclared(wfWithVars(VarLLMFamilies), inputs, fams(FamilyGPT, FamilyClaude))
	if !injected || list != "claude,gpt" || inputs[VarLLMFamilies] != "claude,gpt" {
		t.Fatalf("family list wrong: %q %+v", list, inputs)
	}
}

func TestInjectAllSummary(t *testing.T) {
	wf := wfWithVars(VarReviewMode, VarPlanReview, VarLLMFamilies)
	inputs := map[string]any{}
	inj := InjectAll(wf, inputs, fams(FamilyClaude, FamilyGPT), "")
	want := "review topology: mono (family claude) · plan review: on · llm families: claude,gpt"
	if got := inj.Summary(); got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
	// A workflow declaring none of the vars logs nothing.
	if s := (Injection{}).Summary(); s != "" {
		t.Fatalf("empty injection Summary = %q", s)
	}
}
