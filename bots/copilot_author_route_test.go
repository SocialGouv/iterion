package bots

import (
	"strings"
	"testing"
)

func TestCopilotPersistedAuthorsUseClawWithClaudeFallback(t *testing.T) {
	raw, err := botUnitSource("copilot/main.bot")
	if err != nil {
		t.Fatalf("read copilot: %v", err)
	}
	src := string(raw)

	for _, node := range []struct {
		name  string
		end   string
		model string
	}{
		{name: "copi", end: "\ntool validate_draft:", model: "${ITERION_COPILOT_ENTRY_MODEL:-openai/gpt-5.6-terra}"},
		{name: "reflect", end: "\njudge judge:", model: "${ITERION_COPILOT_REFLECTION_MODEL:-openai/gpt-5.6-sol}"},
	} {
		start := strings.Index(src, "agent "+node.name+":")
		if start < 0 {
			t.Fatalf("could not find Copi %s node", node.name)
		}
		relEnd := strings.Index(src[start:], node.end)
		if relEnd < 0 {
			t.Fatalf("could not isolate Copi %s node", node.name)
		}
		body := src[start : start+relEnd]
		if !strings.Contains(body, "backend: \"claw\"") {
			t.Errorf("Copi %s must use Claw", node.name)
		}
		if !strings.Contains(body, "model: \""+node.model+"\"") {
			t.Errorf("Copi %s model does not match its dedicated default %q", node.name, node.model)
		}
		const fallback = "  fallbacks:\n    claude:\n      backend: \"claw\"\n      model: \"${ITERION_COPILOT_FALLBACK_MODEL:-anthropic/claude-opus-5}\"\n      on: [usage_window, unavailable, transient_exhausted]"
		if !strings.Contains(body, fallback) || strings.Count(body, "fallbacks:") != 1 || strings.Count(body, "backend:") != 2 || strings.Count(body, "backend: \"claw\"") != 2 {
			t.Errorf("Copi %s must retain exactly one same-Claw Claude fallback", node.name)
		}
	}
	if strings.Contains(src, "ITERION_COPILOT_OPENAI_MODEL") {
		t.Error("Copi still references the old shared OpenAI model override")
	}
}
