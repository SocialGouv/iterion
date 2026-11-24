package bundle

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
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

// ReadFrontmatter reads the file at path and returns its parsed frontmatter
// (ParseFrontmatter): nil and no error when it carries no `## ---` block,
// an error when the file cannot be read or its block is not read.
func ReadFrontmatter(path string) (*Frontmatter, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- a bot file the caller found
	if err != nil {
		return nil, err
	}
	return ParseFrontmatter(path, raw)
}

// ParseFrontmatter reads a `.bot` text, named name, as the parser does and
// takes the `## ---` … `## ---` block that opens its head
// (parser.Frontmatter): the head is what precedes the first declaration, so
// blank lines, the `dsl:` header and `import` lines may come before the
// fence, and a comment that is not the fence may not; a BOM or CRLF endings
// read as the lexer reads them. The inner lines are decoded through
// workflowfile.DecodeFrontmatter. It returns nil and no error when there is
// no such block, when it is not closed, or when its lines are not a YAML
// mapping — a block no reader of a catalog identity reads: the author writer
// takes the same block off the same head, so the `catalog:` of a document is
// what the catalogue reads off its `.bot`, and `fmt`, which writes the
// head's comments at the top, never changes an identity.
//
// A block a bound of its readers kept from being read is an error, never
// "no block": the text past the size cap or the read budget
// (*parser.ReadRefusal — the parser reads the whole text, so a body past
// the budget keeps the head from being read too), or a block whose YAML
// could open more values than the budget (*workflowfile.YAMLDecodeRefusal).
// The identity is there, and the budget is the operator's dial: a reader
// that took "not read" for "none" would strip a bot of its name, its
// triggers and its capabilities in silence.
//
// The fence and the inner lines are comment lines of the workflow source, so
// they follow the lexer's definition of a comment (workflowfile.CommentText):
// `# ---` and `## ---` are the same fence, `# name: x` and `## name: x` the
// same line. A reader that only knew `##` would silently drop the whole
// catalogue identity of a file the parser accepts.
func ParseFrontmatter(name string, raw []byte) (*Frontmatter, error) {
	fm, _, _, err := FrontmatterParse(name, raw)
	return fm, err
}

// FrontmatterParse is ParseFrontmatter with the parse kept: the block's
// frontmatter (nil when there is none), the parsed AST, and what the
// reading spent of the read budget — what a caller that goes on to load
// the unit hands unit.LoadDirWithMainAST, so one parse serves both
// readers of a bot file (discovery's catalogue identity and the schema
// load). err is ParseFrontmatter's own: a read refusal, or nil with a nil
// frontmatter when the file carries no block a reader reads.
func FrontmatterParse(name string, raw []byte) (*Frontmatter, *ast.File, int, error) {
	pr, err := parser.Parse(name, string(raw))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("the frontmatter is not read: %w", err)
	}
	block := parser.Frontmatter(pr.File)
	if !block.Found || !block.Closed {
		return nil, pr.File, pr.Spent, nil
	}
	fm, _, derr := workflowfile.DecodeFrontmatter(strings.Join(block.Lines, "\n"), parser.ReadBudget())
	var refusal *workflowfile.YAMLDecodeRefusal
	if errors.As(derr, &refusal) {
		return nil, pr.File, pr.Spent, fmt.Errorf("the frontmatter is not read: %w — %s", derr, parser.ReadBudgetHint)
	}
	if derr != nil {
		return nil, pr.File, pr.Spent, nil
	}
	return fm, pr.File, pr.Spent, nil
}
