package delegate

import "testing"

// The delegate forwards the model id verbatim to the claude CLI, which
// sends it to whichever Anthropic-compatible endpoint the facade points
// at. The facade trio (Anthropic, z.ai, Moonshot) expects the BARE model
// code — a provider-prefixed code reaches z.ai whole and dies with
// "[1211] Unknown Model". A genuinely foreign prefix stays and fails
// fast.
func TestClaudeCodeModelIDStripsTheFacadePrefixes(t *testing.T) {
	for spec, want := range map[string]string{
		"anthropic/claude-opus-4-8": "claude-opus-4-8",
		"zai/glm-5.3":               "glm-5.3",
		"zai/glm-5.3-air":           "glm-5.3-air",
		"moonshot/kimi-k2":          "kimi-k2",
		"claude-opus-4-8":           "claude-opus-4-8",
		"":                          "",
		// A foreign provider prefix is a genuinely non-Anthropic model:
		// it stays, so the run fails fast instead of masking the route.
		"openai/gpt-5.5": "openai/gpt-5.5",
	} {
		if got := claudeCodeModelID(spec); got != want {
			t.Errorf("claudeCodeModelID(%q) = %q, want %q", spec, got, want)
		}
	}
}
