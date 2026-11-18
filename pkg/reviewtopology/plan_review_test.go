package reviewtopology

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
)

// fams builds an inventory whose named families are backed.
func fams(names ...string) FamilyInventory {
	var f routeFacts
	for _, n := range names {
		switch n {
		case FamilyClaude:
			f.claudeForfait = true
		case FamilyGPT:
			f.openaiCred = true
		}
	}
	return FamilyInventory{facts: f}
}

func TestResolvePlanReview(t *testing.T) {
	cases := []struct {
		name     string
		fams     FamilyInventory
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
		if got := ResolvePlanReview(tc.fams.Families(), tc.override); got != tc.want {
			t.Errorf("%s: ResolvePlanReview = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestFamiliesFromReportCountsOnlyParticipatingCredentials pins what a
// credential has to be for `plan_review: auto` to see a second family. Only
// two families participate (claude, gpt), and a backend outside them backs
// NONE — so a host credentialed claude + kimi has one family and the pair
// review resolves off. The bundles' pages state this, and without the mapping
// measured here the sentence rests on nothing.
//
// The pair this gate counts is not, on every bundle, the credential the peer
// node spends: feature-dev's reviewer is pinned `kimi` +
// `kimi-code/kimi-for-coding` with no model or backend fallback, so `auto`
// resolves OFF on exactly the host that could run it. That is the shipped
// behaviour, not an intent this test endorses — `plan_review: on` is the
// override, measured below, and gating on the families a bundle's peer node
// declares is a change to the gate, not to this mapping.
func TestFamiliesFromReportCountsOnlyParticipatingCredentials(t *testing.T) {
	rep := detect.Report{
		Backends: []detect.BackendStatus{
			{Name: detect.BackendClaudeCode, Available: true},
			{Name: "kimi", Available: true},
			{Name: detect.BackendCodex, Available: false},
		},
		Providers: []detect.ProviderStatus{
			{Name: "moonshot", Available: true},
			{Name: "openai", Available: false},
		},
	}
	got := FamiliesFromReport(rep)
	if len(got) != 1 || !got[FamilyClaude] {
		t.Fatalf("FamiliesFromReport(claude_code + kimi + moonshot) = %v, want only %q: a backend and a provider outside the two participating families back none", got, FamilyClaude)
	}
	if mode := ResolvePlanReview(got, ""); mode != PlanReviewOff {
		t.Errorf("plan_review auto on a claude + kimi host = %q, want %q — the pair review needs two participating families, whatever CLI the bundle pins its peer to", mode, PlanReviewOff)
	}
	if mode := ResolvePlanReview(got, "on"); mode != PlanReviewOn {
		t.Errorf("plan_review on a claude + kimi host, overridden %q = %q, want %q — the explicit switch is how that host gets the peer review its own credentials could run", "on", mode, PlanReviewOn)
	}
}

// The local gateway row (#2646) participates in NO review family: a gateway
// may serve anything, so its presence must not read as a second family and
// turn a mono resolution dual.
func TestFamiliesFromReportIgnoresTheGatewayRow(t *testing.T) {
	rep := detect.Report{
		Providers: []detect.ProviderStatus{
			{Name: "openai_compatible", Available: true, Source: "OPENAI_COMPATIBLE_BASE_URL"},
		},
	}
	if got := FamiliesFromReport(rep); len(got) != 0 {
		t.Errorf("FamiliesFromReport(gateway only) = %v, want empty — the gateway backs no review family", got)
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

// ITERION_PLAN_REVIEW is the deployment-wide brake for lanes with no
// per-run surface (webhook/cron on a platform-credentialed cloud): it
// wins over auto, loses to an explicit --var — and it must FAIL SAFE:
// a set-but-unrecognised value reads OFF, never silently auto → on.
func TestInjectPlanReviewEnvDefault(t *testing.T) {
	for env, want := range map[string]string{
		"off": PlanReviewOff, "false": PlanReviewOff, "0": PlanReviewOff,
		"garbage": PlanReviewOff,
		"on":      PlanReviewOn, "true": PlanReviewOn, "1": PlanReviewOn,
	} {
		t.Setenv(PlanReviewEnv, env)
		inputs := map[string]any{}
		mode, _ := InjectPlanReviewIfDeclared(wfWithVars(VarPlanReview), inputs, fams(FamilyClaude, FamilyGPT))
		if mode != want {
			t.Errorf("env %q: got %q, want %q", env, mode, want)
		}
	}
	// An explicit --var beats the env; a --var spelled "AUTO" is still
	// auto (case-insensitive) and therefore still consults the env.
	t.Setenv(PlanReviewEnv, "off")
	inputs := map[string]any{VarPlanReview: "on"}
	if mode, _ := InjectPlanReviewIfDeclared(wfWithVars(VarPlanReview), inputs, fams(FamilyClaude)); mode != PlanReviewOn {
		t.Fatalf("an explicit --var on must beat the env: got %q", mode)
	}
	inputs = map[string]any{VarPlanReview: "AUTO"}
	if mode, _ := InjectPlanReviewIfDeclared(wfWithVars(VarPlanReview), inputs, fams(FamilyClaude, FamilyGPT)); mode != PlanReviewOff {
		t.Fatalf("--var AUTO must still consult the env brake: got %q", mode)
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
