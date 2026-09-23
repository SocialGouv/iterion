package bundle

import (
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// Frontmatter is the optional `## ---` … `## ---` YAML block at the head of
// a main.bot. It lets a loose .bot file or a bundle carry catalog metadata
// (name / description / triggers / capabilities) inline. For a bundle the
// manifest is authoritative; a non-empty frontmatter value OVERRIDES the
// manifest's triggers/capabilities at discovery time
// (botregistry.parseBundle). bundlelint flags that silent override (C221).
// The type and its one reading live in workflowfile (DecodeFrontmatter),
// where the author writer reads the same block the same way.
type Frontmatter = workflowfile.Frontmatter

// ReadFrontmatter reads the file at path and returns its parsed frontmatter,
// or nil when the file is unreadable or carries no `## ---` block.
func ReadFrontmatter(path string) *Frontmatter {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return ParseFrontmatter(raw)
}

// ParseFrontmatter reads a `.bot` text as the parser does and takes the
// `## ---` … `## ---` block that opens its head (parser.Frontmatter): the
// head is what precedes the first declaration, so blank lines, the `dsl:`
// header and `import` lines may come before the fence, and a comment that
// is not the fence may not; a BOM or CRLF endings read as the lexer reads
// them. The inner lines are decoded through workflowfile.DecodeFrontmatter.
// Returns nil when there is no such block, when it is not closed, or when
// the reading does not read it — a block no reader of a catalog identity
// reads: the author writer takes the same block off the same head, so the
// `catalog:` of a document is what the catalogue reads off its `.bot`, and
// `fmt`, which writes the head's comments at the top, never changes an
// identity.
//
// The fence and the inner lines are comment lines of the workflow source, so
// they follow the lexer's definition of a comment (workflowfile.CommentText):
// `# ---` and `## ---` are the same fence, `# name: x` and `## name: x` the
// same line. A reader that only knew `##` would silently drop the whole
// catalogue identity of a file the parser accepts.
func ParseFrontmatter(raw []byte) *Frontmatter {
	block := parser.Frontmatter(parser.Parse("main.bot", string(raw)).File)
	if !block.Found || !block.Closed {
		return nil
	}
	fm, _, err := workflowfile.DecodeFrontmatter(strings.Join(block.Lines, "\n"))
	if err != nil {
		return nil
	}
	return fm
}
