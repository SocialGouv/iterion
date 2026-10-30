package forge

import "testing"

// ProjectPathFromCloneURL is the identity bridge of the per-repo quota: the
// board lane's card carries a clone URL, and the meter the quota reads keys
// on the forge slug. The derivation must return byte-for-byte the slug the
// webhook payload stamped — case preserved, ".git" and scheme forms
// canonicalised away — or the same repository is charged under two keys and
// its quota reads as half-used on each.
func TestProjectPathFromCloneURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://github.com/acme/widgets.git", "acme/widgets"},
		{"https://github.com/acme/widgets", "acme/widgets"},
		{"https://github.com/acme/widgets/", "acme/widgets"},
		// The forge's own casing survives: GitHub writes "Acme/Widgets" and
		// the meter keys on it byte-for-byte — lowercasing would split the
		// quota between two keys.
		{"https://github.com/Acme/Widgets.git", "Acme/Widgets"},
		// GitLab subgroups are deeper paths, all of them kept.
		{"https://gitlab.example.com/group/sub/repo.git", "group/sub/repo"},
		{"http://forgejo.local:3000/o/r.git", "o/r"},
		{"ssh://git@github.com/acme/widgets.git", "acme/widgets"},
		{"git@github.com:acme/widgets.git", "acme/widgets"},
		{"  https://github.com/acme/widgets.git  ", "acme/widgets"},
		// A bare slug round-trips, so the function is idempotent on its own
		// output and safe on a card that already carried one.
		{"acme/widgets", "acme/widgets"},
		{"acme/widgets.git", "acme/widgets"},
		// Nothing usable: the host alone is not a repository.
		{"", ""},
		{"   ", ""},
		{"https://github.com", ""},
		{"https://github.com/", ""},
		{"git@github.com", ""},
	}
	for _, c := range cases {
		if got := ProjectPathFromCloneURL(c.in); got != c.want {
			t.Errorf("ProjectPathFromCloneURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The round-trip the whole mechanism rests on: a slug the launch surfaces
// stamp, turned into a clone URL by CloneURLFor, must come back byte-for-byte
// — a card charged under anything else escapes its repo's quota by spelling.
func TestProjectPathFromCloneURL_RoundTripsCloneURLFor(t *testing.T) {
	for _, slug := range []string{"acme/widgets", "Acme/Widgets", "group/sub/repo"} {
		if got := ProjectPathFromCloneURL(CloneURLFor("https://gitlab.example.com", slug)); got != slug {
			t.Errorf("round-trip of %q = %q", slug, got)
		}
	}
}
