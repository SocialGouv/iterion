package apikit

import "testing"

func TestCurrentModelCapabilitiesOffline(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna"} {
		e := (&ModelRegistry{}).LookupModel(model)
		if e == nil {
			t.Fatalf("missing %s", model)
		}
		if e.MaxOutput != 128_000 || e.ContextWindow < 1_000_000 {
			t.Errorf("limits for %s: %+v", model, e)
		}
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			if err := ValidateEffortForModel(effort, model); err != nil {
				t.Error(err)
			}
		}
		if err := ValidateEffortForModel("minimal", model); err == nil {
			t.Errorf("%s must reject minimal", model)
		}
	}
	if ValidateEffortForModel("none", "gpt-6-astra") == nil {
		t.Error("Astra cannot disable reasoning")
	}
	for _, model := range []string{"gpt-6-sol", "gpt-6-luna"} {
		if err := ValidateEffortForModel("none", model); err != nil {
			t.Error(err)
		}
	}
	if !AnthropicProfile("claude-opus-5-5").RequiresAdaptiveThinking {
		t.Error("missing mandatory thinking capability")
	}
	if AnthropicProfile("claude-opus-5").RequiresAdaptiveThinking {
		t.Error("legacy explicit model changed")
	}
}
