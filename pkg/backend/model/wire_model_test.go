package model

import "testing"

func TestWireModelID(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"anthropic/claude-sonnet-4-6": "claude-sonnet-4-6",
		"openai/gpt-5.5":              "gpt-5.5",
		"anthropic/claude-opus-4-8":   "claude-opus-4-8",
		"claude-opus-4-8":             "claude-opus-4-8", // already bare → unchanged
		"":                            "",
	}
	for in, want := range cases {
		if got := wireModelID(in); got != want {
			t.Errorf("wireModelID(%q) = %q, want %q", in, got, want)
		}
	}
}
