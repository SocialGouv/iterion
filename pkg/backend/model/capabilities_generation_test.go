package model

import "testing"

// Reasoning used to be decided by a list of known model ids. A list is silent
// when it is wrong: the model that is not on it is classified as
// non-reasoning, extended thinking is never requested, and the run pays full
// price for a degraded answer without a single warning. These tests pin the
// PROPERTY — generation ≥ 3.5 reasons — so a new Claude works on arrival.
func TestClaudeGenerationParsesBothIdShapes(t *testing.T) {
	cases := []struct {
		id           string
		major, minor int
		ok           bool
	}{
		// family-then-number, the current shape
		{"claude-opus-5", 5, 0, true},
		{"claude-sonnet-5", 5, 0, true},
		{"claude-fable-5", 5, 0, true},
		{"claude-opus-4-8", 4, 8, true},
		{"claude-sonnet-4-6", 4, 6, true},
		{"claude-haiku-4-5-20251001", 4, 5, true},
		// a variant suffix must not disturb the reading
		{"claude-opus-5[1m]", 5, 0, true},
		// number-then-family, the older shape
		{"claude-3-5-sonnet-20241022", 3, 5, true},
		{"claude-3.5-sonnet", 3, 5, true},
		{"claude-3-opus-20240229", 3, 0, true},
		// no recognisable generation: unknown, NOT "generation zero"
		{"claude-instant", 0, 0, false},
		{"some-other-model", 0, 0, false},
	}
	for _, c := range cases {
		major, minor, ok := claudeGeneration(c.id)
		if ok != c.ok || major != c.major || minor != c.minor {
			t.Errorf("%s: got (%d, %d, %v), want (%d, %d, %v)",
				c.id, major, minor, ok, c.major, c.minor, c.ok)
		}
	}
}

func TestAnthropicReasoningFollowsGeneration(t *testing.T) {
	reasons := []string{
		"claude-opus-5", "claude-sonnet-5", "claude-fable-5", "claude-opus-5[1m]",
		"claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5-20251001",
		"claude-3-5-sonnet-20241022", "claude-3.5-sonnet",
	}
	for _, id := range reasons {
		if !anthropicCapabilities(id).Reasoning {
			t.Errorf("%s: expected extended thinking to be available", id)
		}
	}

	// Below 3.5, and anything unparseable, stays conservative: requesting
	// thinking from a model that has none is a hard API error, so an
	// unrecognised id must not opt in.
	for _, id := range []string{"claude-3-opus-20240229", "claude-2.1", "claude-instant"} {
		if anthropicCapabilities(id).Reasoning {
			t.Errorf("%s: expected no extended thinking", id)
		}
	}
}

// A future generation must work with no code change. This is the test that is
// supposed to keep passing when Claude 6 ships — if it ever fails, the parser
// regressed to a list.
func TestFutureGenerationsNeedNoCodeChange(t *testing.T) {
	for _, id := range []string{"claude-opus-6", "claude-sonnet-7-2", "claude-opus-10"} {
		c := anthropicCapabilities(id)
		if !c.Reasoning {
			t.Errorf("%s: a later generation must resolve as reasoning-capable", id)
		}
		if !c.ToolCall || !c.Temperature {
			t.Errorf("%s: tool calling and temperature must stay available", id)
		}
	}
}

// GLM is served through the Anthropic-compatible endpoint and must keep being
// handled before the Claude generation reading, which would otherwise find no
// generation in "glm-5.2" and report it as non-reasoning.
func TestGLMStillHandledBeforeClaudeParsing(t *testing.T) {
	c := anthropicCapabilities("glm-5.2")
	if !c.Reasoning {
		t.Error("glm-5.2: reasoning lost — the GLM branch must run first")
	}
	if c.ContextWindow != 1_000_000 {
		t.Errorf("glm-5.2: context window %d, want 1M", c.ContextWindow)
	}
}
