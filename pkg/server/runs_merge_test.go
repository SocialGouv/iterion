package server

import "testing"

func TestSameForgeHost(t *testing.T) {
	cases := []struct {
		base, repo string
		want       bool
	}{
		{"https://gitlab.example.com", "https://gitlab.example.com/group/repo", true},
		{"https://gitlab.example.com", "https://GitLab.Example.COM/group/repo.git", true},
		{"https://gitlab.example.com", "https://gitlab.other.com/group/repo", false},
		{"https://github.com", "https://github.com/org/repo", true},
		// Port differences are ignored: the connection base is
		// canonicalised to scheme+host at connect time.
		{"https://gitlab.example.com", "https://gitlab.example.com:443/group/repo", true},
		// Unparseable or hostless inputs never match — the caller
		// refuses rather than guessing.
		{"", "https://gitlab.example.com/group/repo", false},
		{"https://gitlab.example.com", "", false},
		{"://bad", "https://gitlab.example.com/x", false},
	}
	for _, c := range cases {
		if got := sameForgeHost(c.base, c.repo); got != c.want {
			t.Errorf("sameForgeHost(%q, %q) = %t, want %t", c.base, c.repo, got, c.want)
		}
	}
}
