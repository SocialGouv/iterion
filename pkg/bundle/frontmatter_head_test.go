package bundle

import (
	"strings"
	"testing"
)

// The catalogue reads the frontmatter off the file's head as the parser
// files it — the reading the author writer shares — so a block the writer
// gives a document as `catalog:` is a block the catalogue reads: below the
// `dsl:` header, below an import, glued to the first declaration, behind a
// BOM or CRLF endings. Prose or the strict-escape directive before the fence
// is no frontmatter, for both.
func TestParseFrontmatterReadsTheHeadAsTheParserDoes(t *testing.T) {
	body := "\nworkflow w:\n  entry: a\n"
	block := "## ---\n## name: Alpha Bot\n## triggers: [nightly]\n## ---\n"
	for name, tc := range map[string]struct {
		src  string
		want string // the name read, "" for no identity
	}{
		"at the top":                     {block + body, "Alpha Bot"},
		"below the dsl header":           {"dsl: 2\n\n" + block + body, "Alpha Bot"},
		"below an import":                {"dsl: 2\nimport \"lib/x.bot\"\n\n" + block + body, "Alpha Bot"},
		"glued to the first declaration": {block + "workflow w:\n  entry: a\n", "Alpha Bot"},
		"behind a BOM":                   {"\ufeff" + block + body, "Alpha Bot"},
		"CRLF endings":                   {strings.ReplaceAll(block+body, "\n", "\r\n"), "Alpha Bot"},
		"prose before the fence":         {"## a note\n" + block + body, ""},
		"the directive before the fence": {"## strict-escape: on\n" + block + body, ""},
		"not closed":                     {"## ---\n## name: Alpha Bot\n" + body, ""},
	} {
		t.Run(name, func(t *testing.T) {
			fm := ParseFrontmatter([]byte(tc.src))
			got := ""
			if fm != nil {
				got = fm.Name
			}
			if got != tc.want {
				t.Fatalf("name read %q, want %q\n%q", got, tc.want, tc.src)
			}
			if fm != nil && (len(fm.Triggers) != 1 || fm.Triggers[0] != "nightly") {
				t.Fatalf("triggers = %v, want [nightly]", fm.Triggers)
			}
		})
	}
}
