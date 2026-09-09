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
