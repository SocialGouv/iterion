package parser

import (
	"reflect"
	"testing"
)

// The frontmatter is the fence block that opens the file's head, wherever
// the head starts: at the top, below the `dsl:` header, below an import,
// glued to the first declaration or a blank line away from it; a blank line
// inside the block is kept as one; prose or a directive before the fence
// means no frontmatter; a block with no closing fence is found and not
// closed, and takes the whole head.
func TestFrontmatterIsReadOffTheHeadWhereverTheHeadStarts(t *testing.T) {
	body := "\nschema card:\n  name: string\n"
	for name, tc := range map[string]struct {
		src           string
		found, closed bool
		lines         []string
		span          int
	}{
		"at the top":                     {"## ---\n## name: a\n## ---\n" + body, true, true, []string{"name: a"}, 3},
		"below the dsl header":           {"dsl: 2\n\n## ---\n## name: a\n## ---\n" + body, true, true, []string{"name: a"}, 3},
		"below an import":                {"dsl: 2\nimport \"lib/x.bot\"\n\n## ---\n## name: a\n## ---\n" + body, true, true, []string{"name: a"}, 3},
		"a blank line inside":            {"## ---\n## name: a\n\n## description: d\n## ---\n" + body, true, true, []string{"name: a", "", "description: d"}, 4},
		"glued to the first declaration": {"## ---\n## name: a\n## ---\nschema card:\n  name: string\n", true, true, []string{"name: a"}, 3},
		"single-hash fences":             {"# ---\n# name: a\n# ---\n" + body, true, true, []string{"name: a"}, 3},
		"a blank line inside, glued":     {"## ---\n## name: a\n\n## description: d\n## ---\nschema card:\n  name: string\n", true, true, []string{"name: a", "", "description: d"}, 4},
		"the directive after the block":  {"## ---\n## name: a\n## ---\n## strict-escape: on\n" + body, true, true, []string{"name: a"}, 3},
		"a comment on the header's line": {"dsl: 2 ## a note\n## ---\n## name: a\n## ---\n" + body, false, false, nil, 0},
		"prose before the fence":         {"## prose\n## ---\n## name: a\n## ---\n" + body, false, false, nil, 0},
		"the directive before the fence": {"## strict-escape: on\n## ---\n## name: a\n## ---\n" + body, false, false, nil, 0},
		"not closed":                     {"## ---\n## name: a\n" + body, true, false, []string{"name: a"}, 2},
		"no comment at all":              {"dsl: 2\n" + body, false, false, nil, 0},
	} {
		t.Run(name, func(t *testing.T) {
			pr := Parse("x.bot", tc.src)
			got := Frontmatter(pr.File)
			if got.Found != tc.found || got.Closed != tc.closed || got.Span != tc.span || !reflect.DeepEqual(got.Lines, tc.lines) {
				t.Fatalf("got %+v, want found=%v closed=%v span=%d lines=%q\n%s", got, tc.found, tc.closed, tc.span, tc.lines, tc.src)
			}
		})
	}
}
