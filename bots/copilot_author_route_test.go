package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestCopilotPersistedAuthorsUseClawWithClaudeFallback(t *testing.T) {
	raw, err := botUnitSource("copilot/main.bot")
	if err != nil {
		t.Fatalf("read copilot: %v", err)
	}
	src := string(raw)
	pr := parser.Parse("copilot/main.bot", src)
	if pr.File == nil {
		t.Fatal("copilot/main.bot does not parse")
	}
	agentByName := map[string]*ast.AgentDecl{}
	for _, a := range pr.File.Agents {
		agentByName[a.Name] = a
	}

	for _, node := range []struct {
		name  string
		model string
	}{
		{name: "copi", model: "${ITERION_COPILOT_ENTRY_MODEL:-openai/gpt-6-sol}"},
		{name: "reflect", model: "${ITERION_COPILOT_REFLECTION_MODEL:-openai/gpt-6-sol}"},
	} {
		a := agentByName[node.name]
		if a == nil {
			t.Fatalf("could not find Copi %s node", node.name)
		}
		if a.Backend != "claw" {
			t.Errorf("Copi %s must use Claw", node.name)
		}
		if a.Model != node.model {
			t.Errorf("Copi %s model does not match its dedicated default %q", node.name, node.model)
		}
		if len(a.Fallbacks) != 1 {
			t.Fatalf("Copi %s must retain exactly one fallback, has %d", node.name, len(a.Fallbacks))
		}
		fb := a.Fallbacks[0]
		if fb.Name != "claude" || fb.Backend != "claw" ||
			fb.Model != "${ITERION_COPILOT_FALLBACK_MODEL:-anthropic/claude-opus-5-5}" ||
			strings.Join(fb.On, ", ") != "usage_window, unavailable, transient_exhausted" {
			t.Errorf("Copi %s must retain exactly one same-Claw Claude fallback, has %+v", node.name, fb)
		}
	}
	if strings.Contains(src, "ITERION_COPILOT_OPENAI_MODEL") {
		t.Error("Copi still references the old shared OpenAI model override")
	}
}
