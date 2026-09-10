package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// resolveToolCommandUnderTest reproduces how a tool node's `command:` (and
// its `postcondition:`) is resolved in production — shellRecipe /
// runPostcondition — so these assertions bind the shipped path, not a
// helper's private behaviour.
func resolveToolCommandUnderTest(cmd string, refs []*ir.Ref, input, vars map[string]any, td *TemplateData, ctxRunID string) string {
	return resolveCommandTemplate(cmd, refs, input, vars, td, ctxRunID)
}

// resolveToolScriptUnderTest is the same for a tool node's `script:` body
// (scriptRecipe).
func resolveToolScriptUnderTest(script string, refs []*ir.Ref, input, vars map[string]any, td *TemplateData, ctxRunID string) string {
	return resolveScriptTemplate(script, refs, input, vars, td, ctxRunID)
}

func runTemplateData() *TemplateData {
	return &TemplateData{
		RunID: "019ec1d3-f3b2-7a90-ae2f-bba543558786",
		Run: map[string]any{
			"id":              "019ec1d3-f3b2-7a90-ae2f-bba543558786",
			"elapsed_seconds": 42.5,
			"max_cost_usd":    12.0,
		},
	}
}

// TestRunRefUnknownMemberKeepsPlaceholderInShellBody: the `run.*`
// namespace must follow the SAME missing-ref rule as `input.*` and
// `outputs.*` — an unresolvable ref keeps its `{{…}}` placeholder in a
// shell body, so `sh -c` fails on visible braces instead of silently
// running one argument short.
func TestRunRefUnknownMemberKeepsPlaceholderInShellBody(t *testing.T) {
	refs := []*ir.Ref{{Kind: ir.RefRun, Path: []string{"no_such_member"}, Raw: "{{run.no_such_member}}"}}
	got := resolveToolCommandUnderTest("git checkout {{run.no_such_member}}", refs, nil, nil, runTemplateData(), "run-abc")
	if got != "git checkout {{run.no_such_member}}" {
		t.Errorf("unknown run member rendered %q, want the placeholder kept so the command fails visibly", got)
	}
}

// TestRunRefUnknownMemberRendersNullInScriptBody: in a script body the
// missing-ref rule is the language's null literal — a bare `{{…}}` would
// crash the interpreter at parse time before any script logic runs.
func TestRunRefUnknownMemberRendersNullInScriptBody(t *testing.T) {
	refs := []*ir.Ref{{Kind: ir.RefRun, Path: []string{"no_such_member"}, Raw: "{{run.no_such_member}}"}}
	got := resolveToolScriptUnderTest("const v = {{run.no_such_member}};", refs, nil, nil, runTemplateData(), "run-abc")
	if got != "const v = null;" {
		t.Errorf("unknown run member rendered %q, want `const v = null;`", got)
	}
}

// TestRunRefValueIsNotReExpanded: a `run.*` value that happens to contain
// `{{…}}` text matching a LATER ref must land in the command verbatim.
// Substituting run refs in a pass of their own feeds their output back
// into the next pass — the cascade the single-pass walk exists to kill.
func TestRunRefValueIsNotReExpanded(t *testing.T) {
	td := runTemplateData()
	td.Run["note"] = "{{input.payload}}"
	refs := []*ir.Ref{
		{Kind: ir.RefRun, Path: []string{"note"}, Raw: "{{run.note}}"},
		{Kind: ir.RefInput, Path: []string{"payload"}, Raw: "{{input.payload}}"},
	}
	input := map[string]any{"payload": "INJECTED"}

	got := resolveToolCommandUnderTest("echo {{run.note}} {{input.payload}}", refs, input, nil, td, "run-abc")
	if got != "echo '{{input.payload}}' 'INJECTED'" {
		t.Errorf("run value was re-expanded: got %q, want the run value substituted once, verbatim", got)
	}
}

// TestRunRefKnownMemberResolvesInBothBodies pins what already worked:
// `run.*` reaches tool commands and scripts, rendered for the target
// context (shell-escaped / JSON literal). Folding the namespace into the
// shared resolver must not cost that.
func TestRunRefKnownMemberResolvesInBothBodies(t *testing.T) {
	td := runTemplateData()
	refs := []*ir.Ref{
		{Kind: ir.RefRun, Path: []string{"id"}, Raw: "{{run.id}}"},
		{Kind: ir.RefRun, Path: []string{"max_cost_usd"}, Raw: "{{run.max_cost_usd}}"},
	}

	cmd := resolveToolCommandUnderTest("branch iterion/x/{{run.id}} cap {{run.max_cost_usd}}", refs, nil, nil, td, "")
	if want := "branch iterion/x/'" + td.RunID + "' cap '12'"; cmd != want {
		t.Errorf("command = %q, want %q", cmd, want)
	}

	scr := resolveToolScriptUnderTest("const id = {{run.id}}; const cap = {{run.max_cost_usd}};", refs, nil, nil, td, "")
	if want := `const id = "` + td.RunID + `"; const cap = 12;`; scr != want {
		t.Errorf("script = %q, want %q", scr, want)
	}
}

// TestRunRefIDFallsBackToTheContextRunID: a host that wires only
// WithRunID (no template snapshot) still resolves `{{run.id}}` — the one
// member the ctx identity answers for.
func TestRunRefIDFallsBackToTheContextRunID(t *testing.T) {
	refs := []*ir.Ref{{Kind: ir.RefRun, Path: []string{"id"}, Raw: "{{run.id}}"}}
	got := resolveToolCommandUnderTest("b/{{run.id}}", refs, nil, nil, nil, "run-abc")
	if got != "b/'run-abc'" {
		t.Errorf("ctx run id fallback = %q, want b/'run-abc'", got)
	}
}

// TestRunRefIDWithNoIdentityKeepsPlaceholder: with neither a snapshot nor
// a ctx run id there is no value to render, so the shell body keeps its
// placeholder like any other unresolvable ref. Rendering the empty string
// instead would hand `sh -c` a command one argument short.
func TestRunRefIDWithNoIdentityKeepsPlaceholder(t *testing.T) {
	refs := []*ir.Ref{{Kind: ir.RefRun, Path: []string{"id"}, Raw: "{{run.id}}"}}
	got := resolveToolCommandUnderTest("b/{{run.id}}", refs, nil, nil, nil, "")
	if got != "b/{{run.id}}" {
		t.Errorf("unresolvable run.id rendered %q, want the placeholder kept", got)
	}
}
