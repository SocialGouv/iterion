package cli_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CHANGELOG.md is generated — by release-it at every release, and by
// `task changelog:gen` on a re-split (scripts/changelog-writer.mjs holds the
// rendering both share). Nobody reads 219 KB of it by eye, so the failure
// modes below are silent and cumulative. This guard asserts only structural
// invariants that hold for EVERY future release, never a count, so a normal
// release commit can never turn it red.

var (
	// The `why` excerpt block scripts/changelog-writer.mjs appends to an entry.
	changelogExcerpt  = regexp.MustCompile(`(?s)<details><summary>why</summary>\n\n(.*?)\n\n\s*</details>`)
	changelogH1       = regexp.MustCompile(`(?m)^# Changelog[ \t]*$`)
	changelogAlnum    = regexp.MustCompile(`[a-zA-Z0-9]`)
	changelogCodeSpan = regexp.MustCompile("`[^`]*`")
	// The `export const HEADER = ` + "`...`" + ` template literal. It contains no
	// backtick and no ${} interpolation, so a lazy match to the closing
	// backtick is exact — and if someone introduces either, this stops
	// matching and the test says so rather than silently passing.
	changelogHeaderDecl = regexp.MustCompile("(?s)export const HEADER = `(.*?)`")
)

// TestChangelogHeaderMatchesGenerator keeps CHANGELOG.md's preamble byte-equal
// to the HEADER the generator will use.
//
// @release-it/conventional-changelog strips the configured header from the
// existing file with a literal `previousChangelog.replace(header, <empty>)`
// before prepending it again. That strip is an exact string match, so editing HEADER's
// prose WITHOUT regenerating CHANGELOG.md in the same commit means the next
// release finds nothing to strip, prepends the new header on top of the old
// one, and buries every existing section below it — once more per release.
//
// TestChangelogStructure catches that, but only after the release has already
// landed. This catches the drift on the PR that introduces it.
func TestChangelogHeaderMatchesGenerator(t *testing.T) {
	root := repoRootForDocsTest(t)

	src, err := os.ReadFile(filepath.Join(root, "scripts", "changelog-writer.mjs"))
	if err != nil {
		t.Fatalf("read scripts/changelog-writer.mjs: %v", err)
	}
	m := changelogHeaderDecl.FindSubmatch(src)
	if m == nil {
		t.Fatal("could not find the `export const HEADER = ` template literal in " +
			"scripts/changelog-writer.mjs — if its shape changed, update this guard")
	}
	header := string(m[1])

	md, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	if !strings.HasPrefix(string(md), header) {
		t.Errorf("CHANGELOG.md does not open on the generator's HEADER.\n"+
			"Editing HEADER requires regenerating in the SAME commit (`task changelog:gen`), "+
			"or the next release duplicates the preamble.\nwant prefix:\n%q\ngot:\n%q",
			header, string(md[:min(len(md), len(header)+40)]))
	}
}

func TestChangelogStructure(t *testing.T) {
	root := repoRootForDocsTest(t)
	raw, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	md := string(raw)

	// The whole `infile` scheme rests on @release-it/conventional-changelog
	// stripping the configured header with a literal, unanchored
	// String.replace before re-emitting it on top. If a future plugin version
	// switches to a RegExp, or normalizes EOLs before comparing, the strip
	// misses: the preamble is then prepended a second time and every existing
	// release sinks below it — compounding once per release, unnoticed.
	if n := len(changelogH1.FindAllString(md, -1)); n != 1 {
		t.Errorf("CHANGELOG.md has %d `# Changelog` H1s, want exactly 1 — "+
			"the release-it header strip likely missed and duplicated the preamble", n)
	}
	if !strings.HasPrefix(md, "# Changelog\n") {
		t.Error("CHANGELOG.md must open on the `# Changelog` header; prose above the " +
			"newest release sinks under it at the next release")
	}

	// The generator emits a phantom leading section when HEAD is past the
	// newest tag; scripts/changelog-gen.mjs filters it out. Reaching the
	// committed file means that filter stopped matching.
	if strings.Contains(md, "## [undefined]") {
		t.Error("CHANGELOG.md contains a `## [undefined]` section")
	}

	if open, closed := strings.Count(md, "<details>"), strings.Count(md, "</details>"); open != closed {
		t.Errorf("unbalanced excerpt markup: %d <details> vs %d </details>", open, closed)
	}

	// An excerpt with nothing readable in it renders as an empty disclosure —
	// or, for the `---------` rule GitHub writes above the trailers of a
	// squashed PR, as a bare <hr>. isProse is what keeps those out.
	for _, m := range changelogExcerpt.FindAllStringSubmatch(md, -1) {
		if !changelogAlnum.MatchString(m[1]) {
			t.Errorf("excerpt with no readable content: %q", strings.TrimSpace(m[1]))
		}

		// Commit bodies are full of `<placeholder>` notation. Unescaped and
		// outside a code span, GitHub's sanitizer reads it as an unknown tag
		// and DELETES it, so the rendered changelog silently loses the word
		// ("iterion-sandbox-slim:, a tag nobody pushed"). escapeAngles in
		// scripts/changelog-writer.mjs backslash-escapes them; a `<` that
		// arrives here bare means it stopped doing so.
		outside := changelogCodeSpan.ReplaceAllString(m[1], "")
		for i := 0; i < len(outside); i++ {
			if outside[i] == '<' && (i == 0 || outside[i-1] != '\\') {
				t.Errorf("unescaped `<` outside a code span (GitHub will strip the token): %q",
					strings.TrimSpace(outside[max(0, i-40):min(len(outside), i+40)]))
				break
			}
		}
	}
}
