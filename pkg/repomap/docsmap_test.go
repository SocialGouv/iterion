package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/mdcode"
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

// A page that merely QUOTES a link form inside backticks carries no link:
// the string is inline code, and both readers of the map render it as text.
// Rewriting it edits quoted prose, and refusing it fails `task map:gen` — and
// so the required `test` check — on a page whose own links are all sound. The
// page most likely to write one of these is the page documenting the rule.
//
// Each row is a form the corpus can write. The mutation that reddens them all
// is scanning the raw sentence instead of internal/mdcode's mask.
func TestReanchorReadsACodeSpanAsCodeAndNotAsALink(t *testing.T) {
	for _, tc := range []struct{ name, page, quote, want string }{
		{
			// The ticket's own page: this form used to abort the generator.
			name:  "a single-backtick span carrying a target that leaves the repository",
			page:  "docs/why.md",
			quote: "The docs map used to emit `](../../docs/foo.md)`, which github.com renders and the site cannot.",
			want:  "The docs map used to emit `](../../docs/foo.md)`, which github.com renders and the site cannot.",
		},
		{
			name:  "a single-backtick span carrying a target that would be rewritten",
			page:  "docs/guide.md",
			quote: "Write `](adr/081.md)` and the map re-anchors it.",
			want:  "Write `](adr/081.md)` and the map re-anchors it.",
		},
		{
			// Two backticks are how a span that itself carries a backtick is
			// written, which is exactly how a doc quotes a link to a `file`.
			name:  "a double-backtick span",
			page:  "docs/guide.md",
			quote: "Written ``](`x`.md)`` in the page.",
			want:  "Written ``](`x`.md)`` in the page.",
		},
		{
			name:  "a full link inside a span",
			page:  "docs/guide.md",
			quote: "The form `[the page](../../docs/dsl.md)` is the one that breaks.",
			want:  "The form `[the page](../../docs/dsl.md)` is the one that breaks.",
		},
		{
			// The mask must not disarm the feature: a real link in the same
			// sentence as a quoted one is still re-anchored, at its own offset.
			name:  "a real link beside a quoted one is still re-anchored",
			page:  "docs/guide.md",
			quote: "See [the decisions](adr/081.md), never `](../../docs/foo.md)`.",
			want:  "See [the decisions](../adr/081.md), never `](../../docs/foo.md)`.",
		},
		{
			name:  "a quoted form before a real link does not shift it",
			page:  "docs/guide.md",
			quote: "Never `](../../x.md)`, always [the page](sub/page.md).",
			want:  "Never `](../../x.md)`, always [the page](../sub/page.md).",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reanchor(tc.quote, tc.page)
			if err != nil {
				t.Fatalf("the quoted form was refused as a broken link: %v", err)
			}
			if got != tc.want {
				t.Errorf("reanchor page %s\n  quote %s\n  got  %s\n  want %s", tc.page, tc.quote, got, tc.want)
			}
		})
	}
}

// The summary is TRUNCATED before it is masked, and the cut lands where a
// byte bound falls. A cut between a code span's two runs leaves half a span,
// the mask correctly reads the surviving run as literal text, and the link
// form the page was quoting is read as a link again — so the refusal fires
// and `task map:gen`, with the required `test` check, goes red on a page
// whose own links are all sound. That is the failure #1619 exists to remove,
// reachable through the one path the first fix did not cover.
//
// The sentences below are over 140 bytes on purpose: every other case in this
// file is under the bound and cannot see this.
func TestALongSentenceQuotingALinkFormIsNotCutInsideItsCodeSpan(t *testing.T) {
	for _, tc := range []struct{ name, quote string }{
		{
			name:  "a span carrying a target that leaves the repository",
			quote: "Avant ce correctif, la carte des docs réécrivait la phrase d'ouverture sans lire les spans et émettait la forme `](../../docs/foo.md) ou sa voisine ](../foo.md)`, que github.com rend et que le site ignore.",
		},
		{
			// The quoted target RESOLVES, so a broken repair does not trip
			// the refusal above — only the balance oracle can catch it, which
			// is what makes that assertion load-bearing rather than a second
			// copy of the error check.
			name:  "a resolvable target quoted in the cut span",
			quote: "The docs map reproduced the opening sentence of every page with no notion of a code span, so it read the quoted `](adr/081.md) and its neighbour ](guide.md)` as links the page had written.",
		},
		{
			// The dangling run sits BEFORE a later closed span of a
			// different width — a page quoting both a `…` form and a ``…``
			// form, which is what the page documenting this rule writes. A
			// repair that looked only past the LAST span reported nothing to
			// repair and the generator aborted.
			name:  "a cut span followed by a closed span of another width",
			quote: "Le correctif de la carte des docs evoque la forme `](../../docs/foo.md) puis la forme ``doublee`` que la page cite sans jamais la suivre et rien de plus.",
		},
		{
			// The cut lands at the last space before the bound, so the span
			// has to CARRY spaces for the cut to fall inside it — which is
			// what a span quoting two forms, or a sentence, always does.
			name:  "a span carrying spaces, cut between its two runs",
			quote: "The docs map reproduced the opening sentence of every page with no notion of a code span, so it read the quoted `](../../docs/foo.md) and its neighbour ](../bar.md)` as links the page had written.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary := firstSentence(tc.quote, 140)
			got, err := reanchor(summary, "docs/why.md")
			if err != nil {
				t.Fatalf("the truncated sentence was refused as a broken link: %v\n  summary: %s", err, summary)
			}
			// The cell must not end mid-span either: a stray backtick is what
			// makes every reader disagree about where the code was. Asked of
			// mdcode.Spans, never of CloseDanglingSpan — firstSentence has
			// already applied that function, so asking it again is a
			// tautology that stays green for ANY implementation, including
			// none at all.
			if pos, dangling := backtickOutsideASpan(got); dangling {
				t.Errorf("the summary ends inside a code span (byte %d): %s", pos, got)
			}
		})
	}
}

// backtickOutsideASpan is the independent oracle for "this cell is
// code-complete": it asks where the spans ARE, not whether the repair says it
// succeeded.
func backtickOutsideASpan(s string) (int, bool) {
	spans := mdcode.Spans(s)
	for i := 0; i < len(s); i++ {
		if s[i] != '`' {
			continue
		}
		inside := false
		for _, r := range spans {
			if i >= r[0] && i < r[1] {
				inside = true
				break
			}
		}
		if !inside {
			return i, true
		}
	}
	return 0, false
}

// A page that shows a ```-fence by wrapping it in another fence is ordinary
// markdown, and the inner `~~~` or `> ```sh` line is CONTENT. A scanner that
// recognises those as delimiters but closes on any delimiter ends the outer
// block early, and the "opening sentence" it then reads is quoted code —
// which reanchor rewrites, or refuses, aborting `iterion map gen` on a page
// whose own links are sound.
//
// The mutation that reddens this: a naive toggle in readDocRow, whatever set
// of lines it recognises.
func TestAFenceShownInsideAFenceDoesNotEndIt(t *testing.T) {
	// The property, not a list of spellings: a block ends only at a delimiter
	// matching its OPENER — same character, at least as long, same blockquote
	// depth, no further indented. Each row differs from the opener in exactly
	// one of those, so none of them may close it. A table shaped to the two
	// spellings that happened to fail stayed green on the next two.
	for _, tc := range []struct{ name, inner string }{
		{"a different fence character", "~~~"},
		{"a deeper blockquote, with an info string", "> ```sh"},
		{"a deeper blockquote, bare", "> ```"},
		{"further indented", "    ```"},
		{"a shorter run", "``"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			body := "# Hazard\n\n```markdown\n" + tc.inner +
				"\nNever write the form ](../../elsewhere/foo.md) — the site ignores it.\n" +
				tc.inner + "\n```\n\nThe real opening sentence, which links to [the guide](guide.md).\n"
			for rel, text := range map[string]string{
				"docs/hazard.md": body,
				"docs/guide.md":  "# Guide\n\nA guide.\n",
			} {
				abs := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(abs, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var docs Extractor
			for _, e := range Extractors() {
				if e.Stem() == "docs" {
					docs = e
				}
			}
			out, err := docs.Extract(root)
			if err != nil {
				t.Fatalf("the extractor refused a page whose links are sound: %v", err)
			}
			if !strings.Contains(out, "The real opening sentence") {
				t.Errorf("the summary is not the page's prose:\n%s", out)
			}
			if strings.Contains(out, "elsewhere/foo.md") {
				t.Errorf("a link form quoted inside the fenced example reached the map:\n%s", out)
			}
		})
	}
}

// A page may open with a YAML front-matter block, and its keys are not the
// page's prose. Six rows of the committed docs map published `title:
// Changelog` and `layout: home` as the opening sentence because this scanner
// had never heard of front matter while the documentation link checker had —
// two consumers, two answers about where a page begins.
//
// Latently worse than cosmetic: docs/index.md carries a 66-line front matter
// holding a `](visual-editor.md)` form, which reanchor would rewrite the day
// it became the first prose-shaped line.
func TestFrontMatterIsNotThePagesOpeningSentence(t *testing.T) {
	root := t.TempDir()
	body := "---\nlayout: home\ntitle: Changelog\nhero:\n  text: See [the editor](visual-editor.md)\n---\n\n" +
		"# The page\n\nThe real opening sentence, which links to [the guide](guide.md).\n"
	for rel, text := range map[string]string{
		"docs/front.md": body,
		"docs/guide.md": "# Guide\n\nA guide.\n",
		// A `---` that is NOT on the first line is a horizontal rule, and the
		// prose after it is still prose.
		"docs/rule.md": "# Rule\n\n---\n\nProse after a horizontal rule.\n",
	} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var docs Extractor
	for _, e := range Extractors() {
		if e.Stem() == "docs" {
			docs = e
		}
	}
	out, err := docs.Extract(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"layout: home", "title: Changelog", "visual-editor.md"} {
		if strings.Contains(out, key) {
			t.Errorf("a front-matter key reached the map (%q):\n%s", key, out)
		}
	}
	if !strings.Contains(out, "The real opening sentence") {
		t.Errorf("the page's prose is not the summary:\n%s", out)
	}
	if !strings.Contains(out, "Prose after a horizontal rule") {
		t.Errorf("a `---` below the first line was read as front matter:\n%s", out)
	}
}
