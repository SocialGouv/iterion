package server

import "testing"

func TestIsSkippedDir(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", false},
		{"src", false},
		{"node_modules", true},
		{".git", true},
		{".idea", true},
		{".devcontainer", true},
		{"dist", false},
		{".", true},  // technically starts with "."
		{"..", true}, // same
		{"a.b", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSkippedDir(c.name); got != c.want {
				t.Errorf("isSkippedDir(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestIsWorkflowFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"foo.yaml", false},
		{"foo.bot", true},
		{"RUN.BOT", true}, // the suffix is case-folded, one rule with Detect (#1762)
		{"foo.bot.tmp", false},
		{"foo", false},
		{"foo.go", false},
		{".yaml", false},
		{"path/to/foo.yaml", false},
		{"path/to/foo.bot", true},
		{"path/to/foo.botz", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := isWorkflowFile(c.path); got != c.want {
				t.Errorf("isWorkflowFile(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}
