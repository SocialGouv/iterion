package permission

import (
	"reflect"
	"testing"
)

// The bare-string form is what a checkpoint written by a previous build
// carries; the slice forms are the current accumulated set, before and
// after a JSON round-trip through that checkpoint.
func TestGrantsFrom(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want []string
	}{
		{"legacy single grant", "Write", []string{"Write"}},
		{"legacy empty", "", nil},
		{"accumulated set", []string{"Write", "Bash(git add:*)"}, []string{"Write", "Bash(git add:*)"}},
		{"after a JSON round-trip", []any{"Write", "Bash(git add:*)"}, []string{"Write", "Bash(git add:*)"}},
		{"blanks dropped", []string{"Write", "", "Edit"}, []string{"Write", "Edit"}},
		{"non-strings dropped", []any{"Write", 42, nil}, []string{"Write"}},
		{"absent", nil, nil},
		{"unexpected shape", map[string]any{"a": 1}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := GrantsFrom(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("GrantsFrom(%#v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// The whole point of the accumulated set: a policy rebuilt from scratch
// at the next pause still honours a grant earned at an earlier one.
func TestAccumulatedGrantsSurviveAPolicyRebuild(t *testing.T) {
	build := func(grants any) *Policy {
		p, err := NewPolicy(ModeAsk, []string{"Read(**)", "Glob", "Grep"}, nil, nil)
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		for _, rule := range GrantsFrom(grants) {
			p.AddAllowRule(rule)
		}
		return p
	}

	// Pause 1: the operator answers `allow always` on Write.
	first := build([]string{"Write"})
	if d, _ := first.Evaluate("Write", map[string]any{"file_path": "/wt/docs/x.md"}); d != Allow {
		t.Fatalf("Write right after its own grant = %v, want allow", d)
	}

	// Pause 2 (a Bash commit): the run carries BOTH grants. Before the
	// fix only the newest reached here, so Write went back to Ask and the
	// operator re-authorized it on every document.
	second := build([]string{"Write", "Bash(git add:*)"})
	if d, _ := second.Evaluate("Write", map[string]any{"file_path": "/wt/docs/y.md"}); d != Allow {
		t.Errorf("Write at a later pause = %v, want allow (the grant must outlive the pause it was earned on)", d)
	}
	if d, _ := second.Evaluate("Bash", map[string]any{"command": "git add docs/y.md"}); d != Allow {
		t.Errorf("Bash at its own pause = %v, want allow", d)
	}
	// Nothing else widened.
	if d, _ := second.Evaluate("Edit", map[string]any{"file_path": "/wt/docs/y.md"}); d != Ask {
		t.Errorf("Edit = %v, want ask — accumulating grants must not grant what was never asked", d)
	}
}
