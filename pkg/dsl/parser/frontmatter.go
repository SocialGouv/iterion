package parser

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// FrontmatterBlock is the `## ---` … `## ---` block among a parsed file's
// head comments, as Frontmatter reads it.
type FrontmatterBlock struct {
	// Lines are the block's inner lines, hashes off (the comment texts); a
	// blank line an author left inside the block is one empty line, as the
	// catalogue has always read it.
	Lines []string
	// Found reports that the head's first comment is the opening fence;
	// Closed that a second fence closes the block.
	Found, Closed bool
	// Span is the number of head comments the block takes, the closing
	// fence included — what a reader of the comments beyond the block
	// skips. Without a closing fence the block takes the whole head.
	Span int
}

// Frontmatter reads the frontmatter block off a parsed file's head
// comments: the `---` fence that opens the head, through the closing one.
// The head is what the parser files before the first declaration — blank
// lines, the `dsl:` header and `import` lines do not end it (comments.go),
// so a block written below the header is the block the canonical form
// writes at the top — and a comment that is not the fence closes the
// question: a file whose head opens with prose, or with the strict-escape
// directive, carries no frontmatter.
//
// It is the ONE extraction every reader of a catalog identity uses —
// bundle.ParseFrontmatter for the catalogue, the author writer for a
// document's `catalog:`, `fmt --to yaml` for its notice — so a block one of
// them reads, all of them read; and since the parser reads the file's bytes
// as the lexer does, a BOM or CRLF endings change nothing. The lines are
// decoded by workflowfile.DecodeFrontmatter, the one reading.
func Frontmatter(f *ast.File) FrontmatterBlock {
	var b FrontmatterBlock
	if f == nil || len(f.Comments) == 0 || strings.TrimSpace(f.Comments[0].Text) != workflowfile.FrontmatterFence {
		return b
	}
	b.Found = true
	for i, c := range f.Comments[1:] {
		if c.Blank {
			b.Lines = append(b.Lines, "")
		}
		if strings.TrimSpace(c.Text) == workflowfile.FrontmatterFence {
			b.Closed, b.Span = true, i+2
			return b
		}
		b.Lines = append(b.Lines, c.Text)
	}
	b.Span = len(f.Comments)
	return b
}
