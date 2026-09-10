package bundle

import "testing"

// The frontmatter block is made of comment lines, so it follows the lexer's
// definition of a comment: `# ---` is the same fence as `## ---`, a `# note`
// above the block is a comment like any other, and `# name: x` inside it is
// the same line as `## name: x`. Before this held, a valid `.bot` lost its
// name, description and triggers to a single-hash comment.
func TestParseFrontmatterAcceptsSingleHashComments(t *testing.T) {
	cases := map[string]string{
		"double-hash block": "## ---\n## name: Alpha Bot\n## description: the catalog description\n## triggers:\n##   - nightly\n## ---\n\nworkflow w:\n  entry: a\n",
		"single-hash block": "# ---\n# name: Alpha Bot\n# description: the catalog description\n# triggers:\n#   - nightly\n# ---\n\nworkflow w:\n  entry: a\n",
		"mixed hashes":      "## ---\n# name: Alpha Bot\n## description: the catalog description\n# triggers:\n##   - nightly\n# ---\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			fm := ParseFrontmatter([]byte(src))
			if fm == nil {
				t.Fatal("no frontmatter parsed")
			}
			if fm.Name != "Alpha Bot" || fm.Description != "the catalog description" {
				t.Errorf("name/description = %q / %q", fm.Name, fm.Description)
			}
			if len(fm.Triggers) != 1 || fm.Triggers[0] != "nightly" {
				t.Errorf("triggers = %v, want [nightly]", fm.Triggers)
			}
		})
	}
	if ParseFrontmatter([]byte("workflow w:\n  entry: a\n")) != nil {
		t.Error("a file without a fence has no frontmatter")
	}
	// The block still has to be the first thing in the file: a comment above
	// the fence, of either hash count, means "no frontmatter" — the same rule
	// as before, now applied evenly.
	if ParseFrontmatter([]byte("# a note\n## ---\n## name: Alpha Bot\n## ---\n")) != nil {
		t.Error("a comment above the fence must not be read as a frontmatter block")
	}
}
