package workflowfile

import "testing"

func TestIsWorkflowFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"foo.yaml", false},
		{"foo.bot", true},
		{"path/to/foo.yaml", false},
		{"path/to/foo.bot", true},
		{"foo.txt", false},
		{"foo", false},
		{"foo.bot.bak", false},
		{".yaml", false},
		{".bot", true},
		{"foo.botz", false},
		{"", false},
		// One rule with bundle.Detect (#1762): the suffix is case-folded, so
		// a file the launcher takes, the walks, fmt and the storage routes
		// take too.
		{"RUN.BOT", true},
		{"path/to/RUN.BOT", true},
		{"foo.Bot", true},
		{"foo.bOt", true},
		{"FOO.BOTZ", false}, // .botz is the archive's door, not IsWorkflowFile's
	}
	for _, tc := range cases {
		if got := IsWorkflowFile(tc.path); got != tc.want {
			t.Errorf("IsWorkflowFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestSkipWalkDirIsTheOneRule pins what a walk over a source tree does not
// descend into. It is shared on purpose: `iterion fmt`'s collector and the
// guards that check the same tree have to enumerate the same files, and two
// spellings of the rule put them in a disagreement no list can settle — a
// `.bot` under `.iterion/` was refused by one and invisible to the other,
// with no content of `.fmt-refused` able to make both green.
func TestSkipWalkDirIsTheOneRule(t *testing.T) {
	for name, skip := range map[string]bool{
		".iterion":     true,
		".git":         true,
		".works":       true,
		".repos":       true,
		"vendor":       true,
		"node_modules": true,
		"bots":         false,
		"examples":     false,
		"lib":          false,
		"testdata":     false,
		"my.dir":       false,
	} {
		if got := SkipWalkDir(name); got != skip {
			t.Errorf("SkipWalkDir(%q) = %v, want %v", name, got, skip)
		}
	}
}
