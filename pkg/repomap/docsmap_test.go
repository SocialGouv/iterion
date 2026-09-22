package repomap

import (
	"strings"
	"testing"
)

// The summary column quotes the page's opening sentence, relative links
// included. A link resolves against the page that carries it, so on the
// map — one directory level away — the quoted target named nothing
// (five broken links on main within a day of the map landing). The
// re-anchor is what keeps the quoted link pointing at the file it names;
// its mutation to identity reddens every case that carries a relative or
// same-page target.
func TestReanchorRewritesTheTargetsAQuotedLinkCarries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		page  string
		quote string
		want  string
	}{
		{
			// docs/async-interaction.md:136, quoted on the map.
			name:  "a sibling path of the quoted page",
			page:  "docs/async-interaction.md",
			quote: "ADR: [081-async-human-interaction](adr/081-async-human-interaction.md)",
			want:  "ADR: [081-async-human-interaction](../adr/081-async-human-interaction.md)",
		},
		{
			// docs/bot-runs/bmady.md:149 and its three siblings, quoted on the map.
			name:  "a file next to the quoted page",
			page:  "docs/bot-runs/bmady.md",
			quote: "Index + template: [README.md](README.md)",
			want:  "Index + template: [README.md](../bot-runs/README.md)",
		},
		{
			name:  "a bare fragment names the quoted page's own anchor",
			page:  "docs/async-interaction.md",
			quote: "The [flow](#flow) below is the reference",
			want:  "The [flow](../async-interaction.md#flow) below is the reference",
		},
		{
			name:  "a fragment rides its target",
			page:  "docs/guide.md",
			quote: "See [the intro](guide.md#intro) and [its end](guide.md#end)",
			want:  "See [the intro](../guide.md#intro) and [its end](../guide.md#end)",
		},
		{
			// The slash is the whole signal: the site routes `../sub/` to a
			// github.com tree URL and `../sub` to a page it never builds.
			name:  "a directory target keeps its trailing slash",
			page:  "docs/guide.md",
			quote: "The [tools](sub/) it ships",
			want:  "The [tools](../sub/) it ships",
		},
		{
			// The slash belongs to the path, not to the fragment: every
			// reader splits the `#` off first and looks for a trailing slash
			// on the path alone, so `../adr#why/` never reaches the directory
			// rule and is routed as a page that was never built.
			name:  "a directory target keeps its slash before the fragment",
			page:  "docs/guide.md",
			quote: "The [decisions](adr/#why) behind it",
			want:  "The [decisions](../adr/#why) behind it",
		},
		{
			name:  "a nested directory target keeps its trailing slash",
			page:  "docs/bot-runs/bmady.md",
			quote: "The [decisions](../adr/) behind it",
			want:  "The [decisions](../adr/) behind it",
		},
		{
			name:  "external and root-absolute targets are left alone",
			page:  "docs/guide.md",
			quote: "The [site](https://example.com/x), [cdn](//cdn.example.com), [root](/docs/a.csv)",
			want:  "The [site](https://example.com/x), [cdn](//cdn.example.com), [root](/docs/a.csv)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reanchor(tc.quote, tc.page)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("reanchor page %s\n  quote %s\n  got  %s\n  want %s", tc.page, tc.quote, got, tc.want)
			}
		})
	}
}

// The re-anchor must not invent links either: prose without `](…)` passes
// through byte-identical, and the freshness gate pins the committed map to
// the tree.
func TestReanchorLeavesProseWithoutLinksUntouched(t *testing.T) {
	const prose = "It explains the thing. See `code` and _emphasis_, no links."
	got, err := reanchor(prose, "docs/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != prose {
		t.Errorf("plain prose moved: %s", got)
	}
}

// A quoted target that climbs above the repository root names a file no
// reader of the map can open, and re-anchoring only makes it climb further
// (`docs/guide.md` quoting `../../pkg/z.go` renders `../../../pkg/z.go`).
// The generator refuses it and names the page rather than writing it down.
func TestReanchorRefusesATargetThatLeavesTheRepository(t *testing.T) {
	for _, tc := range []struct{ name, quote string }{
		{"a target above the repository root", "See [it](../../pkg/z.go) for the detail"},
		{"the repository root itself", "See [it](..) for the detail"},
		{"the repository root as a directory", "See [it](../) for the detail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reanchor(tc.quote, "docs/guide.md")
			if err == nil {
				t.Fatalf("%s was rewritten instead of refused", tc.name)
			}
			// The author greps for what the page wrote, not for the resolved form.
			if !strings.Contains(err.Error(), tc.quote[strings.Index(tc.quote, "]("):strings.Index(tc.quote, ")")+1]) {
				t.Errorf("the refusal does not name the target as the page wrote it: %v", err)
			}
		})
	}
}

// Which refusal a single variable would keep is decided by the order of the
// targets in the sentence, and both have to be fixed: every one is named.
func TestReanchorNamesEveryTargetThatLeavesTheRepository(t *testing.T) {
	_, err := reanchor("Both [a](../../pkg/z.go) and [b](../../etc/passwd) are quoted", "docs/guide.md")
	if err == nil {
		t.Fatal("two targets climbing out of the repository were rewritten instead of refused")
	}
	for _, want := range []string{"](../../pkg/z.go)", "](../../etc/passwd)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
}
