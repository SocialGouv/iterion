package model

import "testing"

// TestFallbackText covers extracting the free-form text a parse-fallback output
// wraps under "text" — the input the claw structured-output recovery converts
// back into schema-valid JSON.
func TestFallbackText(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"nil", nil, ""},
		{"text wrapper", map[string]any{"text": "hello world"}, "hello world"},
		{"non-text output", map[string]any{"headline": "x"}, ""},
		{"non-string text", map[string]any{"text": 42}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fallbackText(c.in); got != c.want {
				t.Errorf("fallbackText(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
