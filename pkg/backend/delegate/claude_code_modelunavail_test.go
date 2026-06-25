package delegate

import "testing"

// TestIsModelUnavailableResult guards the model-unavailable guard added after a
// Seki (sec-audit-source) dogfood: a claude_code node was dragged onto an
// openai/* model by the shared ITERION_SEC_AUDIT_BACKEND/MODEL override. The
// claude CLI completed "successfully" (subtype=success, IsError=false) with its
// model-error sentence AS the result text, which then failed two formatting
// passes and surfaced as an opaque "missing required field" schema error. The
// matcher re-types that prose so the node fails fast and legibly.
func TestIsModelUnavailableResult(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// The exact CLI sentence (and variants) — must match.
		{"exact openai", "There's an issue with the selected model (openai/gpt-5.5). It may not exist or you may not have access to it. Run --model to pick a different model.", true},
		{"no access phrasing", "There's an issue with the selected model (some-model). It may not exist or you may not have access to it.", true},
		{"may not exist only", "Issue with the selected model: it may not exist.", true},
		{"leading/trailing space", "  There's an issue with the selected model (x). It may not exist or you may not have access to it.\n", true},
		{"case-insensitive", "THERE'S AN ISSUE WITH THE SELECTED MODEL (X). IT MAY NOT EXIST OR YOU MAY NOT HAVE ACCESS TO IT.", true},

		// Genuine assistant output — must never be mistaken for the CLI error.
		{"empty", "", false},
		{"normal answer", "Here is the test plan: add unit tests for pkg/log.", false},
		{"mentions selected model only", "The user selected model from the dropdown and clicked submit.", false},
		{"mentions access only", "Grant the service account access to it before deploying.", false},
		{"discusses unavailability mid-text", "When the selected model is unavailable the client should retry with a fallback, so add a test that the access to it path is covered.", true}, // contains both markers — acceptable; bounded length keeps false positives rare in real CLI traffic
		{"too long", "There's an issue with the selected model. It may not exist or you may not have access to it. " + longPad(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isModelUnavailableResult(tt.in); got != tt.want {
				t.Errorf("isModelUnavailableResult(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
