package reviewtopology

import (
	"reflect"
	"testing"
)

// TestPinnedFamilies pins the reading the publisher's want widening depends
// on: a mono pin is a credential DEMAND, and only a MONO run's pin is one.
// Mutant: reading mono_family regardless of review_mode makes the dual case
// red — a dual run would ask the pool for a family it never spends.
func TestPinnedFamilies(t *testing.T) {
	cases := []struct {
		name string
		vars map[string]string
		want []string
	}{
		{"mono pin gpt", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: "gpt"}, []string{FamilyGPT}},
		{"mono pin claude", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: "claude"}, []string{FamilyClaude}},
		{"pin is case-folded and trimmed", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: " GPT "}, []string{FamilyGPT}},
		{"dual ignores a stale pin", map[string]string{VarReviewMode: ModeDual, VarMonoFamily: "gpt"}, nil},
		{"auto ignores the pin", map[string]string{VarReviewMode: ModeAuto, VarMonoFamily: "gpt"}, nil},
		{"no mode, no demand", map[string]string{VarMonoFamily: "gpt"}, nil},
		{"unknown name dropped", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: "gemini"}, nil},
		{"duplicates collapse", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: "gpt,gpt"}, []string{FamilyGPT}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PinnedFamilies(c.vars); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("PinnedFamilies(%v) = %v, want %v", c.vars, got, c.want)
			}
		})
	}
}
