package bots

import (
	"os"
	"strings"
	"testing"
)

func TestCopilotReviewerUsesExternalFallbackOrder(t *testing.T) {
	raw, err := os.ReadFile("copilot/main.bot")
	if err != nil {
		t.Fatalf("read copilot: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "judge judge:")
	if start < 0 {
		t.Fatal("could not find Copi judge node")
	}
	end := strings.Index(src[start:], "\ncompute judge_route:")
	if end < 0 {
		t.Fatal("could not isolate Copi judge node")
	}
	review := src[start : start+end]
	if !strings.Contains(review, "model: \"${ITERION_COPILOT_REVIEWER_MODEL:-claude-opus-5}\"") {
		t.Fatal("Copi reviewer must default to Claude Opus")
	}

	const kimi = "    kimi:\n      backend: \"kimi\"\n      model: \"kimi-code/k3\"\n      on: [usage_window, unavailable]"
	// Grok is the final external reviewer rescue: it must take a Kimi route that
	// exhausted its own transient retry budget, but no unrelated category.
	const grok = "    grok:\n      backend: \"grok\"\n      model: \"grok-4.6\"\n      on: [usage_window, unavailable, transient_exhausted]"
	const unavailable = "    reviewer_unavailable:\n      action: skip\n      on: [usage_window, unavailable, transient_exhausted]"
	first := strings.Index(review, kimi)
	second := strings.Index(review, grok)
	third := strings.Index(review, unavailable)
	if first < 0 || second < 0 || third < 0 || first >= second || second >= third {
		t.Fatalf("Copi reviewer fallbacks must be Kimi, Grok, then terminal skip, got:\n%s", review)
	}
	if strings.Contains(review, "backend: \"claw\"") || strings.Contains(review, "openai/gpt-") {
		t.Fatal("Copi reviewer must not fall back to Copi's OpenAI family")
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
