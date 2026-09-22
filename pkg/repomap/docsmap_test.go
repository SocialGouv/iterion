package repomap

import (
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
			name:  "a directory target keeps resolving to the directory",
			page:  "docs/guide.md",
			quote: "The [tools](sub/) it ships",
			want:  "The [tools](../sub) it ships",
		},
		{
			name:  "external and root-absolute targets are left alone",
			page:  "docs/guide.md",
			quote: "The [site](https://example.com/x), [cdn](//cdn.example.com), [root](/docs/a.csv)",
			want:  "The [site](https://example.com/x), [cdn](//cdn.example.com), [root](/docs/a.csv)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reanchor(tc.quote, tc.page); got != tc.want {
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
	if got := reanchor(prose, "docs/guide.md"); got != prose {
		t.Errorf("plain prose moved: %s", got)
	}
}
