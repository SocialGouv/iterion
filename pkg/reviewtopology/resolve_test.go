package reviewtopology

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The review topology names ROLES, not vendors (ADR-169): the resolver
// decides HOW MANY reviewers run, never which family serves them, and it
// reads no credential at all — which is what these witnesses pin down.

func wfWithVars(names ...string) *ir.Workflow {
	vars := make(map[string]*ir.Var, len(names))
	for _, n := range names {
		vars[n] = &ir.Var{}
	}
	return &ir.Workflow{Vars: vars}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		override string
		want     string
	}{
		// Frugality is the default: auto is what every unconfigured caller
		// gets, and a second reviewer pass is a deliberate spend.
		{"", ModeSingle},
		{"auto", ModeSingle},
		{"single", ModeSingle},
		{"paired", ModePaired},
		{" PAIRED ", ModePaired},
		// A value outside the vocabulary resolves single rather than
		// refusing the launch — the bot's declared enum is the gate for
		// typos, and it sees the CONCRETE mode this function writes back.
		{"wat", ModeSingle},
		// Retired spellings still in the field (integration launch vars)
		// map onto the role vocabulary instead of tripping that gate.
		{"mono", ModeSingle},
		{"dual", ModePaired},
		{"DUAL", ModePaired},
	}
	for _, tt := range tests {
		t.Run(tt.override, func(t *testing.T) {
			if got := Resolve(tt.override); got != tt.want {
				t.Fatalf("Resolve(%q) = %q, want %q", tt.override, got, tt.want)
			}
		})
	}
}

func TestInjectReviewModeIfDeclared_optIn(t *testing.T) {
	inputs := map[string]any{}
	if _, injected := InjectReviewModeIfDeclared(wfWithVars("workspace_dir"), inputs, ""); injected {
		t.Fatal("expected no injection for a bot without review_mode var")
	}
	if len(inputs) != 0 {
		t.Fatalf("inputs mutated for non-opted-in bot: %v", inputs)
	}
}

// Every declaring bot gets a CONCRETE mode written back, whatever the
// surface handed in: the enum gate reads the injected value, so a retired or
// misspelt override must never reach it.
func TestInjectReviewModeIfDeclared_writesTheConcreteMode(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		input string
		want  string
	}{
		{"auto detected", "", ModeAuto, ModeSingle},
		{"flag alone", ModePaired, "", ModePaired},
		{"var alone", "", ModePaired, ModePaired},
		{"flag wins over var", ModeSingle, ModePaired, ModeSingle},
		{"retired var", "", "dual", ModePaired},
		{"retired flag", "mono", "", ModeSingle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputs := map[string]any{}
			if tt.input != "" {
				inputs[VarReviewMode] = tt.input
			}
			mode, injected := InjectReviewModeIfDeclared(wfWithVars(VarReviewMode), inputs, tt.flag)
			if !injected || mode != tt.want {
				t.Fatalf("got (%q, injected=%v), want (%q, true)", mode, injected, tt.want)
			}
			if got := inputs[VarReviewMode]; got != tt.want {
				t.Fatalf("inputs[review_mode] = %v, want the concrete %q", got, tt.want)
			}
			if _, stale := inputs["mono_family"]; stale {
				t.Fatalf("mono_family written back: %v", inputs)
			}
		})
	}
}

func TestInjectAllSummaryNamesTheModeAndTheFamilies(t *testing.T) {
	inputs := map[string]any{}
	inj := InjectAll(wfWithVars(VarReviewMode, VarLLMFamilies), inputs,
		InventoryFromCredentialNames([]string{"anthropic", "openai"}, nil), ModePaired)
	if inj.ReviewMode != ModePaired || !inj.ReviewModeInjected {
		t.Fatalf("review mode not injected paired: %+v", inj)
	}
	if got, want := inj.Summary(), "review topology: paired · llm families: claude,gpt"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}
