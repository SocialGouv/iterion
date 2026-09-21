package cli

import "testing"

// TestPickString_ExplicitWinsElseInherits pins the doctrine every
// launch-time override on resume follows: an explicit resume-time
// value replaces the record's persisted launch decision, an empty
// explicit inherits it. Mirrors mergePersistedResumeBudget's
// per-field merge for the string-valued overrides added in #1435 /
// #1366 (--sandbox, --merge-into, --branch-name, --merge-strategy,
// --sandbox-default-image, --sandbox-host-state). Without this
// semantics, a resume that says nothing would clobber a launch
// override with an empty string.
//
// Mutation: swap the branches — return persisted when explicit is
// non-empty — this test reddens on the first sub-case.
func TestPickString_ExplicitWinsElseInherits(t *testing.T) {
	cases := []struct {
		name      string
		explicit  string
		persisted string
		want      string
	}{
		{"explicit-wins-over-persisted", "none", "docker", "none"},
		{"empty-explicit-inherits", "", "docker", "docker"},
		{"both-empty-stays-empty", "", "", ""},
		{"explicit-only", "auto", "", "auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickString(tc.explicit, tc.persisted); got != tc.want {
				t.Errorf("pickString(explicit=%q, persisted=%q) = %q, want %q", tc.explicit, tc.persisted, got, tc.want)
			}
		})
	}
}
