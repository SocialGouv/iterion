package forge

import (
	"strings"
	"testing"
)

func TestParsePullURL(t *testing.T) {
	cases := []struct {
		in     string
		host   string
		repo   string
		number int
		ok     bool
	}{
		{"https://github.com/owner/repo/pull/42", "github.com", "owner/repo", 42, true},
		{"https://github.com/owner/repo/pull/42/files", "github.com", "owner/repo", 42, true},
		{"https://forge.example/owner/repo/pulls/7", "forge.example", "owner/repo", 7, true},
		{"https://gitlab.example/group/sub/proj/-/merge_requests/9", "gitlab.example", "group/sub/proj", 9, true},
		{"https://gitlab.example/group/proj/-/merge_requests/9/diffs", "gitlab.example", "group/proj", 9, true},
		{"https://gitlab.example/group/proj/merge_requests/3", "gitlab.example", "group/proj", 3, true},
		{"https://github.com/owner/repo/issues/42", "", "", 0, false},
		{"https://github.com/owner/repo/pull/abc", "", "", 0, false},
		{"ftp://github.com/owner/repo/pull/42", "", "", 0, false},
		{"github.com/owner/repo/pull/42", "", "", 0, false}, // no scheme
		{"", "", "", 0, false},
	}
	for _, tc := range cases {
		host, repo, n, err := ParsePullURL(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("ParsePullURL(%q): err=%v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if !tc.ok {
			continue
		}
		if host != tc.host || repo != tc.repo || n != tc.number {
			t.Errorf("ParsePullURL(%q) = (%q,%q,%d), want (%q,%q,%d)", tc.in, host, repo, n, tc.host, tc.repo, tc.number)
		}
	}
}

func TestFoldCommentsMarkdown(t *testing.T) {
	if got := FoldCommentsMarkdown(nil); got != "" {
		t.Fatalf("empty fold should be empty, got %q", got)
	}
	out := FoldCommentsMarkdown([]ReviewComment{
		{Path: "a.go", Line: 3, LineEnd: 5, Body: "off-by-one", Suggestion: "i <= n"},
		{Path: "b.go", Line: 9, Body: "plain finding"},
	})
	for _, want := range []string{"a.go:3-5", "off-by-one", "i <= n", "b.go:9", "plain finding"} {
		if !strings.Contains(out, want) {
			t.Errorf("folded markdown missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "```suggestion") {
		t.Error("folded (unanchored) comments must not render one-click suggestion fences")
	}
}
