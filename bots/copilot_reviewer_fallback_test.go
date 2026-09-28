package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestCopilotReviewerUsesExternalFallbackOrder(t *testing.T) {
	raw, err := botUnitSource("copilot/main.bot")
	if err != nil {
		t.Fatalf("read copilot: %v", err)
	}
	src := string(raw)
	pr := parser.Parse("copilot/main.bot", src)
	if pr.File == nil {
		t.Fatal("copilot/main.bot does not parse")
	}
	var judge *ast.JudgeDecl
	for _, j := range pr.File.Judges {
		if j.Name == "judge" {
			judge = j
		}
	}
	if judge == nil {
		t.Fatal("could not find Copi judge node")
	}
	if judge.Model != "${ITERION_COPILOT_REVIEWER_MODEL:-claude-opus-5-5}" {
		t.Fatal("Copi reviewer must default to Claude Opus")
	}

	// Kimi first; Grok is the final external reviewer rescue: it must take a
	// Kimi route that exhausted its own transient retry budget, but no
	// unrelated category; then the terminal skip.
	want := []struct {
		name                   string
		backend, model, action string
		on                     string
	}{
		{name: "kimi", backend: "kimi", model: "kimi-code/k3", on: "usage_window, unavailable"},
		{name: "grok", backend: "grok", model: "grok-4.6", on: "usage_window, unavailable, transient_exhausted"},
		{name: "reviewer_unavailable", action: "skip", on: "usage_window, unavailable, transient_exhausted"},
	}
	if len(judge.Fallbacks) != len(want) {
		t.Fatalf("Copi reviewer must fall back Kimi, Grok, then terminal skip, has %d routes", len(judge.Fallbacks))
	}
	for i, w := range want {
		fb := judge.Fallbacks[i]
		if fb.Name != w.name || fb.Backend != w.backend || fb.Model != w.model || fb.Action != w.action ||
			strings.Join(fb.On, ", ") != w.on {
			t.Errorf("Copi reviewer fallback %d must be %s, got %+v", i+1, w.name, fb)
		}
	}
	for _, fb := range judge.Fallbacks {
		if fb.Backend == "claw" || strings.HasPrefix(fb.Model, "openai/gpt-") {
			t.Fatal("Copi reviewer must not fall back to Copi's OpenAI family")
		}
	}
}

// A complex turn is privately planned by Sol and challenged by the judge. It
// must not ask the operator to orchestrate a second reviewer or choose a
// technical route that the active evidence can settle.
func TestCopilotReflectionPreventsDuplicateWorkerSearchForActiveEditorEdit(t *testing.T) {
	src := readCopilotContractFile(t, "copilot/main.bot")
	system := copilotContractSection(t, src, "prompt copi_system:", "prompt copi_user:")
	reflect := copilotContractSection(t, src, "prompt reflect_system:", "prompt reflect_user:")
	judge := copilotContractSection(t, src, "prompt judge_system:", "prompt judge_user:")
	requireCopilotContract(t, "Copi reflection handoff", system,
		"Set needs_reflection:true only for a material technical choice, dependent changes, uncertain diagnosis, or observed blocker that current evidence cannot resolve.",
		"Resolve technical objections that repository, run evidence, validation, or the reviewer can settle",
		"do not make the operator choose the implementation",
	)
	requireCopilotContract(t, "Copi private reflection", reflect,
		"You are Copi's private advanced-reflection node.",
		"You do not speak to the operator, produce no Studio actions, and do not write workspace files.",
		"Do not ask the operator or Terra to choose between alternatives that the repository, run state or validation can settle.",
	)
	requireCopilotContract(t, "Copi private judge", judge,
		"You are the private judge for Copi's advanced-reflection plan.",
		"A critique is advice, never a veto",
	)
}
