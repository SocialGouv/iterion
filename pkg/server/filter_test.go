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
		{".", true},   // technically starts with "."
		{"..", true},  // same
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
		{"foo.iter", true},
		{"foo.bot", true},
		{"foo.iter.tmp", false},
		{"foo", false},
		{"foo.go", false},
		{".iter", true},
		{"path/to/foo.iter", true},
		{"path/to/foo.bot", true},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := isWorkflowFile(c.path); got != c.want {
				t.Errorf("isWorkflowFile(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}
