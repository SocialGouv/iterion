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

// A dual resolution has no mono family; writing mono_family="" violates
// the [enum: "claude","gpt"] review-pr/evolve declare and kills the run
// at Engine.Run's validateVarEnums gate before its first node (proven
// live: `iterion run bots/review-pr/main.bot --var review_mode=dual`).
// Absent = the bot's own default, which is what dual must leave behind.
func TestInjectDualDoesNotWriteEmptyMonoFamily(t *testing.T) {
	inputs := map[string]any{}
	mode, family, injected := InjectIfDeclaredFamilies(wfWithVars(VarReviewMode), inputs, fams(FamilyClaude, FamilyGPT), "dual")
	if !injected || mode != ModeDual || family != "" {
		t.Fatalf("unexpected resolution: mode=%q family=%q injected=%v", mode, family, injected)
	}
	if v, present := inputs[VarMonoFamily]; present && v == "" {
		t.Fatalf("mono_family written as %q — violates the declared enum; must be left absent", v)
	}
	// Same hole via mono on a credential-less host: preferredFamily is "".
	inputs = map[string]any{}
	InjectIfDeclaredFamilies(wfWithVars(VarReviewMode), inputs, fams(), "mono")
	if v, present := inputs[VarMonoFamily]; present && v == "" {
		t.Fatalf("mono with no family wrote mono_family=%q — must fail on the missing credential, not the enum gate", v)
	}
}

// ITERION_PLAN_REVIEW is the deployment-wide brake for lanes with no
// per-run surface (webhook/cron on a platform-credentialed cloud): it
// wins over auto, loses to an explicit --var.
func TestInjectPlanReviewEnvDefault(t *testing.T) {
	t.Setenv(PlanReviewEnv, "off")
	inputs := map[string]any{}
	mode, _ := InjectPlanReviewIfDeclared(wfWithVars(VarPlanReview), inputs, fams(FamilyClaude, FamilyGPT))
	if mode != PlanReviewOff {
		t.Fatalf("env off must beat auto-on: got %q", mode)
	}
	inputs = map[string]any{VarPlanReview: "on"}
	mode, _ = InjectPlanReviewIfDeclared(wfWithVars(VarPlanReview), inputs, fams(FamilyClaude))
	if mode != PlanReviewOn {
		t.Fatalf("an explicit --var on must beat the env: got %q", mode)
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
