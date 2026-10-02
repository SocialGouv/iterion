package author

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// The `catalog:` the writer gives a document is what the catalogue reads off
// the .bot's frontmatter — the same block (parser.Frontmatter) through the
// same reading (workflowfile.DecodeFrontmatter) — so the two never disagree
// on a file: a block below the `dsl:` header or an import, glued to the
// first declaration, behind a BOM; a key named twice keeps its last value in
// both; a block the reading does not read (a scalar where a list is
// expected) gives the .bot no identity and the document no `catalog:`; a
// block with none of the four keys the same. And read back through the
// unparser — the `fmt --to bot` path — the .bot the document writes carries
// the identity the original did.
func TestTheDocumentsCatalogIsWhatTheCatalogReaderReads(t *testing.T) {
	body := "\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n"
	for name, head := range map[string]string{
		"a key named twice":              "## ---\n## name: first\n## name: last\n## ---\n",
		"a scalar triggers":              "## ---\n## name: probe\n## triggers: just-one\n## ---\n",
		"none of the four":               "## ---\n## owner: jo\n## ---\n",
		"single-hash fences":             "# ---\n# name: probe\n# capabilities: [board.read]\n# ---\n",
		"an unclosed block":              "## ---\n## name: probe\n",
		"no block":                       "",
		"a nested unknown key":           "## ---\n## name: probe\n## nested:\n##   deep:\n##     key: 1\n## ---\n",
		"below the dsl header":           "dsl: 2\n\n## ---\n## name: probe\n## triggers: [a]\n## ---\n",
		"below an import":                "dsl: 2\nimport \"lib/x.bot\"\n\n## ---\n## name: probe\n## ---\n",
		"glued to the first declaration": "## ---\n## name: probe\n## ---",
		"behind a BOM":                   "\ufeff## ---\n## name: probe\n## ---\n",
		"a blank line inside the block":  "## ---\n## name: probe\n\n## capabilities: [x]\n## ---\n",
		"prose before the fence":         "## a note\n## ---\n## name: probe\n## ---\n",
		"a blank line inside, glued":     "## ---\n## name: probe\n\n## capabilities: [x]\n## ---",
		"the directive after the block":  "## ---\n## name: probe\n## ---\n## strict-escape: on\n",
		"a comment on the header's line": "dsl: 2 ## a note\n## ---\n## name: probe\n## ---\n",
	} {
		t.Run(name, func(t *testing.T) {
			src := head + body
			pr := parser.Parse("x.bot", src)
			if hasParseErrors(pr.Diagnostics) {
				t.Fatalf("the .bot does not parse: %v", pr.Diagnostics)
			}
			out, err := Write(pr.File)
			if err != nil {
				t.Fatal(err)
			}
			want := bundle.ParseFrontmatter([]byte(src))
			if has := strings.Contains(string(out), "\ncatalog:\n"); has != !want.Empty() {
				t.Fatalf("the document carries a catalog: %v, while the catalogue reads %s\n%s", has, identityOf(want), out)
			}
			res := Parse("x.bot.yaml", out)
			if errs := errorsOf(res); len(errs) > 0 {
				t.Fatalf("the document does not read: %v\n%s", errs, out)
			}
			if got := bundle.ParseFrontmatter([]byte(unparse.Unparse(res.File))); identityOf(got) != identityOf(want) {
				t.Fatalf("the .bot the document writes back carries %s, the original %s\n%s", identityOf(got), identityOf(want), out)
			}
		})
	}
}

// identityOf renders a catalog identity for a comparison, an empty one as
// none: a .bot with no block and one whose block says nothing the catalogue
// uses are the same to it.
func identityOf(fm *bundle.Frontmatter) string {
	if fm.Empty() {
		return "(none)"
	}
	return fmt.Sprintf("name=%q description=%q triggers=%q capabilities=%q", fm.Name, fm.Description, fm.Triggers, fm.Capabilities)
}
