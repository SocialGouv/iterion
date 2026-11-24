package reviewtopology

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
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

// An engine with the role vocabulary still runs bundles written before it.
// An integration pinned to review-pr ≤ 0.9.25 declares the OLD enum, and the
// launch gate judges the injected value against it: the injection must write
// a spelling that enum accepts, or every launch of an untouched bot is
// refused. The reported mode stays the role either way.
func TestInjectReviewModeSpeaksTheWorkflowsOwnEnum(t *testing.T) {
	withEnum := func(values ...string) *ir.Workflow {
		return &ir.Workflow{Vars: map[string]*ir.Var{VarReviewMode: {Name: VarReviewMode, Type: ir.VarString, EnumValues: values}}}
	}
	for _, tc := range []struct {
		name      string
		wf        *ir.Workflow
		override  string
		wantMode  string
		wantInput string
	}{
		{"an old bundle, default", withEnum("auto", "mono", "dual"), "", ModeSingle, "mono"},
		{"an old bundle, paired asked", withEnum("auto", "mono", "dual"), ModePaired, ModePaired, "dual"},
		{"an old bundle, a retired spelling asked", withEnum("auto", "mono", "dual"), "dual", ModePaired, "dual"},
		{"a new bundle", withEnum("auto", "single", "paired"), "dual", ModePaired, "paired"},
		{"no enum at all", wfWithVars(VarReviewMode), "mono", ModeSingle, "single"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs := map[string]any{}
			mode, injected := InjectReviewModeIfDeclared(tc.wf, inputs, tc.override)
			if !injected || mode != tc.wantMode {
				t.Fatalf("got (%q, injected=%v), want the role %q", mode, injected, tc.wantMode)
			}
			if got := inputs[VarReviewMode]; got != tc.wantInput {
				t.Fatalf("inputs[review_mode] = %v, want %q — the spelling this workflow's enum accepts", got, tc.wantInput)
			}
		})
	}
}

// A bundle written before ADR-169 still routes on `mono_family` and defaults
// it to "claude": on a GPT-only host the old resolver picked gpt for it, and
// without that pick the bundle takes its claude nodes and dies. The legacy
// declaration (with its enum) is resolved from the credentials; the new
// bots' retired declaration (no enum) and an explicit value are left alone.
func TestInjectAllResolvesALegacyBundlesMonoFamily(t *testing.T) {
	legacy := func() *ir.Workflow {
		return &ir.Workflow{Vars: map[string]*ir.Var{
			VarReviewMode: {Name: VarReviewMode, Type: ir.VarString, EnumValues: []string{"auto", "mono", "dual"}},
			"mono_family": {Name: "mono_family", Type: ir.VarString, EnumValues: []string{"claude", "gpt"}},
		}}
	}
	retired := &ir.Workflow{Vars: map[string]*ir.Var{
		VarReviewMode: {Name: VarReviewMode, Type: ir.VarString, EnumValues: []string{"auto", "single", "paired"}},
		"mono_family": {Name: "mono_family", Type: ir.VarString},
	}}
	gptOnly := InventoryFromCredentialNames([]string{"openai"}, nil)
	both := InventoryFromCredentialNames([]string{"anthropic", "openai"}, nil)
	for _, tc := range []struct {
		name   string
		wf     *ir.Workflow
		inv    FamilyInventory
		inputs map[string]any
		want   any
	}{
		{"a legacy bundle on a gpt-only host", legacy(), gptOnly, map[string]any{}, "gpt"},
		{"a legacy bundle where claude is backed", legacy(), both, map[string]any{}, "claude"},
		{"a legacy bundle's explicit pin is kept", legacy(), gptOnly, map[string]any{"mono_family": "claude"}, "claude"},
		{"a legacy bundle on a paired run is left alone", legacy(), gptOnly, map[string]any{VarReviewMode: "dual"}, nil},
		{"a new bundle's retired var is left alone", retired, gptOnly, map[string]any{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			InjectAll(tc.wf, tc.inputs, tc.inv, "")
			if got := tc.inputs["mono_family"]; got != tc.want {
				t.Fatalf("mono_family = %v, want %v (inputs %v)", got, tc.want, tc.inputs)
			}
		})
	}
}

// A third-party reviewer-loop bot that declares `review_mode` WITHOUT an enum
// and compares it to 'mono'/'dual' reads that vocabulary, not the roles:
// writing `single` would read as neither and flip its topology. The shipped
// fixture e2e/testdata/review_topology_mini.bot has exactly that shape.
func TestInjectReviewModeKeepsAnUnconstrainedBotsOwnVocabulary(t *testing.T) {
	pr, err := parser.Parse("review_topology_mini.bot", mustRead(t, "../../e2e/testdata/review_topology_mini.bot"))
	if err != nil || pr == nil || pr.File == nil {
		t.Fatalf("the mini fixture does not parse: %v", err)
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatalf("the mini fixture does not compile: %+v", cr.Diagnostics)
	}
	// The fixture compares only to 'mono': a single review must read as
	// 'mono' there, and a paired one as anything else (its alternation).
	for override, single := range map[string]bool{"": true, "mono": true, "single": true, "dual": false, "paired": false} {
		inputs := map[string]any{}
		InjectReviewModeIfDeclared(cr.Workflow, inputs, override)
		if got := inputs[VarReviewMode]; (got == "mono") != single {
			t.Errorf("override %q: the mini fixture got review_mode=%v — its own guard reads 'mono' as the single topology, so a single review owes 'mono' and a paired one anything else", override, got)
		}
	}
	// A bot with no enum that reads none of the retired literals gets the role.
	inputs := map[string]any{}
	InjectReviewModeIfDeclared(wfWithVars(VarReviewMode), inputs, "dual")
	if got := inputs[VarReviewMode]; got != ModePaired {
		t.Errorf("an unconstrained bot reading no retired literal got %v, want %q", got, ModePaired)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
