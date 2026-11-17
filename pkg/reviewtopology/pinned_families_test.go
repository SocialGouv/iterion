package reviewtopology

import (
	"reflect"
	"testing"
)

// TestPinnedFamilies pins the reading the publisher's want widening depends
// on: a pin is a credential DEMAND unless the run is explicitly DUAL — auto
// resolves to mono and honors the pin, so it demands too, and the dedicated
// launch field wins over the var as it does at injection.
// Mutants: gating on vars[review_mode] alone reds the explicit-mono and
// auto cases; demanding on dual reds the suppression cases.
func TestPinnedFamilies(t *testing.T) {
	cases := []struct {
		name     string
		vars     map[string]string
		explicit string
		want     []string
	}{
		{"mono var pin gpt", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: "gpt"}, "", []string{FamilyGPT}},
		{"mono var pin claude", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: "claude"}, "", []string{FamilyClaude}},
		{"pin is case-folded and trimmed", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: " GPT "}, "", []string{FamilyGPT}},
		{"dual var ignores a stale pin", map[string]string{VarReviewMode: ModeDual, VarMonoFamily: "gpt"}, "", nil},
		// AUTO is not dual: the run resolves to mono, honors the pin and
		// would die on the same missing credential — the walk must ask.
		{"auto var still demands", map[string]string{VarReviewMode: ModeAuto, VarMonoFamily: "gpt"}, "", []string{FamilyGPT}},
		{"no mode still demands", map[string]string{VarMonoFamily: "gpt"}, "", []string{FamilyGPT}},
		// The dedicated launch field wins over the var, exactly as at injection.
		{"explicit mono demands over an absent var", map[string]string{VarMonoFamily: "gpt"}, ModeMono, []string{FamilyGPT}},
		{"explicit dual suppresses a mono var", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: "gpt"}, ModeDual, nil},
		{"unknown name dropped", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: "gemini"}, "", nil},
		{"duplicates collapse", map[string]string{VarReviewMode: ModeMono, VarMonoFamily: "gpt,gpt"}, "", []string{FamilyGPT}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PinnedFamilies(c.vars, c.explicit); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("PinnedFamilies(%v, %q) = %v, want %v", c.vars, c.explicit, got, c.want)
			}
		})
	}
}
