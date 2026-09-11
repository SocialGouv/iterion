package server

import "testing"

// The inline cap was a hardcoded constant with no way out: an operator whose
// bot exceeded it could not use the assistant on that bot and had no way to
// say "I will pay those tokens". The override exists to lift that; it must
// fail SOFT, because a mis-typed cap that disabled the guard (inline
// everything) or zeroed it (inline nothing) would be worse than the constant
// it replaced.
func TestAssistantEditorMaxSource_OverrideFailsSoft(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		want      int
	}{
		{"unset falls back to the SPA default", "", 0},
		{"a real raise is honoured", "400000", 400000},
		{"garbage falls back", "beaucoup", 0},
		{"zero falls back rather than inlining nothing", "0", 0},
		{"negative falls back", "-1", 0},
		{"surrounding space is tolerated", "  250000  ", 250000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ITERION_ASSISTANT_EDITOR_MAX_SOURCE", tc.env)
			if got := assistantEditorMaxSource(); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}
