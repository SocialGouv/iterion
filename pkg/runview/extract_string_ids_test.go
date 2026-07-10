package runview

import (
	"reflect"
	"testing"
)

// TestExtractStringIDs covers the JSON shapes a `json`-typed schema
// field (e.g. assign_to_bots / triage_board `dispatched_ids`) decodes
// into. The regression case is the literal string "[]": an LLM that
// emits an empty array as text must NOT produce a phantom watched issue
// (which then 404s in the run console — see whats-next run 019ec0a1).
func TestExtractStringIDs(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"nil", nil, nil},
		{"empty string", "", nil},
		{"single id string", "native:abc", []string{"native:abc"}},
		{"typed slice", []string{"native:a", "", "native:b"}, []string{"native:a", "native:b"}},
		{"interface slice", []any{"native:a", "", 7, "native:b"}, []string{"native:a", "native:b"}},
		// Regression: stringified empty array → zero IDs, no phantom "[]".
		{"stringified empty array", "[]", nil},
		{"stringified empty array padded", "  []  ", nil},
		// Stringified populated array → its real elements.
		{"stringified populated array", `["native:a","native:b"]`, []string{"native:a", "native:b"}},
		{"stringified array with empties", `["native:a","",null]`, []string{"native:a"}},
		// Looks like an array but malformed → dropped, not watched verbatim.
		{"malformed array literal", "[native:a", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractStringIDs(c.in)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("extractStringIDs(%#v) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}
