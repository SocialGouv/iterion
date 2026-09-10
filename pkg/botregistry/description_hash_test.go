package botregistry

import "testing"

// The description reader follows the lexer's definition of a comment line
// (`#` or `##`) and of the frontmatter fence (`# ---` or `## ---`): a bot
// whose leading comments use a single hash keeps its catalogue description,
// and a single-hash frontmatter block is skipped like a double-hash one.
func TestLeadingCommentDescription_SingleHashComments(t *testing.T) {
	raw := []byte("# ---\n# name: Alpha\n# ---\n# A simple bot.\n## Does one thing.\n\nagent a:\n")
	if got := leadingCommentDescription(raw, "simple.bot"); got != "A simple bot. Does one thing." {
		t.Fatalf("description = %q, want the two comment lines joined", got)
	}
}

// The description is the LEADING comment paragraph, before the first line of
// code. A file with no header comment has no description: a `# shell comment`
// inside a `command: |` block, or a prompt body's `# Heading` (text for the
// lexer), must never be published as the bot's catalogue description.
func TestLeadingCommentDescription_StopsAtTheFirstLineOfCode(t *testing.T) {
	cases := map[string]string{
		"shell comment in a block scalar": "tool t:\n  command: |\n    # shell comment\n    echo hi\nworkflow w:\n  entry: t\n  t -> done\n",
		"heading in a prompt body":        "prompt p:\n  # Internal reviewer notes — do not ship\n  You are a reviewer.\n",
		"comment after the first decl":    "agent a:\n  model: \"m\"\n\n# Not a description: it follows code.\n",
	}
	for name, raw := range cases {
		if got := leadingCommentDescription([]byte(raw), "x.bot"); got != "" {
			t.Errorf("%s: description = %q, want none", name, got)
		}
	}
	// A blank line between the frontmatter and the paragraph is still fine.
	raw := "## ---\n## name: Alpha\n## ---\n\n## A simple bot.\n\nagent a:\n"
	if got := leadingCommentDescription([]byte(raw), "x.bot"); got != "A simple bot." {
		t.Errorf("description = %q, want the paragraph after the frontmatter", got)
	}
	// A BOM is not a line of code (the lexer strips it too), and CR / CRLF
	// line endings terminate lines — neither may cost the description or
	// leak the code that follows it.
	for name, raw := range map[string]string{
		"BOM":     "\ufeff## A simple bot.\n## Does one thing.\n\nagent a:\n",
		"CRLF":    "## A simple bot.\r\n## Does one thing.\r\n\r\nagent a:\r\n",
		"lone CR": "## A simple bot.\r## Does one thing.\ragent a:\r",
	} {
		if got := leadingCommentDescription([]byte(raw), "x.bot"); got != "A simple bot. Does one thing." {
			t.Errorf("%s: description = %q, want the two comment lines joined", name, got)
		}
	}
}
