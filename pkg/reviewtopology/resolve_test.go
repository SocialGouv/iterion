package reviewtopology

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func rep(providers ...detect.ProviderStatus) detect.Report {
	return detect.Report{Providers: providers}
}

func prov(name string, avail bool) detect.ProviderStatus {
	return detect.ProviderStatus{Name: name, Available: avail}
}

// repB builds a report from backend statuses (no provider creds), used to
// cover the forfait case: a claude_code backend OAuth with no anthropic key.
func repB(backends ...detect.BackendStatus) detect.Report {
	return detect.Report{Backends: backends}
}

func backendSt(name string, avail bool) detect.BackendStatus {
	return detect.BackendStatus{Name: name, Available: avail}
}

// TestResolve_BackendFamilies covers families backed by a usable BACKEND
// (not a provider key) — the claude_code forfait being the motivating case.
func TestResolve_BackendFamilies(t *testing.T) {
	tests := []struct {
		name       string
		report     detect.Report
		override   string
		wantMode   string
		wantFamily string
	}{
		// forfait-only host: claude_code backend, no anthropic provider key.
		// Must now resolve a claude family (was gpt/none before the fix).
		{"forfait claude_code → mono claude", repB(backendSt(detect.BackendClaudeCode, true)), "mono", ModeMono, FamilyClaude},
		{"forfait claude_code auto → mono claude", repB(backendSt(detect.BackendClaudeCode, true)), "auto", ModeMono, FamilyClaude},
		// claude_code forfait + openai provider → two families available, but
		// auto still resolves MONO: having two providers configured is not a
		// request to spend two reviewer passes on every run.
		{
			"forfait + openai provider → mono claude",
			detect.Report{
				Providers: []detect.ProviderStatus{prov("openai", true)},
				Backends:  []detect.BackendStatus{backendSt(detect.BackendClaudeCode, true)},
			},
			"auto", ModeMono, FamilyClaude,
		},
		// codex backend → gpt family (symmetry).
		{"codex backend → mono gpt", repB(backendSt(detect.BackendCodex, true)), "mono", ModeMono, FamilyGPT},
		// claw backend backs no family on its own.
		{"claw backend only → no family (dual fallback)", repB(backendSt(detect.BackendClaw, true)), "auto", ModeDual, ""},
		// unavailable backend is ignored.
		{"unavailable claude_code ignored", repB(backendSt(detect.BackendClaudeCode, false)), "auto", ModeDual, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, family := Resolve(tt.report, tt.override)
			if mode != tt.wantMode || family != tt.wantFamily {
				t.Fatalf("Resolve(%q) = (%q, %q), want (%q, %q)",
					tt.override, mode, family, tt.wantMode, tt.wantFamily)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name       string
		report     detect.Report
		override   string
		wantMode   string
		wantFamily string
	}{
		// auto: two families → dual
		{"auto both families → mono (dual is a deliberate spend)", rep(prov("anthropic", true), prov("openai", true)), "auto", ModeMono, FamilyClaude},
		{"empty override both families → mono", rep(prov("anthropic", true), prov("openai", true)), "", ModeMono, FamilyClaude},
		// auto: single family → mono on that family
		{"auto claude only", rep(prov("anthropic", true), prov("openai", false)), "auto", ModeMono, FamilyClaude},
		{"auto gpt only", rep(prov("anthropic", false), prov("openai", true)), "auto", ModeMono, FamilyGPT},
		// zai counts as the claude family (operator decision)
		{"auto zai only → claude", rep(prov("zai", true)), "auto", ModeMono, FamilyClaude},
		{"auto zai + openai → mono", rep(prov("zai", true), prov("openai", true)), "auto", ModeMono, FamilyClaude},
		{"auto anthropic + zai is one family → mono claude", rep(prov("anthropic", true), prov("zai", true)), "auto", ModeMono, FamilyClaude},
		// auto: no participating family → dual (fail normally on credential)
		{"auto no providers", rep(), "auto", ModeDual, ""},
		{"auto only cloud providers ignored", rep(prov("bedrock", true), prov("vertex", true)), "auto", ModeDual, ""},
		// explicit dual always dual
		{"explicit dual with one family", rep(prov("anthropic", true)), "dual", ModeDual, ""},
		{"explicit dual with none", rep(), "dual", ModeDual, ""},
		// explicit mono picks preferred available family
		{"explicit mono both → claude preferred", rep(prov("anthropic", true), prov("openai", true)), "mono", ModeMono, FamilyClaude},
		{"explicit mono gpt only", rep(prov("openai", true)), "mono", ModeMono, FamilyGPT},
		{"explicit mono none → empty family", rep(), "mono", ModeMono, ""},
		// unknown override collapses to auto behaviour
		{"unknown override falls back to auto", rep(prov("openai", true)), "wat", ModeMono, FamilyGPT},
		{"case-insensitive DUAL", rep(prov("anthropic", true), prov("openai", true)), "DUAL", ModeDual, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, family := Resolve(tt.report, tt.override)
			if mode != tt.wantMode || family != tt.wantFamily {
				t.Fatalf("Resolve(%q) = (%q, %q), want (%q, %q)",
					tt.override, mode, family, tt.wantMode, tt.wantFamily)
			}
		})
	}
}

func wfWithVars(names ...string) *ir.Workflow {
	vars := make(map[string]*ir.Var, len(names))
	for _, n := range names {
		vars[n] = &ir.Var{}
	}
	return &ir.Workflow{Vars: vars}
}

func TestInjectIfDeclared_optIn(t *testing.T) {
	// Bot that does NOT declare review_mode is untouched.
	inputs := map[string]any{}
	_, _, injected := InjectIfDeclared(wfWithVars("workspace_dir"), inputs,
		rep(prov("anthropic", true), prov("openai", true)), "")
	if injected {
		t.Fatal("expected no injection for a bot without review_mode var")
	}
	if len(inputs) != 0 {
		t.Fatalf("inputs mutated for non-opted-in bot: %v", inputs)
	}
}

// auto is what every unconfigured caller gets, so it must resolve to the
// FRUGAL topology even when both families are present: dual doubles the
// reviewer spend on every run and has to be asked for.
func TestInjectIfDeclared_autoDetectMono(t *testing.T) {
	wf := wfWithVars(VarReviewMode, VarMonoFamily)
	inputs := map[string]any{VarReviewMode: ModeAuto}
	mode, family, injected := InjectIfDeclared(wf, inputs,
		rep(prov("anthropic", true), prov("openai", true)), "")
	if !injected || mode != ModeMono || family != FamilyClaude {
		t.Fatalf("got (%q,%q,injected=%v), want mono/claude/true", mode, family, injected)
	}
	if inputs[VarReviewMode] != ModeMono || inputs[VarMonoFamily] != FamilyClaude {
		t.Fatalf("inputs not written: %v", inputs)
	}
}

// Dual stays reachable — it is an explicit override, not a detection outcome.
func TestInjectIfDeclared_explicitDual(t *testing.T) {
	wf := wfWithVars(VarReviewMode, VarMonoFamily)
	inputs := map[string]any{VarReviewMode: ModeDual}
	mode, family, injected := InjectIfDeclared(wf, inputs,
		rep(prov("anthropic", true), prov("openai", true)), "")
	if !injected || mode != ModeDual || family != "" {
		t.Fatalf("got (%q,%q,injected=%v), want dual/empty/true", mode, family, injected)
	}
}

func TestInjectIfDeclared_autoDetectMonoSingleFamily(t *testing.T) {
	wf := wfWithVars(VarReviewMode, VarMonoFamily)
	inputs := map[string]any{VarReviewMode: ModeAuto}
	mode, family, _ := InjectIfDeclared(wf, inputs, rep(prov("openai", true)), "")
	if mode != ModeMono || family != FamilyGPT {
		t.Fatalf("got (%q,%q), want mono/gpt", mode, family)
	}
	if inputs[VarMonoFamily] != FamilyGPT {
		t.Fatalf("mono_family not injected: %v", inputs)
	}
}

func TestInjectIfDeclared_flagOverrideWins(t *testing.T) {
	wf := wfWithVars(VarReviewMode, VarMonoFamily)
	// Two families available (auto would pick dual) but the flag forces mono.
	inputs := map[string]any{VarReviewMode: ModeAuto}
	mode, family, _ := InjectIfDeclared(wf, inputs,
		rep(prov("anthropic", true), prov("openai", true)), "mono")
	if mode != ModeMono || family != FamilyClaude {
		t.Fatalf("got (%q,%q), want mono/claude (flag override)", mode, family)
	}
}

func TestInjectIfDeclared_varOverrideUsedWhenNoFlag(t *testing.T) {
	wf := wfWithVars(VarReviewMode, VarMonoFamily)
	// Operator set --var review_mode=dual; no flag → var wins over auto.
	inputs := map[string]any{VarReviewMode: ModeDual}
	mode, _, _ := InjectIfDeclared(wf, inputs, rep(prov("openai", true)), "")
	if mode != ModeDual {
		t.Fatalf("got %q, want dual (var override)", mode)
	}
}
