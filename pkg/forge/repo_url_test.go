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
		{"https://github.com/acme/widgets.git/", "acme/widgets"},
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
		{"https://github.com/x/../acme/widgets.git", "acme/widgets"},
		{"https://github.com/./acme/widgets.git", "acme/widgets"},
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

// ProjectPathOnBase is the derivation every launch surface holding a forge
// base shares: the base is stripped whole (a sub-path forge's own path is
// never read as a subgroup), and a clone URL off the base is read on its own
// — the repository the run clones is the one it is metered by.
func TestProjectPathOnBase(t *testing.T) {
	cases := []struct{ base, in, want string }{
		{"https://github.com", "https://github.com/Acme/Widgets.git", "Acme/Widgets"},
		{"https://github.com/", "https://github.com/acme/widgets.git/", "acme/widgets"},
		// The sub-path base: stripped whole, so its own path is not a
		// subgroup of the slug.
		{"https://host.example/gitlab", "https://host.example/gitlab/group/sub/repo.git", "group/sub/repo"},
		// Off the base: the clone URL is read on its own.
		{"https://host.example/gitlab", "https://github.com/acme/widgets.git", "acme/widgets"},
		// No base: the base-less derivation.
		{"", "git@github.com:acme/widgets.git", "acme/widgets"},
		{"https://github.com", "", ""},
	}
	for _, c := range cases {
		if got := ProjectPathOnBase(c.base, c.in); got != c.want {
			t.Errorf("ProjectPathOnBase(%q, %q) = %q, want %q", c.base, c.in, got, c.want)
		}
	}
}

// CanonicalRepoSlug is the ONE trim every clone-URL→key derivation shares.
// A ".git/" tail is the spelling a copy-pasted URL carries as often as a
// bare ".git": a derivation that canonicalised one form and not the other
// split the quota key and let an over-ceiling repository launch under a key
// no quota aims at. The fold runs to a fixed point ("…git.git",
// "…git.git//") and folds the suffix's case — the suffix is URL furniture,
// not the slug's own spelling — while the slug's case is preserved (the
// reads fold it: budgetfloor's quota match, credusage's repository read).
func TestCanonicalRepoSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"acme/widgets", "acme/widgets"},
		{"acme/widgets.git", "acme/widgets"},
		{"acme/widgets.git/", "acme/widgets"},
		{"acme/widgets/", "acme/widgets"},
		{"/acme/widgets.git", "acme/widgets"},
		{"group/sub/repo.git/", "group/sub/repo"},
		{"Acme/Widgets.GIT/", "Acme/Widgets"}, // the suffix is furniture: any case folds
		{"acme/widgets.git.git", "acme/widgets"},
		{"acme/widgets.git.git//", "acme/widgets"},
		{"acme/.git", "acme"},
		{"", ""},
		{"/", ""},
		{".git", ""},
		// git (libcurl) collapses "." and ".." segments and doubled slashes
		// before it asks the forge: the key is the repository it clones.
		{"x/../acme/widgets", "acme/widgets"},
		{"./acme/widgets.git", "acme/widgets"},
		{"acme/./widgets", "acme/widgets"},
		{"acme//widgets.git/", "acme/widgets"},
		{"../acme/widgets", "acme/widgets"},
		{"acme/widgets/x/..", "acme/widgets"},
		// A dot inside a name is the name's own.
		{"my.org/my.repo", "my.org/my.repo"},
		{"acme/widgets.js.git", "acme/widgets.js"},
		{"acme/..widgets", "acme/..widgets"},
	}
	for _, c := range cases {
		if got := CanonicalRepoSlug(c.in); got != c.want {
			t.Errorf("CanonicalRepoSlug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Idempotent on its own output, whatever the spelling that produced it.
	for _, s := range []string{"acme/widgets", "Acme/Widgets", "group/sub/repo"} {
		if got := CanonicalRepoSlug(CanonicalRepoSlug(s)); got != CanonicalRepoSlug(s) {
			t.Errorf("CanonicalRepoSlug(%q) = %q, not a fixed point", s, got)
		}
	}
}
